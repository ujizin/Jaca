import Foundation

/// Response overrides in the daemon. The engine lives here because the captures it arms run
/// here; its state (rules, master switch, hit counts, arming) is retained on `overrides.state`.
enum OverridesArea {
    struct RuleParams: Codable, Sendable { var rule: OverrideRule }
    struct IDParams: Codable, Sendable { var id: UUID }
    struct EnabledParams: Codable, Sendable { var id: UUID; var enabled: Bool }
    struct MoveParams: Codable, Sendable { var id: UUID; var offset: Int }
    struct MasterParams: Codable, Sendable { var enabled: Bool }
    struct FromTransactionParams: Codable, Sendable {
        /// The capture, and the transaction in it.
        var id: UUID
        var transaction: UUID
        /// Each replaces what the captured response would seed.
        var name: String?
        var statusCode: Int?
        /// The response body's bytes (base64 on the wire, like `network.body`).
        var body: Data?
        /// Absent: enabled, like every new rule.
        var enabled: Bool?
    }
    struct Created: Codable, Sendable, Equatable {
        var rule: OverrideRule
        /// Why the rule may not reproduce the captured response (`OverrideSeeding.warning`).
        var warning: String?
    }

    static let stateTopic = "overrides.state"

    /// Installs the area and returns its engine, whose services the daemon's captures use.
    @MainActor
    @discardableResult
    static func install(on server: DaemonServer, engine: OverridesEngine? = nil,
                        captures: NetworkArea.Captures? = nil) -> OverridesEngine {
        let engine = engine ?? OverridesEngine()
        server.keep(engine)
        let bus = server.bus
        engine.onChange = { bus.publish(stateTopic, $0, retain: true) }
        // Tunnels left by a daemon (or app) that died without cleaning up.
        engine.reconcileOrphanedTunnels()
        bus.publish(stateTopic, engine.state, retain: true)

        let r = server.router
        r.register("overrides.state", "The override library and runtime state (also on overrides.state).",
                   takes: .empty, returns: .overridesState) { (_: RPCEmpty, _) in
            await engine.state
        }
        r.register("overrides.reload", "Re-reads the rule library from disk (the app edited it in-process).",
                   takes: .empty, returns: .empty) { (_: RPCEmpty, _) in
            await engine.reload(); return RPCEmpty()
        }
        r.register("overrides.save", "Adds or replaces a rule (by id).", params: RuleParams.self,
                   takes: .object(["rule": .overrideRule]), returns: .empty) { p, _ in
            await engine.save(p.rule); return RPCEmpty()
        }
        r.register("overrides.remove", "Deletes a rule.", params: IDParams.self,
                   takes: .object(["id": .uuid]), returns: .empty) { p, _ in
            await engine.remove(p.id); return RPCEmpty()
        }
        r.register("overrides.setEnabled", "Turns one rule on or off.", params: EnabledParams.self,
                   takes: .object(["id": .uuid, "enabled": .boolean]), returns: .empty) { p, _ in
            await engine.setEnabled(p.enabled, for: p.id); return RPCEmpty()
        }
        r.register("overrides.duplicate", "Copies a rule to the end of the list.", params: IDParams.self,
                   takes: .object(["id": .uuid]), returns: .empty) { p, _ in
            await engine.duplicate(p.id); return RPCEmpty()
        }
        r.register("overrides.move", "Moves a rule by an offset in the precedence order.", params: MoveParams.self,
                   takes: .object(["id": .uuid, "offset": .integer]), returns: .empty) { p, _ in
            await engine.move(p.id, by: p.offset); return RPCEmpty()
        }
        r.register("overrides.setMaster", "The master switch: apply every rule, or none.", params: MasterParams.self,
                   takes: .object(["enabled": .boolean]), returns: .empty) { p, _ in
            await engine.setMasterEnabled(p.enabled); return RPCEmpty()
        }
        if let captures {
            r.register("overrides.createFromTransaction", "", params: FromTransactionParams.self,
                       takes: .object(["id": .uuid, "transaction": .uuid],
                                      optional: ["name": .string, "statusCode": .integer, "body": .bytes, "enabled": .boolean]),
                       returns: .nullable(.object(["rule": .overrideRule], optional: ["warning": .string]))) { p, _ in
                await create(p, engine: engine, captures: captures)
            }
        }
        r.describeTopic(stateTopic, "Rules, master switch, hit counts and per-target arming after every change. Data: OverridesState.",
                        retained: true)
        return engine
    }

    /// Seeds a rule from a captured transaction, as the app's "create override" does, and saves
    /// it. nil when the capture or the transaction is unknown (closed, cleared).
    @MainActor
    static func create(_ p: FromTransactionParams, engine: OverridesEngine, captures: NetworkArea.Captures) async -> Created? {
        guard let txn = await captures.transaction(.init(id: p.id, transaction: p.transaction)) else { return nil }
        var rule = OverrideSeeding.rule(for: txn, responseBody: txn.responseBody ?? Data())
        if let name = p.name { rule.name = name }
        if let enabled = p.enabled { rule.enabled = enabled }
        if case .respond(var spec) = rule.action {
            if let statusCode = p.statusCode { spec.statusCode = statusCode }
            if let body = p.body { spec.body = OverrideRuleStore.makeBodyRef(body) }
            rule.action = .respond(spec)
        }
        engine.save(rule)
        // As the engine kept it.
        let saved = engine.state.rules.first { $0.id == rule.id } ?? rule
        // A replaced body is the caller's own, so the captured one's limits no longer apply.
        return Created(rule: saved, warning: p.body == nil ? OverrideSeeding.warning(for: txn) : nil)
    }
}

extension OverridesArea.Created {
    private enum CodingKeys: String, CodingKey { case rule, warning }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(rule: try c.decode(OverrideRule.self, forKey: .rule),
                  warning: try? c.decodeIfPresent(String.self, forKey: .warning))
    }
}
