import XCTest
@testable import Jaca

/// A log source the test feeds by hand.
final class ScriptedLogSource: LogSource, @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: AsyncStream<LogLine>.Continuation?
    private(set) var started = 0

    func start() throws -> AsyncStream<LogLine> {
        let (stream, c) = AsyncStream<LogLine>.makeStream()
        lock.withLock { continuation = c; started += 1 }
        return stream
    }

    func stop() { lock.withLock { continuation?.finish(); continuation = nil } }

    func emit(_ message: String, tag: String = "Test", level: LogLevel = .info, pid: Int32 = 1) {
        let line = LogLine(seq: 0, timestamp: Date(), level: level, tag: tag, pid: pid, tid: 0,
                           message: message, raw: "raw \(message)")
        lock.withLock { _ = continuation?.yield(line) }
    }

    var isStreaming: Bool { lock.withLock { continuation != nil } }
}

private let device = Device(id: "emu-1", platform: .iosSimulator, model: "Sim", state: .booted)

@MainActor
final class DaemonLogsTests: XCTestCase {
    private func waitUntil(_ timeout: Duration = .seconds(3), _ cond: () -> Bool) async throws {
        let deadline = ContinuousClock.now + timeout
        while !cond() {
            guard ContinuousClock.now < deadline else { return XCTFail("condition not met in time") }
            try await Task.sleep(for: .milliseconds(20))
        }
    }

    private func engine(_ source: ScriptedLogSource, id: UUID = UUID(), seqStart: UInt64 = 0) -> LogStreamEngine {
        LogStreamEngine(id: id, device: device, adbURL: nil, seqStart: seqStart,
                        makeSource: { _ in source }, prettifyEnabled: { false })
    }

    // MARK: - Wire format

    func test_logLine_roundTripsAndOmitsDefaults() throws {
        var line = LogLine(seq: 16, timestamp: Date(timeIntervalSince1970: 1_790_000_000.25), level: .warn,
                           tag: "T", pid: 7, tid: 0, message: "hi", raw: "hi")
        line.isConsoleOutput = true
        let json = String(decoding: try JSONEncoder.daemon.encode(line), as: UTF8.self)
        XCTAssertFalse(json.contains("\"r\""), "raw equal to message is omitted: \(json)")
        XCTAssertFalse(json.contains("\"mk\""), json)
        XCTAssertFalse(json.contains("\"i\""), "zero tid is omitted: \(json)")
        let back = try JSONDecoder.daemon.decode(LogLine.self, from: Data(json.utf8))
        XCTAssertEqual(back, line)
    }

    func test_logLine_minimalRecordDecodes() throws {
        let back = try JSONDecoder.daemon.decode(LogLine.self, from: Data(#"{"s":3,"m":"x","l":99}"#.utf8))
        XCTAssertEqual(back.seq, 3)
        XCTAssertEqual(back.raw, "x")
        XCTAssertEqual(back.level, .verbose, "an unknown level reads as verbose")
    }

    // MARK: - Engine

    func test_engine_stampsSeqsAndBatchesLines() async throws {
        let source = ScriptedLogSource()
        let e = engine(source, seqStart: 800)
        var received: [LogLine] = []
        e.onLines = { received += $0 }
        e.start()
        try await waitUntil { source.isStreaming }
        source.emit("a"); source.emit("b")
        try await waitUntil { received.count == 2 }
        XCTAssertEqual(received.map(\.seq), [800, 808])
        e.stop()
        XCTAssertFalse(e.state.isRunning)
    }

    func test_engine_marksCrashesOfTheTargetedAppOnly() async throws {
        let source = ScriptedLogSource()
        let e = LogStreamEngine(device: Device(id: "a", platform: .android, model: "A", state: .connected),
                                adbURL: nil, makeSource: { _ in source }, prettifyEnabled: { false })
        var received: [LogLine] = []
        e.onLines = { received += $0 }
        e.start()
        try await waitUntil { source.isStreaming }
        source.emit("FATAL EXCEPTION: main", tag: "AndroidRuntime", level: .error, pid: 42)
        try await waitUntil { received.contains { $0.isMarker && $0.markerCritical } }
        e.stop()
    }

    // MARK: - Daemon sessions

    private func registry(_ daemon: TestDaemon, source: ScriptedLogSource, orphan: TimeInterval = 600) -> LogsArea.Registry {
        let r = LogsArea.Registry(bus: daemon.server.bus, history: nil, replayCap: 100, orphanTimeout: orphan) { id, _, dev, pkg, seqStart in
            LogStreamEngine(id: id, device: dev, adbURL: nil, package: pkg, seqStart: seqStart,
                            makeSource: { _ in source }, prettifyEnabled: { false })
        }
        LogsArea.install(on: daemon.server, registry: r)
        return r
    }

    func test_logsArea_streamsReplaysAndCloses() async throws {
        let daemon = try TestDaemon()
        let source = ScriptedLogSource()
        let reg = registry(daemon, source: source)
        let client = try await daemon.client()

        let info: LogsArea.SessionInfo = try await client.call(
            "logs.open", LogsArea.OpenParams(id: nil, device: device, package: nil, displayName: "t", autoStart: true, seqStart: nil))
        XCTAssertTrue(info.state.isRunning)
        var events = try await client.subscribe([LogsArea.linesTopic(info.id)]).makeAsyncIterator()
        try await waitUntil { source.isStreaming }
        source.emit("one"); source.emit("two")

        var live: [LogLine] = []
        while live.count < 2, let event = await events.next() { live += try event.decode([LogLine].self) }
        XCTAssertEqual(live.map(\.message), ["one", "two"])

        let all: [LogLine] = try await client.call("logs.range", LogsArea.RangeParams(id: info.id, afterSeq: nil, limit: nil))
        XCTAssertEqual(all.map(\.message), ["one", "two"])
        let after: [LogLine] = try await client.call("logs.range", LogsArea.RangeParams(id: info.id, afterSeq: all[0].seq, limit: nil))
        XCTAssertEqual(after.map(\.message), ["two"])

        // Attaching by id returns the same running session.
        let again: LogsArea.SessionInfo = try await client.call(
            "logs.open", LogsArea.OpenParams(id: info.id, device: device, package: nil, displayName: nil, autoStart: false, seqStart: nil))
        XCTAssertEqual(again.id, info.id)
        XCTAssertTrue(again.state.isRunning)
        XCTAssertTrue(reg.isBusy)

        let closed: Bool = try await client.call("logs.close", LogsArea.IDParams(id: info.id))
        XCTAssertTrue(closed)
        XCTAssertFalse(reg.isBusy)
        do {
            let _: RPCEmpty = try await client.call("logs.start", LogsArea.IDParams(id: info.id))
            XCTFail("a closed session is gone")
        } catch let error as RPCError {
            XCTAssertEqual(error.code, RPCError.failedCode)
        }
    }

    func test_logsArea_reapsUnwatchedSessions() async throws {
        let daemon = try TestDaemon()
        let source = ScriptedLogSource()
        let reg = registry(daemon, source: source, orphan: 0.2)
        let info = try reg.open(LogsArea.OpenParams(id: nil, device: device, package: nil, displayName: nil, autoStart: true, seqStart: nil))
        reg.reapOrphans()
        XCTAssertNotNil(reg.sessions[info.id], "just opened")
        try await Task.sleep(for: .milliseconds(300))
        reg.reapOrphans()
        XCTAssertNil(reg.sessions[info.id], "closed after the orphan timeout")
    }

    // MARK: - The app side

    /// Tests that run against a live daemon never fall back.
    private let noLocal: (UInt64, @escaping @MainActor () -> String) -> LogStreamEngine = { _, _ in
        LogStreamEngine(device: device, adbURL: nil, makeSource: { _ in nil }, prettifyEnabled: { false })
    }

    func test_remoteTab_fillsAGapReportedByEventsDropped() async throws {
        let daemon = try TestDaemon()
        let source = ScriptedLogSource()
        let reg = registry(daemon, source: source)
        let connector = DaemonConnector(paths: daemon.paths, executable: nil, enabledAreas: [.logs])
        let id = UUID()
        let feed = RemoteLogFeed(id: id, device: device, package: "", displayName: "tab", autoStart: true,
                                 daemon: connector, makeLocal: noLocal)
        let tab = LogSession(id: id, device: device, feed: feed, adbURL: URL(fileURLWithPath: "/usr/bin/true"),
                             filter: LogFilter(), displayName: "tab", isRemote: true)
        try await waitUntil { tab.isRunning && source.isStreaming }
        source.emit("one")
        try await waitUntil { tab.visible.count == 1 }

        // Two lines reach the replay buffer but never this client (it was too slow), then the
        // daemon reports the drop right before the next live batch.
        let hosted = try XCTUnwrap(reg.sessions[id])
        let base = tab.visible[0].seq
        let missed = [8, 16].map { LogLine(seq: base + $0, timestamp: Date(), level: .info, tag: "T", pid: 1, tid: 0,
                                           message: "missed \($0)", raw: "missed") }
        hosted.replay.append(contentsOf: missed)
        let topic = LogsArea.linesTopic(id)
        // As the bus sends it: on the lines topic's connection, ahead of the next batch.
        let note = try DaemonLine.encode(RPCEventEnvelope(topic: "events.dropped",
                                                          data: DroppedEvents(topic: topic, count: 2)))
        daemon.server.bus.publishLine(topic, note, retain: false, droppable: false)
        let after = LogLine(seq: base + 24, timestamp: Date(), level: .info, tag: "T", pid: 1, tid: 0,
                            message: "after", raw: "after")
        hosted.replay.append(after)
        daemon.server.bus.publish(topic, [after])

        try await waitUntil { tab.visible.count == 4 }
        XCTAssertEqual(tab.visible.map(\.message), ["one", "missed 8", "missed 16", "after"], "the gap is filled, in order")
        tab.close()
    }

    func test_remoteTab_streamsInProcessWhenTheDaemonIsUnreachable() async throws {
        let dir = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("jd-\(UUID().uuidString.prefix(8))")
        let connector = DaemonConnector(paths: DaemonPaths(directory: dir), executable: nil, enabledAreas: [.logs])
        let source = ScriptedLogSource()
        let id = UUID()
        var madeLocal = false
        let feed = RemoteLogFeed(id: id, device: device, package: "", displayName: "tab", autoStart: false,
                                 daemon: connector, makeLocal: { seqStart, _ in
            madeLocal = true
            return LogStreamEngine(device: device, adbURL: nil, seqStart: seqStart,
                                   makeSource: { _ in source }, prettifyEnabled: { false })
        })
        let tab = LogSession(id: id, device: device, feed: feed, adbURL: URL(fileURLWithPath: "/usr/bin/true"),
                             filter: LogFilter(), displayName: "tab", isRemote: true)
        try await waitUntil(.seconds(5)) { if case .unavailable = connector.state { return true }; return false }
        tab.start()
        try await waitUntil { madeLocal && tab.isRunning && source.isStreaming }
        source.emit("local line")
        try await waitUntil { tab.visible.count == 1 }
        tab.close()
    }

    func test_remoteTab_receivesLinesAndReattachesAfterRelaunch() async throws {
        let daemon = try TestDaemon()
        let source = ScriptedLogSource()
        _ = registry(daemon, source: source)
        let connector = DaemonConnector(paths: daemon.paths, executable: nil, enabledAreas: [.logs])
        let id = UUID()

        let feed = RemoteLogFeed(id: id, device: device, package: "", displayName: "tab", autoStart: true, daemon: connector, makeLocal: noLocal)
        let tab = LogSession(id: id, device: device, feed: feed, adbURL: URL(fileURLWithPath: "/usr/bin/true"),
                             filter: LogFilter(), displayName: "tab", isRemote: true)
        try await waitUntil { tab.isRunning && source.isStreaming }
        source.emit("hello"); source.emit("world")
        try await waitUntil { tab.visible.count == 2 }
        XCTAssertEqual(tab.visible.map(\.message), ["hello", "world"])

        // "Relaunch": a new tab with the same id attaches and backfills from the replay buffer.
        feed.onLines = nil
        let feed2 = RemoteLogFeed(id: id, device: device, package: "", displayName: "tab", autoStart: false, daemon: connector, makeLocal: noLocal)
        let tab2 = LogSession(id: id, device: device, feed: feed2, adbURL: URL(fileURLWithPath: "/usr/bin/true"),
                              filter: LogFilter(), displayName: "tab", isRemote: true)
        try await waitUntil { tab2.visible.count == 2 && tab2.isRunning }
        source.emit("again")
        try await waitUntil { tab2.visible.count == 3 }
        XCTAssertEqual(tab2.visible.map(\.message), ["hello", "world", "again"], "no duplicates across backfill and live")

        tab2.stop()
        try await waitUntil { !tab2.isRunning }
        tab2.close()
    }

    func test_remoteTab_viewFilterStaysLocal() async throws {
        let daemon = try TestDaemon()
        let source = ScriptedLogSource()
        _ = registry(daemon, source: source)
        let connector = DaemonConnector(paths: daemon.paths, executable: nil, enabledAreas: [.logs])
        let id = UUID()
        let feed = RemoteLogFeed(id: id, device: device, package: "", displayName: "tab", autoStart: true, daemon: connector, makeLocal: noLocal)
        let tab = LogSession(id: id, device: device, feed: feed, adbURL: URL(fileURLWithPath: "/usr/bin/true"),
                             filter: LogFilter(), displayName: "tab", isRemote: true)
        tab.setQuery("keep")
        try await waitUntil { source.isStreaming }
        source.emit("keep me"); source.emit("drop me")
        try await waitUntil { tab.totalCount == 2 }
        XCTAssertEqual(tab.visible.map(\.message), ["keep me"])
        tab.close()
    }
}
