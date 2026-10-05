import Foundation
import Observation

/// **The single owner** of the response-override library in the app: the rules, their order, the
/// master switch, live hit counts, and each transport's arming state. Every override surface reads
/// this one object, so there is never a second copy of "is this rule on?" to drift.
///
/// The runtime (resolver, coordinators, hit reporting) is `OverridesEngine`, which must live in the
/// process that runs the capture: in-process here, or in `jacad` when network capture runs in the
/// daemon — then this model mirrors the daemon's retained `overrides.state`, and compiles the
/// mirrored rules locally for the editor's match previews.
@Observable
@MainActor
final class OverridesModel {

    // MARK: - State (mirrored from the engine)

    private(set) var rules: [OverrideRule] = []

    /// Applies every rule, or none — for "let me see the real thing for a second".
    var masterEnabled: Bool = true {
        didSet {
            guard masterEnabled != oldValue, !applyingState else { return }
            let enabled = masterEnabled
            send("overrides.setMaster", OverridesArea.MasterParams(enabled: enabled)) { $0.setMasterEnabled(enabled) }
        }
    }

    private(set) var hitCounts: [UUID: Int] = [:]
    private(set) var lastHitAt: [UUID: Date] = [:]
    /// Arming state per **device + package**.
    private(set) var armings: [InterceptTarget: InterceptArmingState] = [:]
    /// Reclaimed tunnels from a previous run, surfaced once so cleanup is never silent.
    private(set) var reclaimedTunnelCount = 0
    /// The last thing overrides actually **did**, timestamped.
    private(set) var lastActivity: String?

    /// The compiled snapshot, for match previews and shadow detection in the editor.
    private(set) var compiled = OverrideRuleSet.empty

    // MARK: - Plumbing

    private let daemon: DaemonConnector
    /// Whether the runtime lives in `jacad` (network capture runs there). Follows
    /// `DaemonConnector.networkRunsInDaemon`, and changes with it (`setRuntime(inDaemon:)`).
    private(set) var usesDaemon: Bool
    private var localEngine: OverridesEngine?
    private var daemonWatch: Task<Void, Never>?
    private let commands = DaemonCommandQueue()
    private var applyingState = false
    private var compiledOnce = false

    init(daemon: DaemonConnector? = nil, inDaemon: Bool? = nil) {
        let daemon = daemon ?? .shared
        self.daemon = daemon
        usesDaemon = inDaemon ?? daemon.networkRunsInDaemon()
        if usesDaemon { watchDaemon(reload: false) } else { startLocalEngine() }
    }

    /// Moves the runtime between this process and `jacad` when network capture moves (the HTTPS
    /// decryption setting changed). The side giving it up stops publishing; the daemon re-reads
    /// the library when it takes over, since the app may have edited it meanwhile.
    func setRuntime(inDaemon: Bool) {
        guard inDaemon != usesDaemon else { return }
        usesDaemon = inDaemon
        if inDaemon {
            if let local = localEngine {
                local.onChange = nil
                localEngine = nil
            }
            watchDaemon(reload: true)
        } else {
            daemonWatch?.cancel()
            daemonWatch = nil
            startLocalEngine()
        }
    }

    private func watchDaemon(reload: Bool) {
        // First frame from disk (read-only); the daemon's retained state follows.
        var initial = OverridesState()
        initial.rules = OverrideRuleStore.load()
        initial.masterEnabled = FeatureFlags.overridesMasterEnabled
        apply(initial)
        if reload {
            commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call("overrides.reload") }
        }
        daemonWatch = daemon.watch([OverridesArea.stateTopic]) { [weak self] event in
            guard let self, self.usesDaemon, let state = try? event.decode(OverridesState.self) else { return }
            self.apply(state)
        }
    }

    @discardableResult
    private func startLocalEngine() -> OverridesEngine {
        if let localEngine { return localEngine }
        let engine = OverridesEngine()
        engine.onChange = { [weak self] in self?.apply($0) }
        localEngine = engine
        apply(engine.state)
        return engine
    }

    private func apply(_ state: OverridesState) {
        applyingState = true
        defer { applyingState = false }
        let rulesChanged = rules != state.rules || masterEnabled != state.masterEnabled
        rules = state.rules
        masterEnabled = state.masterEnabled
        hitCounts = Dictionary(uniqueKeysWithValues: state.hitCounts.compactMap { k, v in UUID(uuidString: k).map { ($0, v) } })
        lastHitAt = Dictionary(uniqueKeysWithValues: state.lastHitAt.compactMap { k, v in UUID(uuidString: k).map { ($0, v) } })
        armings = Dictionary(state.armings.map { ($0.target, $0.state) }, uniquingKeysWith: { _, last in last })
        reclaimedTunnelCount = state.reclaimedTunnelCount
        lastActivity = state.lastActivity
        // Compiled here from the rules (a pure function) rather than read from the engine: the
        // engine reports a change before it republishes its resolver.
        if rulesChanged || !compiledOnce {
            compiled = OverrideCompiler.compile(rules, masterEnabled: masterEnabled)
            compiledOnce = true
        }
    }

    /// Runs a mutation on the in-process engine, or sends it to the daemon's (in order).
    private func send<P: Encodable & Sendable>(_ method: String, _ params: P,
                                                local: @escaping (OverridesEngine) -> Void) {
        guard usesDaemon else {
            local(startLocalEngine())
            return
        }
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call(method, params) }
    }

    /// Removes tunnels stranded by a previous run that died without cleaning up. In daemon mode
    /// the daemon does this when it starts.
    func reconcileOrphanedTunnels() {
        guard !usesDaemon else { return }
        startLocalEngine().reconcileOrphanedTunnels()
    }

    // MARK: - Services handed to transports

    /// What an in-process capture source needs to participate. Only meaningful when the runtime
    /// is in this process; the daemon hands its own engine's services to its captures.
    func services() -> InterceptServices {
        startLocalEngine().services()
    }

    /// Services for an in-process capture, or nil when the runtime lives in `jacad` (a tab that
    /// captures in-process there — companion capture — runs without overrides).
    var localServices: InterceptServices? {
        usesDaemon ? nil : services()
    }

    // MARK: - Mutations

    func add(_ rule: OverrideRule) { save(rule) }
    func update(_ rule: OverrideRule) {
        guard rules.contains(where: { $0.id == rule.id }) else { return }
        save(rule)
    }

    /// Saves a rule whether or not it already exists, so the editor never has to choose between
    /// `add` and `update` — getting that wrong silently discarded the rule.
    func save(_ rule: OverrideRule) {
        send("overrides.save", OverridesArea.RuleParams(rule: rule)) { $0.save(rule) }
    }

    func remove(_ id: UUID) {
        send("overrides.remove", OverridesArea.IDParams(id: id)) { $0.remove(id) }
    }

    func setEnabled(_ enabled: Bool, for id: UUID) {
        send("overrides.setEnabled", OverridesArea.EnabledParams(id: id, enabled: enabled)) { $0.setEnabled(enabled, for: id) }
    }

    func duplicate(_ id: UUID) {
        send("overrides.duplicate", OverridesArea.IDParams(id: id)) { $0.duplicate(id) }
    }

    /// Precedence is list order, so moving a rule up is how the user resolves shadowing.
    func move(_ id: UUID, by offset: Int) {
        send("overrides.move", OverridesArea.MoveParams(id: id, offset: offset)) { $0.move(id, by: offset) }
    }

    // MARK: - Derived state the UI renders

    var enabledCount: Int { rules.filter(\.enabled).count }

    func hitCount(for id: UUID) -> Int { hitCounts[id] ?? 0 }

    func diagnostic(for id: UUID) -> String? { compiled.diagnostics[id] }

    /// The rule that produced this response, read from the stamp the transaction carries.
    func appliedRule(for txn: NetworkTransaction) -> OverrideRule? {
        guard let ruleID = txn.overriddenByRuleID else { return nil }
        return rules.first { $0.id == ruleID }
    }

    /// Rules that would match this request but can't run on the given transport — the "matches
    /// but can't run here" state, computed by the same clamp the runtime uses.
    func blockedReason(forURL url: String, method: String,
                       transport: InterceptTransportID,
                       capabilities: InterceptCapabilities) -> InterceptSkipReason? {
        guard let facts = OverrideMatching.facts(url: url) else { return nil }
        guard let matched = compiled.firstMatch(facts: facts, method: method,
                                                deviceID: nil, appID: nil) else { return nil }
        let (_, skip) = OverrideMatching.decide(matched, transport: transport,
                                                capabilities: capabilities,
                                                masterEnabled: masterEnabled)
        return skip
    }

    /// Whether an enabled rule matches this request at all (regardless of transport).
    func matchingRule(forURL url: String, method: String) -> OverrideRule? {
        guard let facts = OverrideMatching.facts(url: url) else { return nil }
        return compiled.firstMatch(facts: facts, method: method, deviceID: nil, appID: nil)?.rule
    }

    func arming(for target: InterceptTarget) -> InterceptArmingState {
        armings[target] ?? .idle
    }

    // MARK: - Seeding from a captured request

    /// Builds a rule pre-filled from a captured transaction — the right-click path. Takes its
    /// **own copy** of the body: `NetworkBodyCache` clears on launch, so a reference would lose
    /// its payload.
    func seed(from txn: NetworkTransaction, session: NetworkSession) async -> OverrideRule {
        let bodies = await session.bodies(for: txn.id)
        let responseBody = bodies.resp ?? Data()

        var rule = OverrideRule()
        rule.name = OverrideSeeding.name(for: txn)
        rule.matcher = OverrideMatcher(pattern: OverrideSeeding.pattern(for: txn),
                                       kind: .glob,
                                       methods: [txn.method.uppercased()])
        rule.routedHosts = OverrideCompiler.derivedRoutedHosts(for: rule.matcher)

        let pretty = OverrideSeeding.prettyPrinted(responseBody, contentType: txn.responseContentType)
        rule.action = .respond(OverrideResponseSpec(
            statusCode: txn.statusCode ?? 200,
            headers: OverrideSeeding.headers(txn.responseHeaders),
            body: OverrideRuleStore.makeBodyRef(pretty)
        ))
        return rule
    }
}
