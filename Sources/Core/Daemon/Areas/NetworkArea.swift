import Foundation

/// Network captures in the daemon (in-process agent capture; companion capture stays in the
/// app). Each capture keeps its transactions, with bodies beyond the newest `bodiesInMemory`
/// spilled to `NetworkBodyCache`. Clients get upserts **without bodies** on `net.txns.<id>` and
/// fetch a transaction's bodies with `network.body` when they show it.
enum NetworkArea {
    struct OpenParams: Codable, Sendable {
        var id: UUID
        var device: Device
        /// Restore a chosen source (without starting unless `autoStart`).
        var sourceID: String?
        var package: String?
        var autoStart: Bool?
    }
    struct SelectParams: Codable, Sendable { var id: UUID; var sourceID: String; var package: String? }
    struct IDParams: Codable, Sendable { var id: UUID }
    struct BodyParams: Codable, Sendable { var id: UUID; var transaction: UUID }
    struct Bodies: Codable, Sendable, Equatable { var request: Data?; var response: Data? }
    struct SessionInfo: Codable, Sendable, Equatable {
        var id: UUID
        var state: NetworkCaptureState
        var existed: Bool
    }

    static func transactionsTopic(_ id: UUID) -> String { "net.txns.\(id.uuidString)" }
    static func stateTopic(_ id: UUID) -> String { "net.state.\(id.uuidString)" }

    @MainActor
    final class Captures {
        final class Hosted {
            let engine: NetworkCaptureEngine
            let device: Device
            var transactions: [NetworkTransaction] = []
            var indexByID: [UUID: Int] = [:]
            /// Upserts not yet published, coalesced by id, in first-seen order.
            var outgoing: [UUID: NetworkTransaction] = [:]
            var outgoingOrder: [UUID] = []
            var unwatchedSince: Date? = Date()
            init(engine: NetworkCaptureEngine, device: Device) {
                self.engine = engine
                self.device = device
            }
        }

        private(set) var sessions: [UUID: Hosted] = [:]
        private var contexts: [String: DeviceContext] = [:]
        let bus: DaemonEventBus
        let bodyCache: NetworkBodyCache?
        let bodiesInMemory: Int
        let orphanTimeout: TimeInterval
        private var flushTask: Task<Void, Never>?
        /// Override services for a new capture source (nil when overrides are off).
        var interceptServices: () -> InterceptServices? = { nil }

        init(bus: DaemonEventBus, bodyCache: NetworkBodyCache? = NetworkBodyCache(),
             bodiesInMemory: Int = 1_000, orphanTimeout: TimeInterval = 600) {
            self.bus = bus
            self.bodyCache = bodyCache
            self.bodiesInMemory = bodiesInMemory
            self.orphanTimeout = orphanTimeout
        }

        var isBusy: Bool { sessions.values.contains { $0.engine.state.isRunning } }

        func open(_ p: OpenParams) -> SessionInfo {
            if let existing = sessions[p.id] {
                return SessionInfo(id: p.id, state: existing.engine.state, existed: true)
            }
            let adb = AndroidToolchain.adbURL(override: JacaDefaults.shared.string(forKey: DevicesEngine.adbPathKey))
            let context = context(for: p.device, adbURL: adb)
            let intercept = interceptServices
            let engine = NetworkCaptureEngine(id: p.id, device: p.device, adbURL: adb, ca: nil, companion: nil,
                                              deviceContext: { context }, interceptServices: { intercept() })
            let hosted = Hosted(engine: engine, device: p.device)
            sessions[p.id] = hosted
            let id = p.id, bus = self.bus
            engine.onTransactions = { [weak self, weak hosted] batch in
                guard let self, let hosted else { return }
                for txn in batch { self.store(txn, in: hosted) }
                self.scheduleFlush()
            }
            engine.onState = { bus.publish(NetworkArea.stateTopic(id), $0, retain: true) }
            engine.restoreMode(sourceID: p.sourceID, package: p.package)
            bus.publish(NetworkArea.stateTopic(id), engine.state, retain: true)
            if p.autoStart == true { engine.resume() }
            return SessionInfo(id: id, state: engine.state, existed: false)
        }

        private func context(for device: Device, adbURL: URL?) -> DeviceContext {
            if let existing = contexts[device.id] { return existing }
            let ctx = DeviceContext(device: device, adbURL: adbURL)
            contexts[device.id] = ctx
            ctx.start()
            return ctx
        }

        private func store(_ txn: NetworkTransaction, in hosted: Hosted) {
            if let i = hosted.indexByID[txn.id] {
                hosted.transactions[i] = txn
                // An update to a row already out of the in-memory window brings bodies back;
                // spill them again rather than holding them for the life of the capture.
                if i < hosted.transactions.count - bodiesInMemory { spill(at: i, in: hosted) }
            } else {
                hosted.indexByID[txn.id] = hosted.transactions.count
                hosted.transactions.append(txn)
                let evict = hosted.transactions.count - bodiesInMemory - 1
                if evict >= 0 { spill(at: evict, in: hosted) }
            }
            if hosted.outgoing.updateValue(txn, forKey: txn.id) == nil { hosted.outgoingOrder.append(txn.id) }
        }

        /// Moves an older transaction's bodies to the disk cache (same window as the app's list).
        private func spill(at index: Int, in hosted: Hosted) {
            let txn = hosted.transactions[index]
            guard !txn.bodiesEvicted, txn.requestBody != nil || txn.responseBody != nil, let cache = bodyCache else { return }
            let id = txn.id, req = txn.requestBody, resp = txn.responseBody
            Task { [weak hosted] in
                await cache.save(id, req: req, resp: resp)
                await MainActor.run {
                    guard let hosted, let i = hosted.indexByID[id] else { return }
                    // Only the bodies that were saved: an update that landed during the save
                    // brought new ones, which stay (the next update or window pass spills them).
                    guard hosted.transactions[i].requestBody == req,
                          hosted.transactions[i].responseBody == resp else { return }
                    hosted.transactions[i].requestBody = nil
                    hosted.transactions[i].responseBody = nil
                    hosted.transactions[i].bodiesEvicted = true
                }
            }
        }

        /// Publishes coalesced upserts every 50ms, bodies stripped.
        private func scheduleFlush() {
            guard flushTask == nil else { return }
            flushTask = Task { @MainActor [weak self] in
                try? await Task.sleep(for: .milliseconds(50))
                guard let self else { return }
                self.flushTask = nil
                for (id, hosted) in self.sessions where !hosted.outgoingOrder.isEmpty {
                    let batch = hosted.outgoingOrder.compactMap { hosted.outgoing[$0]?.strippingBodies() }
                    hosted.outgoing.removeAll(keepingCapacity: true)
                    hosted.outgoingOrder.removeAll(keepingCapacity: true)
                    self.bus.publish(NetworkArea.transactionsTopic(id), batch, droppable: true)
                }
            }
        }

        func transactions(_ id: UUID) -> [NetworkTransaction] {
            sessions[id]?.transactions.map { $0.strippingBodies() } ?? []
        }

        /// nil when the capture or the transaction is unknown (closed, cleared), so the client can
        /// tell "unavailable" from an empty body.
        func bodies(_ p: BodyParams) async -> Bodies? {
            guard let hosted = sessions[p.id], let i = hosted.indexByID[p.transaction] else { return nil }
            let txn = hosted.transactions[i]
            if txn.requestBody != nil || txn.responseBody != nil {
                return Bodies(request: txn.requestBody, response: txn.responseBody)
            }
            guard txn.bodiesEvicted, let cache = bodyCache else { return Bodies() }
            let loaded = await cache.load(p.transaction)
            return Bodies(request: loaded.req, response: loaded.resp)
        }

        /// HAR of everything captured. Bodies still in memory are included, spilled ones are
        /// not — the same as exporting from the app's list.
        func har(_ id: UUID) -> Data? {
            guard let hosted = sessions[id] else { return nil }
            return HARExport.data(from: hosted.transactions)
        }

        func clear(_ id: UUID) {
            guard let hosted = sessions[id] else { return }
            hosted.transactions.removeAll()
            hosted.indexByID.removeAll()
            hosted.outgoing.removeAll()          // queued upserts would bring cleared rows back
            hosted.outgoingOrder.removeAll()
        }

        func close(_ id: UUID) -> Bool {
            guard let hosted = sessions.removeValue(forKey: id) else { return false }
            hosted.engine.close()
            bus.clearRetained(NetworkArea.stateTopic(id))
            let deviceID = hosted.device.id
            if !sessions.values.contains(where: { $0.device.id == deviceID }), let ctx = contexts.removeValue(forKey: deviceID) {
                ctx.stop()
            }
            return true
        }

        func session(_ id: UUID) throws -> Hosted {
            guard let hosted = sessions[id] else { throw RPCError.failed("No network capture \(id.uuidString).") }
            return hosted
        }

        func reapOrphans(now: Date = Date()) {
            for (id, hosted) in sessions {
                if bus.hasSubscribers(NetworkArea.transactionsTopic(id)) {
                    hosted.unwatchedSince = nil
                } else if let since = hosted.unwatchedSince {
                    if now.timeIntervalSince(since) >= orphanTimeout {
                        DaemonLog.info("closing network capture \(id): unwatched for \(Int(orphanTimeout))s")
                        _ = close(id)
                    }
                } else {
                    hosted.unwatchedSince = now
                }
            }
        }
    }

    @MainActor
    static func install(on server: DaemonServer, captures: Captures? = nil,
                        interceptServices: @escaping () -> InterceptServices? = { nil }) {
        let orphan = ProcessInfo.processInfo.environment["JACAD_LOG_ORPHAN_SECONDS"].flatMap(TimeInterval.init) ?? 600
        let captures = captures ?? Captures(bus: server.bus, orphanTimeout: orphan)
        captures.interceptServices = interceptServices
        server.keep(captures)
        server.addBusyCheck("network") { captures.isBusy }
        server.keep(TaskBox(Task { @MainActor [weak captures] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(min(60, max(1, captures?.orphanTimeout ?? 60) / 4)))
                captures?.reapOrphans()
            }
        }))

        let r = server.router
        r.register("network.open", "Opens (or attaches to, by id) a network capture for a device.",
                   params: OpenParams.self) { p, _ in await captures.open(p) }
        r.register("network.select", "Chooses a capture source (agent) and starts it.", params: SelectParams.self) { p, _ in
            try await MainActor.run { try captures.session(p.id).engine.select(sourceID: p.sourceID, package: p.package) }
            return RPCEmpty()
        }
        r.register("network.reopenChooser", "Stops and returns to source selection.", params: IDParams.self) { p, _ in
            try await MainActor.run { try captures.session(p.id).engine.reopenChooser() }; return RPCEmpty()
        }
        r.register("network.resume", "Restarts the chosen source.", params: IDParams.self) { p, _ in
            try await MainActor.run { try captures.session(p.id).engine.resume() }; return RPCEmpty()
        }
        r.register("network.stop", "Stops capturing.", params: IDParams.self) { p, _ in
            try await MainActor.run { try captures.session(p.id).engine.stop() }; return RPCEmpty()
        }
        r.register("network.restartForInterceptChange", "Restarts the running source so it picks up changed override settings.",
                   params: IDParams.self) { p, _ in
            try await MainActor.run { try captures.session(p.id).engine.restartForInterceptChange() }; return RPCEmpty()
        }
        r.register("network.relaunchToAttach", "iOS Simulator: relaunches the app to put the agent back.",
                   params: IDParams.self) { p, _ in
            try await MainActor.run { try captures.session(p.id).engine.relaunchToAttach() }; return RPCEmpty()
        }
        r.register("network.clearProxyNeedsSetup", "Dismisses the proxy setup prompt.", params: IDParams.self) { p, _ in
            try await MainActor.run { try captures.session(p.id).engine.clearProxyNeedsSetup() }; return RPCEmpty()
        }
        r.register("network.transactions", "Every captured transaction, bodies omitted.", params: IDParams.self, concurrent: true) { p, _ in
            await captures.transactions(p.id)
        }
        r.register("network.body", "A transaction's request and response bodies (base64).", params: BodyParams.self, concurrent: true) { p, _ in
            await captures.bodies(p)
        }
        r.register("network.exportHAR", "The capture as HAR JSON (base64 data), or null.", params: IDParams.self, concurrent: true) { p, _ in
            await captures.har(p.id)
        }
        r.register("network.clear", "Forgets captured transactions.", params: IDParams.self) { p, _ in
            await captures.clear(p.id); return RPCEmpty()
        }
        r.register("network.close", "Stops and closes a capture. Returns whether it existed.", params: IDParams.self) { p, _ in
            await captures.close(p.id)
        }
        r.describeTopic("net.txns.<id>", "Coalesced transaction upserts (~50ms), bodies omitted. Dropped for a slow client; resync with network.transactions. Data: [NetworkTransaction].")
        r.describeTopic("net.state.<id>", "The capture's state. Data: NetworkCaptureState.", retained: true)
    }
}
