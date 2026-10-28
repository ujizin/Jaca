import Foundation

/// The response-override library as one value: what the app renders and what `jacad` publishes on
/// `overrides.state`.
struct OverridesState: Codable, Equatable, Sendable {
    struct Arming: Codable, Equatable, Sendable {
        var target: InterceptTarget
        var state: InterceptArmingState
    }

    /// In display order, which is precedence (`OverrideGrouping`).
    var rules: [OverrideRule] = []
    var groups: [OverrideGroup] = []
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
        case rules, groups, masterEnabled, hitCounts, lastHitAt, armings, lastActivity, reclaimedTunnelCount
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            rules: CloudPersistence.decodeArrayField(OverrideRule.self, in: c, forKey: .rules),
            groups: CloudPersistence.decodeArrayField(OverrideGroup.self, in: c, forKey: .groups),
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
    /// Every live coordinator per target, oldest first, each with the arming state it last
    /// reported. Two captures of one app each run their own server and tunnel, and the app talks
    /// to whichever one it reached last, so every one of them gets the hosts.
    private var coordinators: [InterceptTarget: [Registered]] = [:]

    private struct Registered {
        let coordinator: AgentHTTPCoordinator
        var state: InterceptArmingState = .idle
    }

    init() {
        // Synchronous so the first frame already has the user's rules — the cache-first rule.
        if let rules = OverrideRuleStore.loadIfReadable(), let groups = OverrideRuleStore.loadGroupsIfReadable() {
            state.groups = groups
            state.rules = OverrideGrouping.normalized(rules, groups: groups)
        } else {
            // Nothing in memory to fall back on: hold saves until the file reads again, so the
            // first edit doesn't replace the user's library with one rule.
            diskUnreadable = true
            JacaLog.error("override", "\(OverrideRuleStore.directory.path) has a file that isn't readable JSON; not saving over it")
        }
        state.masterEnabled = FeatureFlags.overridesMasterEnabled
        diskStamp = Self.diskModified
        JacaLog.info("override",
            "loaded \(state.rules.count) rule(s) in \(state.groups.count) group(s) from \(OverrideRuleStore.rulesURL.path); master=\(state.masterEnabled)")
        republish()
    }

    /// Re-reads the library from disk: the app edited it in-process while network capture ran
    /// there, and hands the runtime back to this engine.
    func reload() {
        diskStamp = Self.diskModified
        if let rules = OverrideRuleStore.loadIfReadable(), let groups = OverrideRuleStore.loadGroupsIfReadable() {
            state.groups = groups
            state.rules = OverrideGrouping.normalized(rules, groups: groups)
            diskUnreadable = false
        } else {
            JacaLog.error("override", "\(OverrideRuleStore.directory.path) has a file that isn't readable JSON; keeping the \(state.rules.count) rule(s) in memory")
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
                    guard let coordinator else {
                        self.setArming(armingState, for: target)
                        return
                    }
                    // On a restart the old teardown publishes `.idle` ~300 ms after the new
                    // coordinator published `.active`; a coordinator no longer registered is
                    // ignored, or that stale value would win permanently.
                    guard var list = self.coordinators[target],
                          let i = list.firstIndex(where: { $0.coordinator === coordinator }) else { return }
                    list[i].state = armingState
                    self.coordinators[target] = list
                    self.setArming(Self.shownArming(list), for: target)
                }
            },
            onRegisterCoordinator: { [weak self] target, coordinator in
                Task { @MainActor in
                    guard let self else { return }
                    // A live coordinator is never evicted or refused: a second tab on the same app
                    // must not orphan the first, and the first must not starve the second (the
                    // one the app may be talking to) of hosts. A *stopped* one is the ordinary
                    // restart, whose deregistration is still in flight: drop it now.
                    var list = (self.coordinators[target] ?? []).filter { !$0.coordinator.isStopped }
                    if !list.contains(where: { $0.coordinator === coordinator }) {
                        list.append(Registered(coordinator: coordinator))
                    }
                    self.coordinators[target] = list
                    coordinator.updateHosts(self.routedHosts(for: target))
                }
            },
            onDeregisterCoordinator: { [weak self] target, coordinator in
                Task { @MainActor in
                    guard let self else { return }
                    // A restart reuses the target, so a late teardown removes only itself, never
                    // its replacement.
                    guard var list = self.coordinators[target] else { return }
                    list.removeAll { $0.coordinator === coordinator }
                    if list.isEmpty {
                        self.coordinators.removeValue(forKey: target)
                        self.state.armings.removeAll { $0.target == target }
                    } else {
                        self.coordinators[target] = list
                        self.setArming(Self.shownArming(list), for: target)
                    }
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

    /// The one arming state shown for a target with several coordinators: the most useful
    /// report, the newest on a tie. An armed capture outranks one still waiting, and a waiting
    /// one outranks a failure, so a second tab that couldn't claim the app doesn't hide the
    /// first tab's working state.
    private static func shownArming(_ list: [Registered]) -> InterceptArmingState {
        func rank(_ s: InterceptArmingState) -> Int {
            switch s {
            case .active: return 3
            case .waitingForAgent, .agentTooOld, .waitingForApp, .detached: return 2
            case .failed: return 1
            case .idle: return 0
            }
        }
        var best: InterceptArmingState = .idle
        for entry in list where rank(entry.state) >= rank(best) { best = entry.state }
        return best
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
        state.rules = OverrideGrouping.normalized(state.rules, groups: state.groups)
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
                                routedHosts: source.routedHosts, groupID: source.groupID)
        state.rules.append(copy)
        state.rules = OverrideGrouping.normalized(state.rules, groups: state.groups)
        persistAndRepublish()
    }

    /// Precedence is list order, so moving a rule up is how the user resolves shadowing. A rule
    /// moves only inside its own group; `setGroup` moves it to another.
    func move(_ id: UUID, by offset: Int) {
        syncFromDisk()
        guard let index = state.rules.firstIndex(where: { $0.id == id }),
              let target = OverrideGrouping.moveTarget(in: state.rules, groups: state.groups,
                                                       index: index, offset: offset) else { return }
        state.rules.swapAt(index, target)
        persistAndRepublish()
    }

    // MARK: - Groups

    /// Adds a group at the end of the list, or replaces the one with its id (a rename, or its
    /// switch).
    func saveGroup(_ group: OverrideGroup) {
        syncFromDisk()
        if let index = state.groups.firstIndex(where: { $0.id == group.id }) {
            state.groups[index] = group
        } else {
            state.groups.append(group)
        }
        state.rules = OverrideGrouping.normalized(state.rules, groups: state.groups)
        persistAndRepublish()
    }

    /// Deletes a group. Its rules stay, in no group.
    func removeGroup(_ id: UUID) {
        syncFromDisk()
        guard state.groups.contains(where: { $0.id == id }) else { return }
        state.groups.removeAll { $0.id == id }
        for index in state.rules.indices where state.rules[index].groupID == id {
            state.rules[index].groupID = nil
        }
        state.rules = OverrideGrouping.normalized(state.rules, groups: state.groups)
        persistAndRepublish()
    }

    /// Moves a group, and with it the precedence of all its rules.
    func moveGroup(_ id: UUID, by offset: Int) {
        syncFromDisk()
        guard let index = state.groups.firstIndex(where: { $0.id == id }) else { return }
        let (target, overflow) = index.addingReportingOverflow(offset)
        guard !overflow, state.groups.indices.contains(target) else { return }
        state.groups.swapAt(index, target)
        state.rules = OverrideGrouping.normalized(state.rules, groups: state.groups)
        persistAndRepublish()
    }

    /// Puts a rule at the end of a group, or (`groupID` nil, or a group that is gone) at the end
    /// of the rules in no group.
    func setGroup(_ groupID: UUID?, for id: UUID) {
        syncFromDisk()
        guard let index = state.rules.firstIndex(where: { $0.id == id }) else { return }
        var rule = state.rules.remove(at: index)
        rule.groupID = state.groups.contains { $0.id == groupID } ? groupID : nil
        state.rules.append(rule)
        state.rules = OverrideGrouping.normalized(state.rules, groups: state.groups)
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

    /// Modification dates of `rules.json` and `groups.json` when this engine last read or wrote
    /// them.
    private var diskStamp: [Date?] = []

    private static var diskModified: [Date?] {
        [OverrideRuleStore.rulesURL, OverrideRuleStore.groupsURL].map {
            try? FileManager.default.attributesOfItem(atPath: $0.path)[.modificationDate] as? Date
        }
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
        // Groups first: a rule that names a group not yet on disk would read as in no group.
        let groupsOK = OverrideRuleStore.saveGroups(state.groups)
        let ok = OverrideRuleStore.save(state.rules) && groupsOK
        JacaLog.info("override", "saved \(state.rules.count) rule(s), \(state.groups.count) group(s) -> \(ok ? "ok" : "FAILED")")
        republish()
    }

    /// Recompiles and pushes to the resolver and every armed transport — what makes an edit
    /// apply on the app's next request with no re-attach.
    private func republish() {
        resolver.publish(OverrideCompiler.compile(state.rules, masterEnabled: state.masterEnabled, groups: state.groups))
        for (target, list) in coordinators {
            let hosts = routedHosts(for: target)
            for entry in list { entry.coordinator.updateHosts(hosts) }
        }
    }
}
