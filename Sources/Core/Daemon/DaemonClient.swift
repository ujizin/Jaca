import Foundation
import NIOCore
import NIOPosix

/// One event as received: its topic and the raw line, decoded on demand by the reader that
/// knows its payload type.
struct DaemonEventLine: Sendable {
    let topic: String
    let line: Data

    func decode<T: Codable>(_ type: T.Type) throws -> T {
        try JSONDecoder.daemon.decode(RPCEventEnvelope<T>.self, from: line).params.data
    }
}

/// A connection to `jacad`. Requests are correlated by id, so any number can be in flight.
/// Events arrive on `AsyncStream`s from `subscribe(_:)`. When the socket closes, every
/// pending call fails with `RPCError.disconnected`, every stream finishes, and `onClose` runs.
final class DaemonClient: @unchecked Sendable {
    private struct Subscriber {
        let topics: Set<String>
        let continuation: AsyncStream<DaemonEventLine>.Continuation
    }

    private let lock = NSLock()
    private var peer: DaemonPeer?
    private var nextID = 1
    private var pending: [Int: CheckedContinuation<Data, Error>] = [:]
    private var subscribers: [UUID: Subscriber] = [:]
    private var closed = false
    private var closeHandlers: [@Sendable () -> Void] = []

    private init() {}

    /// Connects to the socket. Throws if nothing is listening.
    static func connect(paths: DaemonPaths = .default) async throws -> DaemonClient {
        let client = DaemonClient()
        let bootstrap = ClientBootstrap(group: DaemonTransport.group)
            .channelOption(ChannelOptions.writeBufferWaterMark, value: DaemonTransport.waterMark)
            .channelInitializer { [weak client] channel in
                channel.eventLoop.makeCompletedFuture {
                    try channel.pipeline.syncOperations.addHandlers([
                        ByteToMessageHandler(LineFrameDecoder()),
                        DaemonLineHandler(onLine: { [weak client] in client?.received($0) },
                                          onClose: { [weak client] in client?.connectionClosed() }),
                    ])
                }
            }
        let channel = try await bootstrap.connect(unixDomainSocketPath: paths.socket.path).get()
        client.lock.withLock { client.peer = DaemonPeer(id: 0, channel: channel) }
        return client
    }

    var isClosed: Bool {
        lock.lock(); defer { lock.unlock() }
        return closed
    }

    /// Runs `handler` once when the connection closes (immediately if it already has).
    func onClose(_ handler: @escaping @Sendable () -> Void) {
        lock.lock()
        if closed { lock.unlock(); handler(); return }
        closeHandlers.append(handler)
        lock.unlock()
    }

    func close() {
        lock.lock(); let peer = self.peer; lock.unlock()
        peer?.close()
    }

    // MARK: - Calls

    /// Calls `method` and decodes its `result` as `R`. Throws the daemon's `RPCError` on an
    /// error response.
    func call<P: Encodable, R: Decodable>(_ method: String, _ params: P, as type: R.Type = R.self) async throws -> R {
        let line = try await send(method: method) { id in
            try DaemonLine.encode(RPCRequestEnvelope(id: .number(id), method: method, params: params))
        }
        do {
            return try JSONDecoder.daemon.decode(RPCResultOnly<R>.self, from: line).result
        } catch {
            throw RPCError.failed("Bad result for \(method): \(RPCError.describing(error))")
        }
    }

    func call<R: Decodable>(_ method: String, as type: R.Type = R.self) async throws -> R {
        try await call(method, RPCEmpty(), as: type)
    }

    /// Sends raw params JSON (already an object, array or null) and returns the raw response
    /// line. Used by `jacad call`, which doesn't know payload types.
    func callRaw(_ method: String, paramsJSON: Data?) async throws -> Data {
        try await send(method: method) { id in
            var line = Data("{\"id\":\(id),\"jsonrpc\":\"2.0\",\"method\":".utf8)
            line.append(try JSONEncoder.daemon.encode(method))
            if let paramsJSON {
                line.append(Data(",\"params\":".utf8))
                line.append(paramsJSON)
            }
            line.append(Data("}\n".utf8))
            return line
        }
    }

    private func send(method: String, encode: (Int) throws -> Data) async throws -> Data {
        let next: (Int, DaemonPeer)? = lock.withLock {
            guard !closed, let peer else { return nil }
            defer { nextID += 1 }
            return (nextID, peer)
        }
        guard let (id, peer) = next else { throw RPCError.disconnected }
        let line = try encode(id)

        let response: Data = try await withCheckedThrowingContinuation { continuation in
            lock.lock()
            if closed {
                lock.unlock()
                continuation.resume(throwing: RPCError.disconnected)
                return
            }
            pending[id] = continuation
            lock.unlock()
            peer.send(line)
        }
        if let error = (try? JSONDecoder.daemon.decode(RPCHeader.self, from: response))?.error {
            throw error
        }
        return response
    }

    // MARK: - Events

    /// Subscribes to `topics` and returns their events. The stream stays open until the
    /// consumer stops iterating or the connection closes; then its topics are unsubscribed
    /// (unless another stream on this client still wants them).
    func subscribe(_ topics: [String]) async throws -> AsyncStream<DaemonEventLine> {
        let key = UUID()
        let (stream, continuation) = AsyncStream<DaemonEventLine>.makeStream(bufferingPolicy: .unbounded)
        let isClosed = lock.withLock {
            if !closed { subscribers[key] = Subscriber(topics: Set(topics), continuation: continuation) }
            return closed
        }
        if isClosed { continuation.finish(); throw RPCError.disconnected }

        continuation.onTermination = { [weak self] _ in self?.dropSubscriber(key) }
        // Registered before the request so a retained event replayed by the daemon isn't missed.
        do {
            let _: RPCEmpty = try await call("events.subscribe", DaemonServer.TopicsParams(topics: topics))
        } catch {
            continuation.finish()
            throw error
        }
        return stream
    }

    private func dropSubscriber(_ key: UUID) {
        lock.lock()
        guard let removed = subscribers.removeValue(forKey: key), !closed else { lock.unlock(); return }
        let stillWanted = subscribers.values.reduce(into: Set<String>()) { $0.formUnion($1.topics) }
        lock.unlock()
        let orphaned = removed.topics.subtracting(stillWanted)
        guard !orphaned.isEmpty else { return }
        Task { [weak self] in
            let _: RPCEmpty? = try? await self?.call("events.unsubscribe", DaemonServer.TopicsParams(topics: Array(orphaned)))
        }
    }

    // MARK: - Inbound

    private func received(_ line: Data) {
        guard let header = try? JSONDecoder.daemon.decode(RPCHeader.self, from: line) else { return }
        if header.method == "event", let topic = header.params?.topic {
            deliver(DaemonEventLine(topic: topic, line: line))
            return
        }
        guard case .number(let id)? = header.id else { return }
        lock.lock()
        let continuation = pending.removeValue(forKey: id)
        lock.unlock()
        continuation?.resume(returning: line)
    }

    private func deliver(_ event: DaemonEventLine) {
        // `events.dropped` goes to whoever subscribed to the topic it describes.
        let target = event.topic == "events.dropped"
            ? ((try? event.decode(DroppedEvents.self))?.topic ?? event.topic)
            : event.topic
        lock.lock()
        let matching = subscribers.values.filter { $0.topics.contains(target) }.map(\.continuation)
        lock.unlock()
        matching.forEach { $0.yield(event) }
    }

    private func connectionClosed() {
        lock.lock()
        closed = true
        let waiting = pending
        pending = [:]
        let streams = subscribers.values.map(\.continuation)
        subscribers = [:]
        let handlers = closeHandlers
        closeHandlers = []
        lock.unlock()
        waiting.values.forEach { $0.resume(throwing: RPCError.disconnected) }
        streams.forEach { $0.finish() }
        handlers.forEach { $0() }
    }
}

// MARK: - Launching

/// Connects to the running daemon, starting it first if nothing is listening.
enum DaemonLauncher {
    enum LaunchError: Error, LocalizedError {
        case executableMissing(String)
        case didNotStart(String)

        var errorDescription: String? {
            switch self {
            case .executableMissing(let p): return "jacad not found at \(p)"
            case .didNotStart(let log): return "jacad did not start; see \(log)"
            }
        }
    }

    /// The `jacad` binary shipped next to the current executable: `Jaca.app/Contents/MacOS/jacad`
    /// for the app (and the tests it hosts), and `jacad` itself inside `jacad`.
    static var bundledExecutable: URL? {
        Bundle.main.executableURL?.deletingLastPathComponent().appendingPathComponent("jacad")
    }

    /// Connects, spawning `executable serve` if the socket isn't answering. Waits up to
    /// `timeout` for a spawned daemon to bind.
    static func connect(paths: DaemonPaths = .default,
                        executable: URL? = bundledExecutable,
                        spawn: Bool = true,
                        timeout: Duration = .seconds(5)) async throws -> DaemonClient {
        if let client = try? await DaemonClient.connect(paths: paths) { return client }
        guard spawn else { return try await DaemonClient.connect(paths: paths) }
        guard let executable, FileManager.default.isExecutableFile(atPath: executable.path) else {
            throw LaunchError.executableMissing(executable?.path ?? "(unknown)")
        }
        try launch(executable, paths: paths)
        let deadline = ContinuousClock.now + timeout
        while ContinuousClock.now < deadline {
            try? await Task.sleep(for: .milliseconds(100))
            if let client = try? await DaemonClient.connect(paths: paths) { return client }
        }
        throw LaunchError.didNotStart(paths.log.path)
    }

    /// Starts `jacad serve` detached from the caller, logging to `paths.log`.
    static func launch(_ executable: URL, paths: DaemonPaths) throws {
        try FileManager.default.createDirectory(at: paths.directory, withIntermediateDirectories: true)
        if !FileManager.default.fileExists(atPath: paths.log.path) {
            FileManager.default.createFile(atPath: paths.log.path, contents: nil)
        }
        let log = try FileHandle(forWritingTo: paths.log)
        log.seekToEndOfFile()
        let process = Process()
        process.executableURL = executable
        process.arguments = ["serve"]
        var env = ProcessInfo.processInfo.environment
        env["JACA_DAEMON_DIR"] = paths.directory.path
        process.environment = env
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = log
        process.standardError = log
        try process.run()
        DaemonLog.info("spawned \(executable.path) serve (pid \(process.processIdentifier))")
    }
}
