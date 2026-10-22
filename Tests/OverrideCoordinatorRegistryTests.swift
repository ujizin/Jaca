import XCTest
@testable import Jaca

/// Who owns a target's coordinator, and whose arming state the UI shows.
///
/// `InterceptTarget` is `(deviceID, package)`, so it is *identical* across a stop/start of the
/// same app — and shared by two tabs opened on the same app. Every bug this suite pins down comes
/// from that reuse: a late teardown or a second tab writing over a registration that is still in
/// use, after which rule edits stop routing with the toolbar still reading as armed.
@MainActor
final class OverrideCoordinatorRegistryTests: XCTestCase {

    // The rule library goes to a temporary directory and the model runs in-process with no
    // daemon, so these tests never touch the user's rules or a running jacad.
    private var overridesDir: URL!

    override func setUpWithError() throws {
        overridesDir = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("ov-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: overridesDir, withIntermediateDirectories: true)
        setenv("JACA_OVERRIDES_DIR", overridesDir.path, 1)
    }

    override func tearDownWithError() throws {
        unsetenv("JACA_OVERRIDES_DIR")
        try? FileManager.default.removeItem(at: overridesDir)
    }

    @MainActor private func makeModel() -> OverridesModel {
        let offline = DaemonConnector(paths: DaemonPaths(directory: overridesDir), executable: nil, enabledAreas: [])
        return OverridesModel(daemon: offline, inDaemon: false)
    }


    private final class SilentWriter: AgentControlWriter, @unchecked Sendable {
        func write(_ json: String) {}
        func flush(timeout: Duration) async {}
    }

    private final class RecordingWriter: AgentControlWriter, @unchecked Sendable {
        private let lock = NSLock()
        private var _frames: [String] = []
        func write(_ json: String) { lock.lock(); _frames.append(json); lock.unlock() }
        func flush(timeout: Duration) async {}
        var frames: [String] { lock.lock(); defer { lock.unlock() }; return _frames }
    }

    private final class NoopTunnel: AgentHTTPTunnel, @unchecked Sendable {
        func origin(forLocalPort port: Int) -> String { "http://stub:\(port)" }
        func open(localPort port: Int) async throws {}
        func close(localPort port: Int) async {}
        var needsTunnelLedger: Bool { false }
    }

    private let target = InterceptTarget(deviceID: "sim-1", package: "com.example.app")

    private func makeCoordinator(_ services: InterceptServices,
                                 writer: AgentControlWriter = SilentWriter()) -> AgentHTTPCoordinator {
        AgentHTTPCoordinator(transport: .iosSimulatorAgent(bundleID: "com.example.app"),
                          deviceID: target.deviceID,
                          appID: target.package,
                          capabilities: .desktopTerminated,
                          tunnel: NoopTunnel(),
                          writer: writer,
                          onStateChange: { services.reportArming(target: self.target,
                                                                 coordinator: $0, state: $1) })
    }

    /// The registry hands work to the main actor, so let those hops land.
    private func settle() async {
        for _ in 0..<5 { await Task.yield() }
    }

    /// Opening a second tab on the same simulator + app must not unregister the first.
    ///
    /// Both controllers register in `init`, *before* taking the launcher claim — so the newcomer
    /// registered over the live tab and only then discovered the app was already claimed. That
    /// orphaned the live coordinator: `republish()` could no longer reach it, so host and rule
    /// edits silently stopped routing while the tab still showed itself as armed.
    func test_aSecondTabDoesNotEvictTheLiveCoordinator() async {
        let model = makeModel()
        let services = model.services()
        let live = makeCoordinator(services)
        let newcomer = makeCoordinator(services)

        services.register(target: target, coordinator: live)
        await settle()
        services.reportArming(target: target, coordinator: live, state: .waitingForAgent)
        await settle()

        services.register(target: target, coordinator: newcomer)
        await settle()
        // The newcomer's claim fails, and it says so — that report must not reach the live tab.
        services.reportArming(target: target, coordinator: newcomer,
                              state: .failed("another Network tab is already capturing"))
        await settle()

        XCTAssertEqual(model.arming(for: target), .waitingForAgent,
                       "a second tab's failure overwrote the live tab's arming state")
    }

    /// …and closing that second tab must not take the live tab's registration with it.
    func test_closingTheSecondTabLeavesTheLiveTabRegistered() async {
        let model = makeModel()
        let services = model.services()
        let live = makeCoordinator(services)
        let newcomer = makeCoordinator(services)

        services.register(target: target, coordinator: live)
        services.register(target: target, coordinator: newcomer)
        await settle()
        services.reportArming(target: target, coordinator: live, state: .waitingForAgent)
        await settle()

        services.deregister(target: target, coordinator: newcomer)
        await settle()

        XCTAssertEqual(model.arming(for: target), .waitingForAgent,
                       "closing a rejected second tab cleared the live tab's arming state")
    }

    /// The restart case must still work: a coordinator that has already been stopped is replaced,
    /// because its deregistration is still in flight behind a flush.
    func test_aStoppedCoordinatorIsReplacedByTheRestart() async {
        let model = makeModel()
        let services = model.services()
        let outgoing = makeCoordinator(services)
        let incoming = makeCoordinator(services)

        services.register(target: target, coordinator: outgoing)
        await settle()
        await outgoing.stop()                       // the tab is restarting
        services.register(target: target, coordinator: incoming)
        await settle()

        services.reportArming(target: target, coordinator: incoming, state: .waitingForAgent)
        await settle()
        XCTAssertEqual(model.arming(for: target), .waitingForAgent,
                       "the restarted tab never took over the registration")

        // The outgoing coordinator's teardown lands last and must change nothing.
        services.reportArming(target: target, coordinator: outgoing, state: .idle)
        services.deregister(target: target, coordinator: outgoing)
        await settle()
        XCTAssertEqual(model.arming(for: target), .waitingForAgent,
                       "a late teardown clobbered the coordinator that replaced it")
    }

    /// Two captures of one app, both live: the app talks to whichever reached it last, so both
    /// get the hosts. The registry used to keep the older one only, and the newer one's agent
    /// was told to route nothing.
    func test_everyLiveCoordinatorOfATargetGetsTheHosts() async throws {
        let model = makeModel()
        let services = model.services()
        let olderWriter = RecordingWriter(), newerWriter = RecordingWriter()
        let older = makeCoordinator(services, writer: olderWriter)
        let newer = makeCoordinator(services, writer: newerWriter)
        for coordinator in [older, newer] {
            await coordinator.start(pipeline: .passthrough)
            coordinator.agentDidAdvertiseOverrideSupport()
            services.register(target: target, coordinator: coordinator)
        }
        await settle()

        var rule = OverrideRule()
        rule.name = "Stub"
        rule.matcher = OverrideMatcher(pattern: "https://api.example.com/**", kind: .glob, methods: ["GET"])
        rule.action = .respond(OverrideResponseSpec(statusCode: 418, headers: [], body: .inline("{}")))
        model.save(rule)
        await settle()

        for (name, writer) in [("older", olderWriter), ("newer", newerWriter)] {
            let last = try XCTUnwrap(writer.frames.last, "the \(name) capture was told nothing")
            XCTAssertTrue(last.contains("api.example.com"), "the \(name) capture got \(last)")
        }

        // The toolbar shows the armed one, whichever registered first.
        services.reportArming(target: target, coordinator: older, state: .waitingForAgent)
        services.reportArming(target: target, coordinator: newer, state: .active(port: 1, hosts: ["api.example.com"]))
        await settle()
        XCTAssertEqual(model.arming(for: target), .active(port: 1, hosts: ["api.example.com"]))

        // Closing the armed one leaves the other's state, not nothing.
        services.deregister(target: target, coordinator: newer)
        await settle()
        XCTAssertEqual(model.arming(for: target), .waitingForAgent)
        await older.stop()
        await newer.stop()
    }
}
