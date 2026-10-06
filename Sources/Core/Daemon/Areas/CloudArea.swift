import Foundation

/// Cloud Logging in the daemon: one `CloudEngine` (auth, projects, templates, label keys) on the
/// retained `cloud.state` topic, and a `CloudStreamEngine` per tab with a replay buffer so a
/// relaunched app reattaches without re-querying gcloud.
enum CloudArea {
    static let stateTopic = "cloud.state"
    static func entriesTopic(_ id: UUID) -> String { "cloud.entries.\(id.uuidString)" }
    static func olderTopic(_ id: UUID) -> String { "cloud.older.\(id.uuidString)" }
    static func sessionStateTopic(_ id: UUID) -> String { "cloud.sstate.\(id.uuidString)" }

    // Registry params.
    struct QueryTemplateParams: Codable, Sendable { var name: String; var query: CloudLogQuery; var rawFilter: String? }
    struct SqlTemplateParams: Codable, Sendable { var name: String; var sql: String }
    struct TemplateIDParams: Codable, Sendable { var id: UUID }
    struct AddProjectParams: Codable, Sendable { var id: String; var displayName: String }
    struct ProjectIDParams: Codable, Sendable { var id: String }
    struct ProjectNameParams: Codable, Sendable { var id: String; var name: String }
    struct LogNameParams: Codable, Sendable { var id: String; var logName: String? }
    struct LogNamesParams: Codable, Sendable { var id: String; var names: [String] }
    struct LabelKeyParams: Codable, Sendable { var project: String; var logName: String; var key: String }
    struct LabelRulesParams: Codable, Sendable { var project: String; var logName: String; var rules: [String: LabelExampleRule] }

    // Session params.
    struct OpenParams: Codable, Sendable {
        var id: UUID
        var config: CloudStreamConfig
        var autoStart: Bool?
    }
    struct SessionIDParams: Codable, Sendable { var id: UUID }
    struct StartParams: Codable, Sendable { var id: UUID; var config: CloudStreamConfig }
    struct QueryParams: Codable, Sendable { var id: UUID; var sql: String }
    struct RangeParams: Codable, Sendable { var id: UUID; var afterSeq: UInt64?; var limit: Int? }
    struct SessionInfo: Codable, Sendable, Equatable {
        var id: UUID
        var config: CloudStreamConfig
        var state: CloudStreamState
        /// False when this open created the session (the daemon didn't have it).
        var existed: Bool
    }

    @MainActor
    final class Sessions {
        final class Hosted {
            let engine: CloudStreamEngine
            var config: CloudStreamConfig
            /// Every entry handed out (live and older pages), seq-ordered, bounded.
            var replay: [CloudLogEntry] = []
            var unwatchedSince: Date? = Date()
            init(engine: CloudStreamEngine, config: CloudStreamConfig) {
                self.engine = engine
                self.config = config
            }
        }

        private(set) var sessions: [UUID: Hosted] = [:]
        let bus: DaemonEventBus
        let replayCap: Int
        let orphanTimeout: TimeInterval
        private let makeEngine: (UUID) -> CloudStreamEngine

        init(bus: DaemonEventBus, replayCap: Int = 100_000, orphanTimeout: TimeInterval = 600,
             makeEngine: @escaping (UUID) -> CloudStreamEngine) {
            self.bus = bus
            self.replayCap = replayCap
            self.orphanTimeout = orphanTimeout
            self.makeEngine = makeEngine
        }

        var isBusy: Bool { sessions.values.contains { $0.engine.state.isRunning } }

        func open(_ p: OpenParams) -> SessionInfo {
            if let existing = sessions[p.id] {
                return SessionInfo(id: p.id, config: existing.config, state: existing.engine.state, existed: true)
            }
            let engine = makeEngine(p.id)
            let hosted = Hosted(engine: engine, config: p.config)
            sessions[p.id] = hosted
            let bus = self.bus, cap = replayCap, id = p.id
            engine.onEntries = { [weak hosted] batch in
                guard let hosted else { return }
                hosted.replay.append(contentsOf: batch)
                if hosted.replay.count > cap { hosted.replay.removeFirst(hosted.replay.count - cap) }
                bus.publish(CloudArea.entriesTopic(id), batch, droppable: true)
            }
            engine.onOlder = { [weak hosted] page in
                guard let hosted else { return }
                hosted.replay.insert(contentsOf: page, at: 0)
                if hosted.replay.count > cap { hosted.replay.removeLast(hosted.replay.count - cap) }
                bus.publish(CloudArea.olderTopic(id), page)
            }
            engine.onState = { bus.publish(CloudArea.sessionStateTopic(id), $0, retain: true) }
            bus.publish(CloudArea.sessionStateTopic(id), engine.state, retain: true)
            if p.autoStart == true { engine.start(p.config) }
            return SessionInfo(id: id, config: p.config, state: engine.state, existed: false)
        }

        func start(_ id: UUID, config: CloudStreamConfig) throws {
            let hosted = try session(id)
            hosted.config = config
            hosted.engine.start(config)
        }

        func close(_ id: UUID) -> Bool {
            guard let hosted = sessions.removeValue(forKey: id) else { return false }
            hosted.engine.dispose()
            bus.clearRetained(CloudArea.sessionStateTopic(id))
            return true
        }

        func range(_ p: RangeParams) -> [CloudLogEntry] {
            guard let hosted = sessions[p.id] else { return [] }
            let limit = max(1, min(p.limit ?? replayCap, replayCap))
            let slice = p.afterSeq.map { after in hosted.replay.filter { $0.seq > after } } ?? hosted.replay
            return Array(slice.suffix(limit))
        }

        func session(_ id: UUID) throws -> Hosted {
            guard let hosted = sessions[id] else { throw RPCError.failed("No cloud session \(id.uuidString).") }
            return hosted
        }

        /// Disposes sessions nobody has watched for `orphanTimeout`.
        func reapOrphans(now: Date = Date()) {
            for (id, hosted) in sessions {
                if bus.hasSubscribers(CloudArea.entriesTopic(id)) {
                    hosted.unwatchedSince = nil
                } else if let since = hosted.unwatchedSince {
                    if now.timeIntervalSince(since) >= orphanTimeout {
                        DaemonLog.info("closing cloud session \(id): unwatched for \(orphanTimeout)s")
                        _ = close(id)
                    }
                } else {
                    hosted.unwatchedSince = now
                }
            }
        }
    }

    @MainActor
    static func install(on server: DaemonServer, engine: CloudEngine? = nil, sessions: Sessions? = nil) {
        let engine = engine ?? CloudEngine()
        server.keep(engine)
        let bus = server.bus
        engine.onChange = { bus.publish(stateTopic, $0, retain: true) }
        bus.publish(stateTopic, engine.state, retain: true)

        let orphan = ProcessInfo.processInfo.environment["JACAD_LOG_ORPHAN_SECONDS"].flatMap(TimeInterval.init)
            .flatMap { $0.isFinite && $0 > 0 ? $0 : nil } ?? 600
        let sessions = sessions ?? Sessions(bus: bus, orphanTimeout: orphan) { [weak engine] id in
            CloudStreamEngine(
                id: id,
                cli: { engine?.cli },
                recordLabels: { keys, project, logName in engine?.recordLabelKeys(keys, project: project, logName: logName) },
                markUnauthenticated: { engine?.markUnauthenticated() })
        }
        server.keep(sessions)
        server.addBusyCheck("cloudLogging") { sessions.isBusy }
        server.keep(TaskBox(Task { @MainActor [weak sessions] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(min(60, max(1, sessions?.orphanTimeout ?? 60) / 4)))
                sessions?.reapOrphans()
            }
        }))

        let r = server.router
        // Registry.
        r.register("cloud.state", "gcloud/auth state, projects and templates (also published on cloud.state).") { (_: RPCEmpty, _) in
            await engine.state
        }
        r.register("cloud.reload", "Re-reads projects and templates from disk (the app edited them while the daemon was away).") { (_: RPCEmpty, _) in
            await engine.reload(); return RPCEmpty()
        }
        r.register("cloud.detect", "Re-detects gcloud and refreshes auth.") { (_: RPCEmpty, _) in
            await engine.detect(); return RPCEmpty()
        }
        r.register("cloud.refreshAuth", "Re-reads the active gcloud account.", concurrent: true) { (_: RPCEmpty, _) in
            await engine.refreshAuth(); return RPCEmpty()
        }
        r.register("cloud.markUnauthenticated", "Marks gcloud as signed out (a call reported an auth failure).") { (_: RPCEmpty, _) in
            await engine.markUnauthenticated(); return RPCEmpty()
        }
        r.register("cloud.addProject", "Validates a GCP project with gcloud and adds it. Returns {result, message?}.",
                   params: AddProjectParams.self, concurrent: true) { p, _ in await engine.addProject(id: p.id, displayName: p.displayName) }
        r.register("cloud.setDisplayName", "Renames a project.", params: ProjectNameParams.self) { p, _ in
            await engine.setDisplayName(p.name, for: p.id); return RPCEmpty()
        }
        r.register("cloud.removeProject", "Removes a project. Returns its title, or null.", params: ProjectIDParams.self) { p, _ in
            await engine.removeProject(p.id)
        }
        r.register("cloud.setSelectedLogName", "Sets a project's global log name (null = all logs).", params: LogNameParams.self) { p, _ in
            await engine.setSelectedLogName(p.logName, for: p.id); return RPCEmpty()
        }
        r.register("cloud.setLogNames", "Replaces a project's cached log names (a manually added one).",
                   params: LogNamesParams.self) { p, _ in
            await engine.setLogNames(p.names, for: p.id); return RPCEmpty()
        }
        r.register("cloud.refreshLogNames", "Lists a project's log names with gcloud. Returns an error message, or null.",
                   params: ProjectIDParams.self, concurrent: true) { p, _ in await engine.refreshLogNames(for: p.id) }
        r.register("cloud.toggleFavoriteLabel", "Pins/unpins a label key for a (project, log name).", params: LabelKeyParams.self) { p, _ in
            await engine.toggleFavoriteLabel(p.key, project: p.project, logName: p.logName); return RPCEmpty()
        }
        r.register("cloud.setLabelExampleRules", "Replaces the example-count rules for a (project, log name).",
                   params: LabelRulesParams.self) { p, _ in
            await engine.setLabelExampleRules(p.rules, project: p.project, logName: p.logName); return RPCEmpty()
        }
        r.register("cloud.saveQueryTemplate", "Saves a query template. False when the name is empty.",
                   params: QueryTemplateParams.self) { p, _ in
            await engine.saveQueryTemplate(name: p.name, query: p.query, rawFilter: p.rawFilter)
        }
        r.register("cloud.deleteQueryTemplate", "Deletes a query template.", params: TemplateIDParams.self) { p, _ in
            await engine.deleteQueryTemplate(p.id); return RPCEmpty()
        }
        r.register("cloud.saveSqlTemplate", "Saves a SQL template. False when the name is empty.", params: SqlTemplateParams.self) { p, _ in
            await engine.saveSqlTemplate(name: p.name, sql: p.sql)
        }
        r.register("cloud.deleteSqlTemplate", "Deletes a SQL template.", params: TemplateIDParams.self) { p, _ in
            await engine.deleteSqlTemplate(p.id); return RPCEmpty()
        }

        // Sessions.
        r.register("cloud.sessions.open", "Opens (or attaches to, by id) a Cloud Logging stream. Returns its info.",
                   params: OpenParams.self) { p, _ in await sessions.open(p) }
        r.register("cloud.sessions.start", "Starts polling with a (possibly new) query.", params: StartParams.self) { p, _ in
            try await sessions.start(p.id, config: p.config); return RPCEmpty()
        }
        r.register("cloud.sessions.stop", "Stops polling.", params: SessionIDParams.self) { p, _ in
            try await MainActor.run { try sessions.session(p.id).engine.stop() }; return RPCEmpty()
        }
        r.register("cloud.sessions.resetScrollback", "Forgets loaded ids and the older-page cursor (the viewer cleared).",
                   params: SessionIDParams.self) { p, _ in
            try await MainActor.run {
                let hosted = try sessions.session(p.id)
                hosted.engine.resetScrollback()
                hosted.replay.removeAll()
            }
            return RPCEmpty()
        }
        r.register("cloud.sessions.loadOlder", "Fetches the next page of older logs (arrives on cloud.older.<id>).",
                   params: SessionIDParams.self) { p, _ in
            try await MainActor.run { try sessions.session(p.id).engine.loadOlder() }; return RPCEmpty()
        }
        r.register("cloud.sessions.query", "Runs read-only SQL over the captured entries.", params: QueryParams.self, concurrent: true) { p, _ in
            guard DatabaseService.isReadOnly(p.sql) else {
                throw RPCError.invalidParams("Only read-only queries are allowed (SELECT / WITH / PRAGMA / EXPLAIN).")
            }
            let feed = try await MainActor.run { try sessions.session(p.id).engine }
            return try await feed.query(p.sql)
        }
        r.register("cloud.sessions.range", "Replayed entries after a seq (default: all, oldest first).",
                   params: RangeParams.self, concurrent: true) { p, _ in await sessions.range(p) }
        r.register("cloud.sessions.close", "Stops a session and deletes its database. Returns whether it existed.",
                   params: SessionIDParams.self) { p, _ in await sessions.close(p.id) }
        r.describeTopic(stateTopic, "gcloud/auth, projects and templates after every change. Data: CloudState.", retained: true)
        r.describeTopic("cloud.entries.<id>", "New entries (~30ms batches). Dropped for a slow client; backfill with cloud.sessions.range. Data: [CloudLogEntry].")
        r.describeTopic("cloud.older.<id>", "A page of older entries to prepend. Data: [CloudLogEntry].")
        r.describeTopic("cloud.sstate.<id>", "A session's stream state. Data: CloudStreamState.", retained: true)
    }
}
