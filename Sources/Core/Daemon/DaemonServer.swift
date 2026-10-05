import Foundation
import NIOCore
import NIOPosix

/// The `jacad` socket server: accepts connections on `DaemonPaths.socket`, routes requests
/// through `DaemonRouter`, fans events out through `DaemonEventBus`, and shuts itself down
/// after an idle period with no clients and no busy area.
///
/// One daemon per user: `start()` takes an exclusive `flock` on `jacad.lock` for the life of
/// the process, so a second daemon fails with `alreadyRunning` instead of stealing the socket.
final class DaemonServer: @unchecked Sendable {
    enum StartError: Error, LocalizedError {
        case alreadyRunning
        case socketPathTooLong(String)
        case io(String)

        var errorDescription: String? {
            switch self {
            case .alreadyRunning: return "jacad is already running"
            case .socketPathTooLong(let p): return "socket path is too long for a Unix socket: \(p)"
            case .io(let m): return m
            }
        }
    }

    struct Status: Codable, Sendable, Equatable {
        var pid: Int32
        var protocolVersion: Int
        var buildID: String
        var startedAt: Date
        var connections: Int
        var subscriptions: Int
        var busy: [String]
    }

    struct HelloParams: Codable, Sendable {
        var protocolVersion: Int
        var client: String?
    }

    struct HelloResult: Codable, Sendable, Equatable {
        var protocolVersion: Int
        var buildID: String
        var pid: Int32
    }

    struct TopicsParams: Codable, Sendable {
        var topics: [String]
    }

    struct Catalog: Codable, Sendable, Equatable {
        var protocolVersion: Int
        var methods: [DaemonRouter.MethodInfo]
        var topics: [DaemonRouter.TopicInfo]
    }

    let paths: DaemonPaths
    let router = DaemonRouter()
    let bus = DaemonEventBus()
    let startedAt = Date()

    /// Called on the main actor when the server stops, from `daemon.shutdown` or idle.
    var onStop: (@MainActor () -> Void)?

    /// Seconds with no connections and nothing busy before the daemon exits. 0 disables it.
    var idleTimeout: TimeInterval

    private let lock = NSLock()
    private var channel: Channel?
    private var lockFD: Int32 = -1
    private var nextPeerID = 1
    private var connectionCount = 0
    private var idleSince = Date()
    private var busyChecks: [(name: String, check: @MainActor () -> Bool)] = []
    private var idleTask: Task<Void, Never>?
    private var stopped = false
    private var kept: [AnyObject] = []

    init(paths: DaemonPaths = .default, idleTimeout: TimeInterval = 300) {
        self.paths = paths
        self.idleTimeout = idleTimeout
        registerBuiltins()
    }

    /// Reports an area as busy (a running log stream, an open capture) so the daemon does not
    /// idle out under it. Checked on the main actor.
    func addBusyCheck(_ name: String, _ check: @escaping @MainActor () -> Bool) {
        lock.lock(); busyChecks.append((name, check)); lock.unlock()
    }

    /// Keeps an area object (a polled topic, a session registry) alive as long as the server.
    func keep(_ object: AnyObject) {
        lock.lock(); kept.append(object); lock.unlock()
    }

    // MARK: - Lifecycle

    func start() throws {
        guard paths.socketPathFits else { throw StartError.socketPathTooLong(paths.socket.path) }
        do {
            try FileManager.default.createDirectory(at: paths.directory, withIntermediateDirectories: true)
        } catch {
            throw StartError.io("can't create \(paths.directory.path): \(error.localizedDescription)")
        }
        try acquireLock()
        // We hold the lock, so any socket file left behind belongs to a dead daemon.
        try? FileManager.default.removeItem(at: paths.socket)

        let bootstrap = ServerBootstrap(group: DaemonTransport.group)
            .serverChannelOption(ChannelOptions.backlog, value: 64)
            .childChannelOption(ChannelOptions.writeBufferWaterMark, value: DaemonTransport.waterMark)
            .childChannelInitializer { [weak self] channel in
                guard let self else { return channel.close() }
                return self.configure(channel)
            }
        do {
            let ch = try bootstrap.bind(unixDomainSocketPath: paths.socket.path).wait()
            lock.lock(); channel = ch; lock.unlock()
        } catch {
            releaseLock()
            throw StartError.io("can't bind \(paths.socket.path): \(error.localizedDescription)")
        }
        chmod(paths.socket.path, 0o600)
        DaemonLog.info("listening on \(paths.socket.path) (pid \(getpid()), protocol \(DaemonProtocol.version))")
        startIdleMonitor()
    }

    func stop() { stop(onlyIfIdle: false) }

    /// `onlyIfIdle` re-checks for connections under the same lock that marks the server stopped,
    /// so a client accepted between the idle check and the stop isn't dropped.
    private func stop(onlyIfIdle: Bool) {
        lock.lock()
        guard !stopped, !onlyIfIdle || connectionCount == 0 else { lock.unlock(); return }
        stopped = true
        let ch = channel
        channel = nil
        idleTask?.cancel()
        lock.unlock()
        try? ch?.close().wait()
        // Closing the listener leaves accepted connections open; close them so clients see
        // the disconnect instead of waiting on a daemon that is gone.
        bus.allPeers.forEach { $0.close() }
        try? FileManager.default.removeItem(at: paths.socket)
        releaseLock()
        DaemonLog.info("stopped")
        let onStop = self.onStop
        Task { @MainActor in onStop?() }
    }

    private func acquireLock() throws {
        let fd = open(paths.lock.path, O_CREAT | O_RDWR, 0o600)
        guard fd >= 0 else { throw StartError.io("can't open \(paths.lock.path): errno \(errno)") }
        guard flock(fd, LOCK_EX | LOCK_NB) == 0 else {
            close(fd)
            throw StartError.alreadyRunning
        }
        ftruncate(fd, 0)
        let pid = "\(getpid())\n"
        _ = pid.withCString { write(fd, $0, strlen($0)) }
        lock.lock(); lockFD = fd; lock.unlock()
    }

    private func releaseLock() {
        lock.lock()
        let fd = lockFD
        lockFD = -1
        lock.unlock()
        guard fd >= 0 else { return }
        flock(fd, LOCK_UN)
        close(fd)
    }

    // MARK: - Connections

    private func configure(_ channel: Channel) -> EventLoopFuture<Void> {
        lock.lock()
        // Stopping: refuse rather than accept a connection that is about to be cut.
        guard !stopped else {
            lock.unlock()
            return channel.close()
        }
        let id = nextPeerID
        nextPeerID += 1
        connectionCount += 1
        lock.unlock()

        let peer = DaemonPeer(id: id, channel: channel)
        bus.add(peer)
        let ctx = DaemonRequestContext(peer: peer, bus: bus)
        let router = self.router
        let ordered = SerialChain()
        let handler = DaemonLineHandler(
            onLine: { line in
                let respond: @Sendable () async -> Void = {
                    if let response = await router.handle(line: line, ctx: ctx) { peer.send(response) }
                }
                if router.isConcurrent(line: line) {
                    Task { await respond() }
                } else {
                    ordered.enqueue(respond)
                }
            },
            onClose: { [weak self] in self?.peerClosed(id) }
        )
        return channel.eventLoop.makeCompletedFuture {
            try channel.pipeline.syncOperations.addHandlers([ByteToMessageHandler(LineFrameDecoder()), handler])
        }
    }

    private func peerClosed(_ id: Int) {
        bus.remove(peerID: id)
        lock.lock()
        connectionCount -= 1
        if connectionCount == 0 { idleSince = Date() }
        lock.unlock()
    }

    // MARK: - Idle shutdown

    private func startIdleMonitor() {
        guard idleTimeout > 0 else { return }
        let interval = min(15, max(1, idleTimeout / 4))
        idleTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(interval))
                guard let self, !Task.isCancelled else { return }
                if await self.shouldIdleOut() {
                    DaemonLog.info("idle for \(Int(self.idleTimeout))s with no clients; exiting")
                    self.stop(onlyIfIdle: true)
                    if self.lock.withLock({ self.stopped }) { return }
                }
            }
        }
    }

    private func shouldIdleOut() async -> Bool {
        let (connections, since) = lock.withLock { (connectionCount, idleSince) }
        guard connections == 0, Date().timeIntervalSince(since) >= idleTimeout else { return false }
        return await busyAreas().isEmpty
    }

    @MainActor
    private func busyAreas() -> [String] {
        lock.lock()
        let checks = busyChecks
        lock.unlock()
        return checks.filter { $0.check() }.map(\.name)
    }

    // MARK: - Built-in methods

    private func registerBuiltins() {
        router.register("hello", "Handshake. Call first on every connection.", params: HelloParams.self) { p, _ in
            guard p.protocolVersion == DaemonProtocol.version else {
                throw RPCError(code: RPCError.versionMismatchCode,
                               message: "protocol \(p.protocolVersion) requested, daemon speaks \(DaemonProtocol.version)")
            }
            return HelloResult(protocolVersion: DaemonProtocol.version, buildID: DaemonBuild.currentID, pid: getpid())
        }
        router.register("ping", "Liveness check. Returns \"pong\".") { (_: RPCEmpty, _) in "pong" }
        router.register("api.describe", "Lists every method and event topic.") { [weak self] (_: RPCEmpty, _) in
            Catalog(protocolVersion: DaemonProtocol.version,
                    methods: self?.router.methods ?? [],
                    topics: self?.router.topics ?? [])
        }
        router.register("events.subscribe", "Subscribes this connection to event topics. Retained topics replay their last event.",
                        params: TopicsParams.self) { p, ctx in
            ctx.bus.subscribe(peerID: ctx.peer.id, topics: p.topics)
            return RPCEmpty()
        }
        router.register("events.unsubscribe", "Unsubscribes this connection from event topics.",
                        params: TopicsParams.self) { p, ctx in
            ctx.bus.unsubscribe(peerID: ctx.peer.id, topics: p.topics)
            return RPCEmpty()
        }
        router.register("daemon.status", "Process, connection and busy-area status.") { [weak self] (_: RPCEmpty, _) in
            guard let self else { throw RPCError.failed("stopping") }
            return await self.status()
        }
        router.register("daemon.shutdown", "Stops the daemon after replying.") { [weak self] (_: RPCEmpty, _) in
            Task { [weak self] in
                try? await Task.sleep(for: .milliseconds(100))
                self?.stop()
            }
            return RPCEmpty()
        }
        router.describeTopic("events.dropped", "Sent before a delivered event when earlier events on that topic were skipped because this connection was slow. Data: {topic, count}.")
    }

    func status() async -> Status {
        let connections = lock.withLock { connectionCount }
        return Status(pid: getpid(), protocolVersion: DaemonProtocol.version, buildID: DaemonBuild.currentID,
                      startedAt: startedAt, connections: connections, subscriptions: bus.subscriptionCount,
                      busy: await busyAreas())
    }
}

/// Identifies a daemon build so the app can tell a daemon left over from an older build (the
/// in-app updater rebuilt the app while it was running) from one it just spawned.
enum DaemonBuild {
    /// The executable's path and modification time. Changes on every rebuild or reinstall.
    static func id(forExecutable url: URL) -> String {
        let mtime = (try? FileManager.default.attributesOfItem(atPath: url.path)[.modificationDate] as? Date)
            .map { String(Int($0.timeIntervalSince1970)) } ?? "?"
        return "\(url.standardizedFileURL.path)@\(mtime)"
    }

    /// This process's build id. Only meaningful inside `jacad`; the app computes the id of the
    /// `jacad` it would spawn with `id(forExecutable:)`.
    /// Whether a running daemon's build is older than this app's `jacad`: the same check as an
    /// update replacing the binary. Two different builds (a worktree build and the installed app)
    /// don't replace each other: the newer one wins, instead of each restarting the other's daemon
    /// on every reconnect. Unreadable ids are never "older".
    static func isOlder(_ running: String, than ours: String) -> Bool {
        func mtime(_ id: String) -> Int? { id.split(separator: "@").last.flatMap { Int($0) } }
        guard let r = mtime(running), let o = mtime(ours) else { return false }
        return r < o
    }

    static let currentID: String = {
        guard let exe = Bundle.main.executableURL else { return "unknown" }
        return id(forExecutable: exe)
    }()
}

/// Runs async operations one after another in submission order (per connection).
final class SerialChain: @unchecked Sendable {
    private let lock = NSLock()
    private var tail: Task<Void, Never>?

    func enqueue(_ operation: @escaping @Sendable () async -> Void) {
        lock.lock()
        let previous = tail
        tail = Task {
            await previous?.value
            await operation()
        }
        lock.unlock()
    }
}
