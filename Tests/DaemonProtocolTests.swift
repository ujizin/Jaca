import XCTest
import NIOCore
import NIOEmbedded
@testable import Jaca

/// A daemon server on a private socket for one test. The temp directory path stays well under
/// the 104-byte `sun_path` limit.
final class TestDaemon {
    let paths: DaemonPaths
    let server: DaemonServer

    init(idleTimeout: TimeInterval = 0) throws {
        let dir = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent("jd-\(UUID().uuidString.prefix(8))", isDirectory: true)
        paths = DaemonPaths(directory: dir)
        server = DaemonServer(paths: paths, idleTimeout: idleTimeout)
        try server.start()
    }

    func client() async throws -> DaemonClient {
        try await DaemonClient.connect(paths: paths)
    }

    deinit {
        server.stop()
        try? FileManager.default.removeItem(at: paths.directory)
    }
}

final class DaemonProtocolTests: XCTestCase {
    // MARK: - Codec

    func test_absentAndNullParams_decodeAsEmptyObject() throws {
        struct P: Decodable { var x: Int? }
        for line in [#"{"id":1,"method":"m"}"#, #"{"id":1,"method":"m","params":null}"#] {
            let p = try JSONDecoder.daemon.decode(RPCRequestParams<P>.self, from: Data(line.utf8)).params
            XCTAssertNil(p.x, line)
        }
    }

    func test_decodingErrors_nameTheFieldPath() {
        struct P: Decodable { var topics: [String] }
        let line = Data(#"{"params":{"topics":3}}"#.utf8)
        do {
            _ = try JSONDecoder.daemon.decode(RPCRequestParams<P>.self, from: line)
            XCTFail("expected a failure")
        } catch {
            XCTAssertTrue(RPCError.describing(error).contains("params.topics"), RPCError.describing(error))
        }
    }

    func test_header_readsEventTopicAndIgnoresOtherParams() throws {
        let event = try JSONDecoder.daemon.decode(
            RPCHeader.self, from: Data(#"{"jsonrpc":"2.0","method":"event","params":{"topic":"a.b","data":[1]}}"#.utf8))
        XCTAssertEqual(event.params?.topic, "a.b")
        let request = try JSONDecoder.daemon.decode(
            RPCHeader.self, from: Data(#"{"id":"x","method":"m","params":[1,2]}"#.utf8))
        XCTAssertEqual(request.id, .string("x"))
        XCTAssertNil(request.params)
    }

    func test_datesRoundTripWithMilliseconds() throws {
        struct D: Codable, Equatable { var at: Date }
        let at = Date(timeIntervalSince1970: 1_790_000_000.123)
        let data = try JSONEncoder.daemon.encode(D(at: at))
        XCTAssertTrue(String(decoding: data, as: UTF8.self).contains(".123Z"))
        let back = try JSONDecoder.daemon.decode(D.self, from: data)
        XCTAssertEqual(back.at.timeIntervalSince1970, at.timeIntervalSince1970, accuracy: 0.001)
    }

    func test_encodedLines_haveNoInteriorNewlines() throws {
        let line = try DaemonLine.encode(RPCResponseEnvelope(id: .number(1), result: "a\nb"))
        XCTAssertEqual(line.filter { $0 == 0x0A }.count, 1)
        XCTAssertEqual(line.last, 0x0A)
    }

    // MARK: - Server + client

    func test_pingAndHello() async throws {
        let daemon = try TestDaemon()
        let client = try await daemon.client()
        let pong: String = try await client.call("ping")
        XCTAssertEqual(pong, "pong")
        let hello: DaemonServer.HelloResult = try await client.call(
            "hello", DaemonServer.HelloParams(protocolVersion: DaemonProtocol.version, client: "test"))
        XCTAssertEqual(hello.protocolVersion, DaemonProtocol.version)
        XCTAssertEqual(hello.pid, getpid())
    }

    func test_versionMismatch_isAnError() async throws {
        let daemon = try TestDaemon()
        let client = try await daemon.client()
        do {
            let _: DaemonServer.HelloResult = try await client.call(
                "hello", DaemonServer.HelloParams(protocolVersion: DaemonProtocol.version + 1, client: nil))
            XCTFail("expected a version mismatch")
        } catch let error as RPCError {
            XCTAssertEqual(error.code, RPCError.versionMismatchCode)
        }
    }

    func test_unknownMethodAndBadParams_areErrorsAndKeepTheConnection() async throws {
        let daemon = try TestDaemon()
        let client = try await daemon.client()
        do {
            let _: String = try await client.call("nope")
            XCTFail("expected an error")
        } catch let error as RPCError {
            XCTAssertEqual(error.code, RPCError.methodNotFoundCode)
        }
        do {
            _ = try await client.callRaw("events.subscribe", paramsJSON: Data(#"{"topics":3}"#.utf8))
            XCTFail("expected an error")
        } catch let error as RPCError {
            XCTAssertEqual(error.code, RPCError.invalidParamsCode)
        }
        let pong: String = try await client.call("ping")
        XCTAssertEqual(pong, "pong")
    }

    func test_handlerErrors_becomeFailedResponses() async throws {
        struct Boom: Error, LocalizedError { var errorDescription: String? { "boom" } }
        let daemon = try TestDaemon()
        daemon.server.router.register("test.fail", "") { (_: RPCEmpty, _) -> String in throw Boom() }
        let client = try await daemon.client()
        do {
            let _: String = try await client.call("test.fail")
            XCTFail("expected an error")
        } catch let error as RPCError {
            XCTAssertEqual(error, RPCError.failed("boom"))
        }
    }

    func test_manyConcurrentCalls_areCorrelatedById() async throws {
        let daemon = try TestDaemon()
        daemon.server.router.register("test.echo", "", params: [Int].self) { p, _ in
            try? await Task.sleep(for: .milliseconds(Int.random(in: 0...20)))
            return p
        }
        let client = try await daemon.client()
        try await withThrowingTaskGroup(of: Void.self) { group in
            for i in 0..<50 {
                group.addTask {
                    let echoed: [Int] = try await client.call("test.echo", [i, i * 2])
                    XCTAssertEqual(echoed, [i, i * 2])
                }
            }
            try await group.waitForAll()
        }
    }

    func test_requestsOnAConnection_runInOrder_exceptConcurrentOnes() async throws {
        let daemon = try TestDaemon()
        let log = Recorder()
        daemon.server.router.register("t.slow", "") { (_: RPCEmpty, _) -> Bool in
            try await Task.sleep(for: .milliseconds(150)); log.add("slow"); return true
        }
        daemon.server.router.register("t.fast", "") { (_: RPCEmpty, _) -> Bool in log.add("fast"); return true }
        daemon.server.router.register("t.du", "", concurrent: true) { (_: RPCEmpty, _) -> Bool in
            try await Task.sleep(for: .milliseconds(400)); log.add("du"); return true
        }
        let client = try await daemon.client()
        // Pipelined without awaiting: the ordered pair keeps its order; the concurrent call
        // sent first doesn't hold them up.
        async let du: Bool = client.call("t.du")
        try await Task.sleep(for: .milliseconds(20))
        async let slow: Bool = client.call("t.slow")
        try await Task.sleep(for: .milliseconds(20))
        async let fast: Bool = client.call("t.fast")
        _ = try await (du, slow, fast)
        XCTAssertEqual(log.items, ["slow", "fast", "du"])
    }

    func test_events_retainedTopicReplaysLastValueToNewSubscriber() async throws {
        let daemon = try TestDaemon()
        daemon.server.bus.publish("t.state", ["v": 1], retain: true)
        daemon.server.bus.publish("t.state", ["v": 2], retain: true)
        let client = try await daemon.client()
        var events = try await client.subscribe(["t.state"]).makeAsyncIterator()
        let first = await events.next()
        XCTAssertEqual(try first?.decode([String: Int].self), ["v": 2])

        daemon.server.bus.publish("t.state", ["v": 3], retain: true)
        let second = await events.next()
        XCTAssertEqual(try second?.decode([String: Int].self), ["v": 3])
    }

    func test_events_onlyReachSubscribers() async throws {
        let daemon = try TestDaemon()
        let client = try await daemon.client()
        var events = try await client.subscribe(["t.a"]).makeAsyncIterator()
        daemon.server.bus.publish("t.b", "ignored")
        daemon.server.bus.publish("t.a", "wanted")
        let event = await events.next()
        XCTAssertEqual(event?.topic, "t.a")
        XCTAssertEqual(try event?.decode(String.self), "wanted")
    }

    func test_onDemandHooks_followFirstAndLastSubscriber() async throws {
        let daemon = try TestDaemon()
        let log = Recorder()
        daemon.server.bus.onDemand(prefix: "poll.", start: { log.add("start \($0)") }, stop: { log.add("stop \($0)") })
        let a = try await daemon.client()
        let b = try await daemon.client()
        // Hold the streams: dropping one unsubscribes it.
        let streamA = try await a.subscribe(["poll.x"])
        let streamB = try await b.subscribe(["poll.x"])
        XCTAssertEqual(log.items, ["start poll.x"])
        a.close()
        try await waitUntil { daemon.server.bus.subscriptionCount == 1 }
        XCTAssertEqual(log.items, ["start poll.x"])
        b.close()
        try await waitUntil { log.items.count == 2 }
        XCTAssertEqual(log.items, ["start poll.x", "stop poll.x"])
        withExtendedLifetime((streamA, streamB)) {}
    }

    func test_droppingAStream_unsubscribesItsTopics() async throws {
        let daemon = try TestDaemon()
        let client = try await daemon.client()
        var stream: AsyncStream<DaemonEventLine>? = try await client.subscribe(["t.drop"])
        XCTAssertNotNil(stream)
        XCTAssertTrue(daemon.server.bus.hasSubscribers("t.drop"))
        stream = nil
        try await waitUntil { !daemon.server.bus.hasSubscribers("t.drop") }
    }

    func test_serverStop_failsPendingCallsAndFinishesStreams() async throws {
        let daemon = try TestDaemon()
        daemon.server.router.register("test.hang", "") { (_: RPCEmpty, _) -> String in
            try await Task.sleep(for: .seconds(30))
            return "late"
        }
        let client = try await daemon.client()
        let stream = try await client.subscribe(["t.x"])
        let closed = expectation(description: "onClose")
        client.onClose { closed.fulfill() }
        async let hung: String = client.call("test.hang")
        try await Task.sleep(for: .milliseconds(100))
        daemon.server.stop()
        do {
            _ = try await hung
            XCTFail("expected disconnect")
        } catch let error as RPCError {
            XCTAssertEqual(error.code, RPCError.disconnectedCode)
        }
        for await _ in stream {}   // finishes
        await fulfillment(of: [closed], timeout: 2)
        XCTAssertTrue(client.isClosed)
    }

    func test_secondServer_onSameDirectory_reportsAlreadyRunning() throws {
        let daemon = try TestDaemon()
        let second = DaemonServer(paths: daemon.paths, idleTimeout: 0)
        XCTAssertThrowsError(try second.start()) { error in
            guard case DaemonServer.StartError.alreadyRunning = error else {
                return XCTFail("unexpected \(error)")
            }
        }
    }

    func test_staleSocketFile_isReplaced() async throws {
        let dir = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("jd-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let paths = DaemonPaths(directory: dir)
        FileManager.default.createFile(atPath: paths.socket.path, contents: Data("stale".utf8))
        let server = DaemonServer(paths: paths, idleTimeout: 0)
        try server.start()
        defer { server.stop() }
        let client = try await DaemonClient.connect(paths: paths)
        let pong: String = try await client.call("ping")
        XCTAssertEqual(pong, "pong")
    }

    func test_idleTimeout_stopsServerWithNoClients() async throws {
        let daemon = try TestDaemon(idleTimeout: 1)
        let stopped = expectation(description: "stopped")
        daemon.server.onStop = { stopped.fulfill() }
        await fulfillment(of: [stopped], timeout: 5)
        XCTAssertFalse(FileManager.default.fileExists(atPath: daemon.paths.socket.path))
    }

    func test_idleTimeout_waitsForBusyAreas() async throws {
        let daemon = try TestDaemon(idleTimeout: 1)
        let busy = Recorder()
        busy.add("busy")
        daemon.server.addBusyCheck("test") { !busy.items.isEmpty }
        try await Task.sleep(for: .milliseconds(1600))
        XCTAssertTrue(FileManager.default.fileExists(atPath: daemon.paths.socket.path))
        let status = await daemon.server.status()
        XCTAssertEqual(status.busy, ["test"])
    }

    func test_subscribeFromAConnectionThatAlreadyClosed_isIgnored() {
        let bus = DaemonEventBus()
        let log = Recorder()
        bus.onDemand(prefix: "poll.", start: { log.add("start \($0)") }, stop: { log.add("stop \($0)") })
        bus.subscribe(peerID: 42, topics: ["poll.x"])   // no such peer: it disconnected first
        XCTAssertFalse(bus.hasSubscribers("poll.x"))
        XCTAssertEqual(log.items, [], "no producer started that nothing would ever stop")
    }

    func test_resubscribingRightAfterDroppingAStream_keepsDelivery() async throws {
        let daemon = try TestDaemon()
        let client = try await daemon.client()
        var first: AsyncStream<DaemonEventLine>? = try await client.subscribe(["t.re"])
        XCTAssertNotNil(first)
        first = nil                                      // unsubscribes
        var second = try await client.subscribe(["t.re"]).makeAsyncIterator()
        try await Task.sleep(for: .milliseconds(100))    // let any stray unsubscribe land
        daemon.server.bus.publish("t.re", "still here")
        let event = await second.next()
        XCTAssertEqual(try event?.decode(String.self), "still here")
    }

    func test_buildAge_onlyAnOlderBuildIsReplaced() {
        XCTAssertTrue(DaemonBuild.isOlder("/a/jacad@100", than: "/a/jacad@200"))
        XCTAssertFalse(DaemonBuild.isOlder("/b/jacad@300", than: "/a/jacad@200"), "a newer other build stays")
        XCTAssertFalse(DaemonBuild.isOlder("unknown", than: "/a/jacad@200"))
    }

    // MARK: - Slow consumers

    func test_droppableEvents_areSkippedForASlowPeerAndReported() throws {
        let bus = DaemonEventBus()
        let channel = EmbeddedChannel()
        try channel.connect(to: SocketAddress(ipAddress: "127.0.0.1", port: 1)).wait()
        let peer = DaemonPeer(id: 1, channel: channel)
        bus.add(peer)
        bus.subscribe(peerID: 1, topics: ["logs.x"])

        channel.isWritable = false
        bus.publish("logs.x", 1, droppable: true)
        bus.publish("logs.x", 2, droppable: true)
        channel.isWritable = true
        bus.publish("logs.x", 3, droppable: true)
        channel.embeddedEventLoop.run()

        var lines: [String] = []
        while let buffer = try channel.readOutbound(as: ByteBuffer.self) {
            lines.append(String(buffer: buffer))
        }
        XCTAssertEqual(lines.count, 2)
        guard lines.count == 2 else { return }
        XCTAssertTrue(lines[0].contains("\"topic\":\"events.dropped\""), lines[0])
        XCTAssertTrue(lines[0].contains("\"count\":2"), lines[0])
        XCTAssertTrue(lines[1].contains("\"data\":3"), lines[1])
    }

    // MARK: - Helpers

    private func waitUntil(timeout: Duration = .seconds(2), _ condition: @escaping () -> Bool) async throws {
        let deadline = ContinuousClock.now + timeout
        while !condition() {
            guard ContinuousClock.now < deadline else { return XCTFail("condition not met in time") }
            try await Task.sleep(for: .milliseconds(20))
        }
    }
}

final class Recorder: @unchecked Sendable {
    private let lock = NSLock()
    private var storage: [String] = []
    func add(_ s: String) { lock.lock(); storage.append(s); lock.unlock() }
    var items: [String] { lock.lock(); defer { lock.unlock() }; return storage }
}
