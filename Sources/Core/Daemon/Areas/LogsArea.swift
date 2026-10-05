import Foundation

/// Device log sessions running in the daemon. Each session is a `LogStreamEngine` plus a
/// replay buffer, so a client that (re)attaches — the app after a relaunch, a Herdr pane opened
/// later, a client that fell behind — can backfill with `logs.range` before following live
/// `logs.lines.<id>` batches.
///
/// A session nobody watches (no subscriber on its lines topic) is stopped and closed after
/// `orphanTimeout`, so a tab left open in a quit app doesn't stream forever.
enum LogsArea {
    struct OpenParams: Codable, Sendable {
        /// Reuse this id: attach when the session exists, create it with this id otherwise.
        var id: UUID?
        var device: Device
        var package: String?
        var displayName: String?
        var autoStart: Bool?
        /// First seq for a new session's lines, so a client recreating a session after a
        /// daemon restart keeps its seqs increasing.
        var seqStart: UInt64?
    }
    struct IDParams: Codable, Sendable { var id: UUID }
    struct PackageParams: Codable, Sendable { var id: UUID; var package: String }
    struct RangeParams: Codable, Sendable {
        var id: UUID
        /// Lines with a seq above this; nil = from the start of the replay buffer.
        var afterSeq: UInt64?
        var limit: Int?
    }
    struct SessionInfo: Codable, Sendable, Equatable {
        var id: UUID
        var device: Device
        var displayName: String
        var state: LogStreamState
        var lastSeq: UInt64?
    }

    static func linesTopic(_ id: UUID) -> String { "logs.lines.\(id.uuidString)" }
    static func stateTopic(_ id: UUID) -> String { "logs.state.\(id.uuidString)" }

    @MainActor
    final class Registry {
        final class Hosted {
            let engine: LogStreamEngine
            var displayName: String
            var replay: [LogLine] = []
            var unwatchedSince: Date? = Date()
            init(engine: LogStreamEngine, displayName: String) {
                self.engine = engine
                self.displayName = displayName
            }
        }

        private(set) var sessions: [UUID: Hosted] = [:]
        let bus: DaemonEventBus
        let history: HistoryStore?
        let replayCap: Int
        let orphanTimeout: TimeInterval
        private let makeEngine: (UUID, Device, String, UInt64) -> LogStreamEngine

        init(bus: DaemonEventBus, history: HistoryStore?, replayCap: Int = 100_000,
             orphanTimeout: TimeInterval = 600,
             makeEngine: ((UUID, Device, String, UInt64) -> LogStreamEngine)? = nil) {
            self.bus = bus
            self.history = history
            self.replayCap = replayCap
            self.orphanTimeout = orphanTimeout
            self.makeEngine = makeEngine ?? { id, device, package, seqStart in
                let adb = AndroidToolchain.adbURL(override: JacaDefaults.shared.string(forKey: DevicesEngine.adbPathKey))
                let store = history
                return LogStreamEngine(
                    id: id, device: device, adbURL: adb, package: package, seqStart: seqStart,
                    onPersist: { sid, lines in Task { await store?.appendLines(sessionID: sid, lines) } })
            }
        }

        var isBusy: Bool { sessions.values.contains { $0.engine.state.isRunning } }

        func info(_ hosted: Hosted) -> SessionInfo {
            SessionInfo(id: hosted.engine.id, device: hosted.engine.device, displayName: hosted.displayName,
                        state: hosted.engine.state, lastSeq: hosted.replay.last?.seq)
        }

        func open(_ p: OpenParams) throws -> SessionInfo {
            if let id = p.id, let existing = sessions[id] {
                if let name = p.displayName { existing.displayName = name }
                return info(existing)
            }
            let adb = AndroidToolchain.adbURL(override: JacaDefaults.shared.string(forKey: DevicesEngine.adbPathKey))
            guard LogSources.isSupported(p.device, adbURL: adb) else {
                throw RPCError.failed("No log source for \(p.device.displayModel) (adb not found).")
            }
            let id = p.id ?? UUID()
            let engine = makeEngine(id, p.device, p.package ?? "", p.seqStart ?? 0)
            let hosted = Hosted(engine: engine, displayName: p.displayName ?? p.device.displayModel)
            sessions[id] = hosted

            let linesTopic = LogsArea.linesTopic(id), stateTopic = LogsArea.stateTopic(id)
            let bus = self.bus, cap = replayCap
            engine.onLines = { [weak hosted] batch in
                guard let hosted else { return }
                hosted.replay.append(contentsOf: batch)
                if hosted.replay.count > cap { hosted.replay.removeFirst(hosted.replay.count - cap) }
                bus.publish(linesTopic, batch, droppable: true)
            }
            engine.onState = { bus.publish(stateTopic, $0, retain: true) }
            let store = history, device = p.device
            engine.onStarted = { [weak hosted, weak engine] in
                guard let hosted, let engine else { return }
                let pkg = engine.state.package, name = hosted.displayName
                Task {
                    await store?.upsertDevice(device)
                    await store?.beginSession(id: id, device: device, package: pkg, displayName: name)
                }
            }
            bus.publish(stateTopic, engine.state, retain: true)
            if p.autoStart == true { engine.start() }
            return info(hosted)
        }

        func close(_ id: UUID) -> Bool {
            guard let hosted = sessions.removeValue(forKey: id) else { return false }
            hosted.engine.close()
            bus.clearRetained(LogsArea.stateTopic(id))
            let store = history
            Task { await store?.endSession(id: id) }
            return true
        }

        func range(_ p: RangeParams) -> [LogLine] {
            guard let hosted = sessions[p.id] else { return [] }
            let limit = max(1, min(p.limit ?? 50_000, replayCap))
            let slice: ArraySlice<LogLine>
            if let after = p.afterSeq {
                // Replay is seq-ordered: binary search for the first line past `after`.
                var lo = 0, hi = hosted.replay.count
                while lo < hi {
                    let mid = (lo + hi) / 2
                    if hosted.replay[mid].seq <= after { lo = mid + 1 } else { hi = mid }
                }
                slice = hosted.replay[lo...]
            } else {
                slice = hosted.replay[...]
            }
            return Array(slice.suffix(limit))
        }

        /// Stops and closes sessions nobody has watched for `orphanTimeout`.
        func reapOrphans(now: Date = Date()) {
            for (id, hosted) in sessions {
                if bus.hasSubscribers(LogsArea.linesTopic(id)) {
                    hosted.unwatchedSince = nil
                } else if let since = hosted.unwatchedSince {
                    if now.timeIntervalSince(since) >= orphanTimeout {
                        DaemonLog.info("closing log session \(id): unwatched for \(Int(orphanTimeout))s")
                        _ = close(id)
                    }
                } else {
                    hosted.unwatchedSince = now
                }
            }
        }

        func withSession<R>(_ id: UUID, _ body: @MainActor (LogStreamEngine) -> R) throws -> R {
            guard let hosted = sessions[id] else { throw RPCError.failed("No log session \(id.uuidString).") }
            return body(hosted.engine)
        }
    }

    @MainActor
    static func install(on server: DaemonServer, registry: Registry? = nil) {
        let orphan = ProcessInfo.processInfo.environment["JACAD_LOG_ORPHAN_SECONDS"].flatMap(TimeInterval.init) ?? 600
        let registry = registry ?? Registry(bus: server.bus, history: HistoryStore(), orphanTimeout: orphan)
        server.keep(registry)
        server.addBusyCheck("logs") { registry.isBusy }
        let reaper = Task { @MainActor [weak registry] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(min(60, max(1, registry?.orphanTimeout ?? 60) / 4)))
                registry?.reapOrphans()
            }
        }
        server.keep(TaskBox(reaper))

        let r = server.router
        r.register("logs.open", "Opens (or attaches to, by id) a device log session. Returns its info.",
                   params: OpenParams.self) { p, _ in try await registry.open(p) }
        r.register("logs.list", "Every open log session.") { (_: RPCEmpty, _) in
            await MainActor.run {
                registry.sessions.values.map { registry.info($0) }.sorted { $0.id.uuidString < $1.id.uuidString }
            }
        }
        r.register("logs.start", "Starts streaming.", params: IDParams.self) { p, _ in
            try await registry.withSession(p.id) { $0.start() }; return RPCEmpty()
        }
        r.register("logs.connect", "Checks the device, then starts streaming (a status message explains a failure).",
                   params: IDParams.self) { p, _ in
            try await registry.withSession(p.id) { $0.connect() }; return RPCEmpty()
        }
        r.register("logs.stop", "Stops streaming (the session stays open).", params: IDParams.self) { p, _ in
            try await registry.withSession(p.id) { $0.stop() }; return RPCEmpty()
        }
        r.register("logs.setPackage", "Targets an app (PID tracking, simulator stdout, iOS scoping). Empty = whole device.",
                   params: PackageParams.self) { p, _ in
            try await registry.withSession(p.id) { $0.setPackage(p.package) }; return RPCEmpty()
        }
        r.register("logs.clearStatus", "Dismisses the session's status message.", params: IDParams.self) { p, _ in
            try await registry.withSession(p.id) { $0.clearStatus() }; return RPCEmpty()
        }
        r.register("logs.resetPairing", "Forgets a half-seen response body (the viewer cleared its scrollback).",
                   params: IDParams.self) { p, _ in
            try await registry.withSession(p.id) { $0.resetBodyPairing() }; return RPCEmpty()
        }
        r.register("logs.clearDeviceBuffer", "Clears the device's logcat buffer (Android).", params: IDParams.self) { p, _ in
            try await registry.withSession(p.id) { $0.clearDeviceBuffer() }; return RPCEmpty()
        }
        r.register("logs.range", "Replayed lines after a seq (default: from the start of the replay buffer), up to limit.",
                   params: RangeParams.self, concurrent: true) { p, _ in await registry.range(p) }
        r.register("logs.close", "Stops and closes a session. Returns whether it existed.", params: IDParams.self) { p, _ in
            await registry.close(p.id)
        }
        r.describeTopic("logs.lines.<id>", "Batches of processed lines (~30ms). Dropped for a slow client; backfill with logs.range. Data: [LogLine].")
        r.describeTopic("logs.state.<id>", "The session's stream state after every change. Data: LogStreamState.", retained: true)
    }
}

/// Keeps a background task alive (and cancels it) with the server.
final class TaskBox {
    let task: Task<Void, Never>
    init(_ task: Task<Void, Never>) { self.task = task }
    deinit { task.cancel() }
}
