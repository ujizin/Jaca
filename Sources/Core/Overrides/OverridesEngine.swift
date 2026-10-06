import Foundation

/// The response-override library as one value: what the app renders and what `jacad` publishes on
/// `overrides.state`.
struct OverridesState: Codable, Equatable, Sendable {
    struct Arming: Codable, Equatable, Sendable {
        var target: InterceptTarget
        var state: InterceptArmingState
    }

    var rules: [OverrideRule] = []
    var masterEnabled = true
    /// Keyed by rule id (`uuidString`); runtime only, never persisted.
    var hitCounts: [String: Int] = [:]
    var lastHitAt: [String: Date] = [:]
    var armings: [Arming] = []
    var lastActivity: String?
    var reclaimedTunnelCount = 0
}

extension OverridesState {
    private enum CodingKeys: String, CodingKey {
        case rules, masterEnabled, hitCounts, lastHitAt, armings, lastActivity, reclaimedTunnelCount
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            rules: CloudPersistence.decodeArrayField(OverrideRule.self, in: c, forKey: .rules),
            masterEnabled: (try? c.decodeIfPresent(Bool.self, forKey: .masterEnabled)) ?? true,
            hitCounts: (try? c.decodeIfPresent([String: Int].self, forKey: .hitCounts)) ?? [:],
            lastHitAt: (try? c.decodeIfPresent([String: Date].self, forKey: .lastHitAt)) ?? [:],
            armings: CloudPersistence.decodeArrayField(Arming.self, in: c, forKey: .armings),
            lastActivity: try? c.decodeIfPresent(String.self, forKey: .lastActivity),
            reclaimedTunnelCount: (try? c.decodeIfPresent(Int.self, forKey: .reclaimedTunnelCount)) ?? 0
        )
    }
}

/// **The single owner** of the response-override library at runtime: the rules, their order, the
/// master switch, live hit counts, each transport's arming state, and the coordinators that route
/// traffic. The capture sources that divert traffic get `services()` from this object, so it runs
/// in whichever process runs the capture: the app (daemon off) or `jacad`.
///
/// Rules are **global**, not per-session: sessions are rebuilt on tab open and relaunch-restore,
/// and two tabs on one device must not disagree about what's mocked. Targeting is `scope`.
@MainActor
final class OverridesEngine {
    private(set) var state = OverridesState() {
        didSet { if state != oldValue { onChange?(state) } }
    }
    var onChange: ((OverridesState) -> Void)?

    private let resolver = OverrideResolver()
    private var coordinators: [InterceptTarget: AgentHTTPCoordinator] = [:]

    init() {
        // Synchronous so the first frame already has the user's rules — the cache-first rule.
        if let rules = OverrideRuleStore.loadIfReadable() {
            state.rules = rules
        } else {
            // Nothing in memory to fall back on: hold saves until the file reads again, so the
            // first edit doesn't replace the user's library with one rule.
            diskUnreadable = true
            JacaLog.error("override", "\(OverrideRuleStore.rulesURL.path) isn't readable JSON; not saving over it")
        }
        state.masterEnabled = FeatureFlags.overridesMasterEnabled
        diskStamp = Self.diskModified
        JacaLog.info("override",
            "loaded \(state.rules.count) rule(s) from \(OverrideRuleStore.rulesURL.path); master=\(state.masterEnabled)")
        republish()
    }

    /// Re-reads the library from disk: the app edited it in-process while network capture ran
    /// there, and hands the runtime back to this engine.
    func reload() {
        diskStamp = Self.diskModified
        if let rules = OverrideRuleStore.loadIfReadable() {
            state.rules = rules
            diskUnreadable = false
        } else {
            JacaLog.error("override", "\(OverrideRuleStore.rulesURL.path) isn't readable JSON; keeping the \(state.rules.count) rule(s) in memory")
        }
        state.masterEnabled = FeatureFlags.overridesMasterEnabled
        republish()
    }

    /// The compiled snapshot, for match previews and shadow detection.
    var compiled: OverrideRuleSet { resolver.current }

    func arming(for target: InterceptTarget) -> InterceptArmingState {
        state.armings.first { $0.target == target }?.state ?? .idle
    }

    /// Removes tunnels stranded by a previous run that died without cleaning up.
    func reconcileOrphanedTunnels() {
        let count = AdbTunnelCleanup.reconcileOrphansFromPreviousRuns()
        if count > 0 { state.reclaimedTunnelCount = count }
    }

    // MARK: - Services handed to transports

    /// What a capture source needs to participate: a resolver and a reporter, never this engine.
    func services() -> InterceptServices {
        InterceptServices(
            resolver: resolver,
            reporter: Reporter { [weak self] _, ruleID in
                Task { @MainActor in self?.recordHit(ruleID: ruleID) }
            },
            onArmingChange: { [weak self] target, coordinator, armingState in
                Task { @MainActor in
                    guard let self else { return }
                    // On a restart the old teardown publishes `.idle` ~300 ms after the new
                    // coordinator published `.active`; that stale value would win permanently.
                    if let coordinator, self.coordinators[target] !== coordinator { return }
                    self.setArming(armingState, for: target)
                }
            },
            onRegisterCoordinator: { [weak self] target, coordinator in
                Task { @MainActor in
                    guard let self else { return }
                    // A second tab on the same simulator + app registers before it finds the app
                    // claimed; evicting the live coordinator there orphaned it for good. A
                    // *stopped* one is the ordinary restart: replace it.
                    if let existing = self.coordinators[target],
                       existing !== coordinator, !existing.isStopped { return }
                    self.coordinators[target] = coordinator
                    coordinator.updateHosts(self.routedHosts(for: target))
                }
            },
            onDeregisterCoordinator: { [weak self] target, coordinator in
                Task { @MainActor in
                    guard let self else { return }
                    // A restart reuses the target, so a late teardown must not evict its
                    // replacement.
                    guard self.coordinators[target] === coordinator else { return }
                    self.coordinators.removeValue(forKey: target)
                    self.state.armings.removeAll { $0.target == target }
                }
            }
        )
    }

    /// A `Sendable` shim so the reporter can be called from a NIO event loop.
    private struct Reporter: InterceptReporting {
        let onApplied: @Sendable (UUID, UUID?) -> Void
        init(_ onApplied: @escaping @Sendable (UUID, UUID?) -> Void) { self.onApplied = onApplied }
        func report(requestID: UUID, appliedRuleID: UUID?, skipped: InterceptSkipReason?) {
            onApplied(requestID, appliedRuleID)
        }
    }

    private func setArming(_ armingState: InterceptArmingState, for target: InterceptTarget) {
        if let i = state.armings.firstIndex(where: { $0.target == target }) {
            state.armings[i].state = armingState
        } else {
            state.armings.append(.init(target: target, state: armingState))
        }
    }

    // MARK: - Mutations

    func setMasterEnabled(_ enabled: Bool) {
        syncFromDisk()
        guard enabled != state.masterEnabled else { return }
        state.masterEnabled = enabled
        FeatureFlags.overridesMasterEnabled = enabled
        republish()
    }

    /// Saves a rule whether or not it already exists. A new rule with no divert hosts gets them
    /// derived from its matcher; its `enabled` is left as the editor set it.
    func save(_ rule: OverrideRule) {
        syncFromDisk()
        if let index = state.rules.firstIndex(where: { $0.id == rule.id }) {
            state.rules[index] = rule
        } else {
            var newRule = rule
            if newRule.routedHosts.isEmpty {
                newRule.routedHosts = OverrideCompiler.derivedRoutedHosts(for: newRule.matcher)
            }
            state.rules.append(newRule)
        }
        persistAndRepublish()
    }

    func remove(_ id: UUID) {
        syncFromDisk()
        state.rules.removeAll { $0.id == id }
        state.hitCounts.removeValue(forKey: id.uuidString)
        state.lastHitAt.removeValue(forKey: id.uuidString)
        persistAndRepublish()
    }

    func setEnabled(_ enabled: Bool, for id: UUID) {
        syncFromDisk()
        guard let index = state.rules.firstIndex(where: { $0.id == id }) else { return }
        state.rules[index].enabled = enabled
        persistAndRepublish()
    }

    func duplicate(_ id: UUID) {
        syncFromDisk()
        guard let source = state.rules.first(where: { $0.id == id }) else { return }
        let copy = OverrideRule(id: UUID(),
                                name: source.name.isEmpty ? "Copy" : "\(source.name) copy",
                                enabled: true, matcher: source.matcher, scope: source.scope,
                                action: source.action, delayMillis: source.delayMillis,
                                routedHosts: source.routedHosts)
        state.rules.append(copy)
        persistAndRepublish()
    }

    /// Precedence is list order, so moving a rule up is how the user resolves shadowing.
    func move(_ id: UUID, by offset: Int) {
        syncFromDisk()
        guard let index = state.rules.firstIndex(where: { $0.id == id }) else { return }
        let target = index + offset
        guard state.rules.indices.contains(target) else { return }
        state.rules.swapAt(index, target)
        persistAndRepublish()
    }

    // MARK: - Internals

    /// Hosts to route for one target. The real `deviceID` matters: with it hard-coded nil, a
    /// device-scoped rule contributed no hosts and could never fire.
    private func routedHosts(for target: InterceptTarget) -> Set<String> {
        resolver.current.routedHosts(deviceID: target.deviceID, appID: target.package)
    }

    private func recordHit(ruleID: UUID?) {
        guard let ruleID else { return }
        // `debug`, not `info`: one per overridden request, and `JacaLog.append` blocks on the
        // filesystem from the main actor. `lastActivity` below is the user-facing one.
        let name = state.rules.first { $0.id == ruleID }?.displayName
        JacaLog.debug("override", "rule applied: \(name ?? ruleID.uuidString)")
        let key = ruleID.uuidString
        let now = Date()
        state.hitCounts[key, default: 0] += 1
        state.lastHitAt[key] = now
        state.lastActivity = "\(now.formatted(date: .omitted, time: .standard)) · applied \(name ?? "a rule")"
    }

    /// Modification date of `rules.json` when this engine last read or wrote it.
    private var diskStamp: Date?

    private static var diskModified: Date? {
        try? FileManager.default.attributesOfItem(atPath: OverrideRuleStore.rulesURL.path)[.modificationDate] as? Date
    }

    /// Re-reads the library when another process wrote it since (the app while network capture
    /// ran in-process, or `jacad`), so this engine never saves a stale list over newer edits.
    /// The master switch lives in defaults, not the file, so it is re-read every time.
    private func syncFromDisk() {
        if Self.diskModified != diskStamp {
            reload()
        } else if FeatureFlags.overridesMasterEnabled != state.masterEnabled {
            state.masterEnabled = FeatureFlags.overridesMasterEnabled
            republish()
        }
    }

    /// Set when the engine started on an unreadable `rules.json`, and cleared once it reads.
    private var diskUnreadable = false

    private func persistAndRepublish() {
        defer { diskStamp = Self.diskModified }
        guard !diskUnreadable else {
            JacaLog.error("override", "not saving: \(OverrideRuleStore.rulesURL.path) is unreadable")
            republish()
            return
        }
        let ok = OverrideRuleStore.save(state.rules)
        JacaLog.info("override", "saved \(state.rules.count) rule(s) -> \(ok ? "ok" : "FAILED")")
        republish()
    }

    /// Recompiles and pushes to the resolver and every armed transport — what makes an edit
    /// apply on the app's next request with no re-attach.
    private func republish() {
        resolver.publish(OverrideCompiler.compile(state.rules, masterEnabled: state.masterEnabled))
        for (target, coordinator) in coordinators {
            coordinator.updateHosts(routedHosts(for: target))
        }
    }
}
