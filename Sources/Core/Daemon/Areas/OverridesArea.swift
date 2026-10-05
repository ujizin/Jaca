import Foundation

/// Response overrides in the daemon. The engine lives here because the captures it arms run
/// here; its state (rules, master switch, hit counts, arming) is retained on `overrides.state`.
enum OverridesArea {
    struct RuleParams: Codable, Sendable { var rule: OverrideRule }
    struct IDParams: Codable, Sendable { var id: UUID }
    struct EnabledParams: Codable, Sendable { var id: UUID; var enabled: Bool }
    struct MoveParams: Codable, Sendable { var id: UUID; var offset: Int }
    struct MasterParams: Codable, Sendable { var enabled: Bool }

    static let stateTopic = "overrides.state"

    /// Installs the area and returns its engine, whose services the daemon's captures use.
    @MainActor
    @discardableResult
    static func install(on server: DaemonServer, engine: OverridesEngine? = nil) -> OverridesEngine {
        let engine = engine ?? OverridesEngine()
        server.keep(engine)
        let bus = server.bus
        engine.onChange = { bus.publish(stateTopic, $0, retain: true) }
        // Tunnels left by a daemon (or app) that died without cleaning up.
        engine.reconcileOrphanedTunnels()
        bus.publish(stateTopic, engine.state, retain: true)

        let r = server.router
        r.register("overrides.state", "The override library and runtime state (also on overrides.state).") { (_: RPCEmpty, _) in
            await engine.state
        }
        r.register("overrides.reload", "Re-reads the rule library from disk (the app edited it in-process).") { (_: RPCEmpty, _) in
            await engine.reload(); return RPCEmpty()
        }
        r.register("overrides.save", "Adds or replaces a rule (by id).", params: RuleParams.self) { p, _ in
            await engine.save(p.rule); return RPCEmpty()
        }
        r.register("overrides.remove", "Deletes a rule.", params: IDParams.self) { p, _ in
            await engine.remove(p.id); return RPCEmpty()
        }
        r.register("overrides.setEnabled", "Turns one rule on or off.", params: EnabledParams.self) { p, _ in
            await engine.setEnabled(p.enabled, for: p.id); return RPCEmpty()
        }
        r.register("overrides.duplicate", "Copies a rule to the end of the list.", params: IDParams.self) { p, _ in
            await engine.duplicate(p.id); return RPCEmpty()
        }
        r.register("overrides.move", "Moves a rule by an offset in the precedence order.", params: MoveParams.self) { p, _ in
            await engine.move(p.id, by: p.offset); return RPCEmpty()
        }
        r.register("overrides.setMaster", "The master switch: apply every rule, or none.", params: MasterParams.self) { p, _ in
            await engine.setMasterEnabled(p.enabled); return RPCEmpty()
        }
        r.describeTopic(stateTopic, "Rules, master switch, hit counts and per-target arming after every change. Data: OverridesState.",
                        retained: true)
        return engine
    }
}
