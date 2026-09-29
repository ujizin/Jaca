import XCTest
@testable import Jaca

/// A poll the test feeds by hand, counting how many times a poll was started.
final class ScriptedPoll: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: AsyncStream<CloudPollEvent>.Continuation?
    private(set) var started = 0

    func make() -> AsyncStream<CloudPollEvent> {
        let (stream, c) = AsyncStream<CloudPollEvent>.makeStream()
        lock.withLock { continuation = c; started += 1 }
        return stream
    }

    var isPolling: Bool { lock.withLock { continuation != nil } }

    func emit(_ messages: [String], labels: [String: String] = [:]) {
        let entries = messages.map { m in
            CloudLogEntry(insertId: UUID().uuidString, timestamp: Date(), receiveTimestamp: nil, severity: .info,
                          logName: "projects/p/logs/app", logId: "app", message: m, payloadKind: .text,
                          labels: labels, resourceType: "", resourceLabels: [:], trace: nil, spanId: nil,
                          httpRequestSummary: nil, raw: m)
        }
        lock.withLock { _ = continuation?.yield(.batch(entries)) }
    }
}

@MainActor
final class DaemonCloudTests: XCTestCase {
    private var tmp: URL!

    override func setUp() async throws {
        tmp = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("cl-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
    }

    override func tearDown() async throws {
        try? FileManager.default.removeItem(at: tmp)
    }

    private func waitUntil(_ timeout: Duration = .seconds(3), _ cond: () -> Bool) async throws {
        let deadline = ContinuousClock.now + timeout
        while !cond() {
            guard ContinuousClock.now < deadline else { return XCTFail("condition not met in time") }
            try await Task.sleep(for: .milliseconds(20))
        }
    }

    private func cloudEngine() -> CloudEngine {
        var project = CloudProject(projectID: "p")
        project.selectedLogName = "projects/p/logs/app"
        let store = CloudProjectStore(fileURL: tmp.appendingPathComponent("projects.json"))
        store.save([project])
        return CloudEngine(store: store, templateStore: CloudTemplateStore(fileURL: tmp.appendingPathComponent("templates.json")),
                           detectOnInit: false)
    }

    private func streamEngine(_ id: UUID, poll: ScriptedPoll, labels: CloudEngine?) -> CloudStreamEngine {
        let dir = tmp!
        return CloudStreamEngine(
            id: id,
            cli: { GcloudCLI(binary: URL(fileURLWithPath: "/usr/bin/true")) },
            recordLabels: { keys, project, logName in labels?.recordLabelKeys(keys, project: project, logName: logName) },
            markUnauthenticated: {},
            makePoll: { _, _ in poll.make() },
            makeDatabase: { CloudLogDatabase(sessionID: $0, url: dir.appendingPathComponent("\($0.uuidString).sqlite")) })
    }

    // MARK: - Wire formats

    func test_cloudStateAndAuth_roundTrip() throws {
        var state = CloudState()
        state.authState = .authenticated(account: "me@example.com")
        state.projects = [CloudProject(projectID: "p")]
        state.binaryPath = "/opt/homebrew/bin/gcloud"
        let back = try JSONDecoder.daemon.decode(CloudState.self, from: JSONEncoder.daemon.encode(state))
        XCTAssertEqual(back, state)
        XCTAssertEqual(try JSONDecoder.daemon.decode(CloudAuthState.self, from: Data(#"{"state":"later"}"#.utf8)), .unknown)
        XCTAssertEqual(try JSONDecoder.daemon.decode(CloudAddProjectResult.self, from: JSONEncoder.daemon.encode(CloudAddProjectResult.failure("x"))),
                       .failure("x"))
    }

    func test_cloudLogEntry_roundTrips() throws {
        var e = CloudLogEntry(insertId: "i1", timestamp: Date(timeIntervalSince1970: 1_790_000_000.5), receiveTimestamp: nil,
                              severity: .error, logName: "l", logId: "app", message: "m", payloadKind: .json,
                              labels: ["user": "u1"], resourceType: "", resourceLabels: [:], trace: nil, spanId: nil,
                              httpRequestSummary: "GET / → 200", raw: "{}")
        e.seq = 42
        let back = try JSONDecoder.daemon.decode(CloudLogEntry.self, from: JSONEncoder.daemon.encode(e))
        XCTAssertEqual(back, e)
    }

    // MARK: - Engine

    func test_streamEngine_stampsPersistsAndDetectsLabels() async throws {
        let poll = ScriptedPoll()
        let labels = cloudEngine()
        let e = streamEngine(UUID(), poll: poll, labels: labels)
        var received: [CloudLogEntry] = []
        e.onEntries = { received += $0 }
        e.start(CloudStreamConfig(projectID: "p", logName: "projects/p/logs/app"))
        try await waitUntil { poll.isPolling }
        poll.emit(["a", "b"], labels: ["user_id": "u1"])
        try await waitUntil { received.count == 2 }
        XCTAssertEqual(received.map(\.seq), [CloudStreamEngine.forwardSeqBase, CloudStreamEngine.forwardSeqBase + 8])
        XCTAssertTrue(e.state.hasData)
        XCTAssertEqual(labels.project("p")?.labelKeysByLogName["projects/p/logs/app"]?.contains("user_id"), true)

        try await Task.sleep(for: .milliseconds(100))   // the DB insert is async
        let rs = try await e.query("SELECT text_payload FROM log_entry ORDER BY seq")
        XCTAssertEqual(rs.rows.map { $0.first ?? nil }, ["a", "b"])
        e.dispose()
    }

    func test_streamEngine_withoutGcloud_reportsIt() {
        let e = CloudStreamEngine(cli: { nil }, recordLabels: { _, _, _ in }, markUnauthenticated: {})
        e.start(CloudStreamConfig(projectID: "p"))
        XCTAssertFalse(e.state.isRunning)
        XCTAssertEqual(e.state.statusMessage, "gcloud isn't installed.")
    }

    // MARK: - Daemon + the app side

    func test_cloudTab_streamsQueriesAndReattachesWithoutRepolling() async throws {
        let daemon = try TestDaemon()
        let poll = ScriptedPoll()
        let engine = cloudEngine()
        let sessions = CloudArea.Sessions(bus: daemon.server.bus) { [unowned self] id in
            self.streamEngine(id, poll: poll, labels: engine)
        }
        CloudArea.install(on: daemon.server, engine: engine, sessions: sessions)

        let connector = DaemonConnector(paths: daemon.paths, executable: nil, enabledAreas: [.cloudLogging])
        let registry = CloudLoggingRegistry(
            store: CloudProjectStore(fileURL: tmp.appendingPathComponent("projects.json")),
            templateStore: CloudTemplateStore(fileURL: tmp.appendingPathComponent("templates.json")),
            daemon: connector)
        try await waitUntil { registry.project("p") != nil }

        let id = UUID()
        let tab = CloudLogSession(id: id, projectID: "p", registry: registry, autoStart: true)
        tab.start()
        try await waitUntil { tab.isRunning && poll.isPolling }
        poll.emit(["first", "second"], labels: ["user_id": "u1"])
        try await waitUntil { tab.visible.count == 2 }
        XCTAssertEqual(tab.visible.map(\.message), ["first", "second"])
        // Label keys detected in the daemon reach the app's registry.
        try await waitUntil { registry.project("p")?.currentLabelKeys.contains("user_id") == true }

        // SQL mode runs in the daemon's session database.
        try await Task.sleep(for: .milliseconds(100))
        tab.setViewMode(.sql)
        try await waitUntil { !tab.sqlRunning && tab.sqlMatchCount == 2 }
        XCTAssertNil(tab.sqlError)
        tab.setViewMode(.logs)

        // Relaunch: a tab with the same id attaches and gets the replay, with no second poll.
        let tab2 = CloudLogSession(id: id, projectID: "p", registry: registry)
        try await waitUntil { tab2.visible.count == 2 && tab2.isRunning }
        XCTAssertEqual(poll.started, 1)

        tab2.dispose()
        try await waitUntil { sessions.sessions[id] == nil }
    }
}
