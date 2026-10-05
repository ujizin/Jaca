import Foundation

/// A Cloud Logging stream running in `jacad`, as a `CloudFeed` for a `CloudLogSession` tab.
///
/// Same shape as `RemoteLogFeed`: on every (re)connection it opens the session by id (attaching,
/// or recreating it after a daemon restart), backfills from the daemon's replay buffer, and
/// follows live entries, older pages and state. Commands go through one serial queue.
@MainActor
final class RemoteCloudFeed: CloudFeed {
    let id: UUID
    private(set) var state = CloudStreamState()
    var onEntries: (([CloudLogEntry]) -> Void)?
    var onOlder: (([CloudLogEntry]) -> Void)?
    var onState: ((CloudStreamState) -> Void)?
    var onReset: (() -> Void)?

    private let daemon: DaemonConnector
    private var config: CloudStreamConfig
    private var wantsRunning: Bool
    private var watchTask: Task<Void, Never>?
    private let commands = DaemonCommandQueue()
    /// The newest live seq delivered; live batches and backfills below it are duplicates.
    private var lastSeq: UInt64?
    /// Whether this tab has received anything from the current daemon session.
    private var hasHistory = false
    private var backfilling = false
    /// Live batches held back while a gap (`events.dropped`) is being filled, so `lastSeq` can't
    /// move past the gap before its entries arrive. nil when not filling.
    private var held: [[CloudLogEntry]]?
    /// Another drop arrived while filling: fill again before releasing `held`.
    private var gapAgain = false

    init(id: UUID, config: CloudStreamConfig, autoStart: Bool, daemon: DaemonConnector) {
        self.id = id
        self.config = config
        self.wantsRunning = autoStart
        self.daemon = daemon
        watch()
    }

    private func watch() {
        let topics = [CloudArea.entriesTopic(id), CloudArea.olderTopic(id), CloudArea.sessionStateTopic(id)]
        watchTask = daemon.watch(
            topics,
            onUnavailable: { [weak self] in self?.lostDaemon() },
            onSubscribed: { [weak self] in await self?.openAndBackfill() }
        ) { [weak self] event in
            self?.handle(event)
        }
    }

    private func handle(_ event: DaemonEventLine) {
        switch event.topic {
        case CloudArea.entriesTopic(id):
            guard let batch = try? event.decode([CloudLogEntry].self) else { return }
            if held != nil { held?.append(batch) } else { deliver(batch) }
        case CloudArea.olderTopic(id):
            guard let page = try? event.decode([CloudLogEntry].self), !page.isEmpty else { return }
            onOlder?(page)
        case CloudArea.sessionStateTopic(id):
            guard let next = try? event.decode(CloudStreamState.self) else { return }
            wantsRunning = next.isRunning
            setState(next)
        case "events.dropped":
            if held == nil {
                held = []
                Task { await fillGap() }
            } else {
                gapAgain = true
            }
        default:
            break
        }
    }

    private func setState(_ next: CloudStreamState) {
        guard next != state else { return }
        state = next
        onState?(next)
    }

    private func deliver(_ batch: [CloudLogEntry]) {
        let fresh = lastSeq.map { last in batch.filter { $0.seq > last } } ?? batch
        guard let last = fresh.last else { return }
        lastSeq = last.seq
        hasHistory = true
        onEntries?(fresh)
    }

    private func openAndBackfill() async {
        let params = CloudArea.OpenParams(id: id, config: config, autoStart: wantsRunning)
        guard let info = await daemon.call("cloud.sessions.open", params, as: CloudArea.SessionInfo.self) else { return }
        if !info.existed && hasHistory {
            // The daemon restarted and this is a fresh session: its seqs start over.
            lastSeq = nil
            hasHistory = false
            onReset?()
        }
        setState(info.state)
        await backfill()
    }

    /// Everything the daemon replays that this tab hasn't seen. The first backfill after an
    /// attach includes older pages (lower seqs), so they arrive as one ordered batch.
    private func backfill() async {
        guard !backfilling else { return }
        backfilling = true
        defer { backfilling = false }
        let params = CloudArea.RangeParams(id: id, afterSeq: lastSeq, limit: nil)
        guard let entries = await daemon.call("cloud.sessions.range", params, as: [CloudLogEntry].self) else { return }
        deliver(entries)
    }

    /// Fetches everything after the last entry delivered, then releases the batches held
    /// meanwhile (deduplicated by seq, so overlap with the fetched entries is harmless).
    private func fillGap() async {
        repeat {
            gapAgain = false
            let params = CloudArea.RangeParams(id: id, afterSeq: lastSeq, limit: nil)
            if let entries = await daemon.call("cloud.sessions.range", params, as: [CloudLogEntry].self) {
                deliver(entries)
            }
        } while gapAgain
        let release = held ?? []
        held = nil
        release.forEach(deliver)
    }

    private func lostDaemon() {
        var next = state
        next.isRunning = false
        next.isLoading = false
        setState(next)
    }

    // MARK: - CloudFeed

    func start(_ config: CloudStreamConfig) {
        self.config = config
        wantsRunning = true
        let params = CloudArea.StartParams(id: id, config: config)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call("cloud.sessions.start", params) }
    }

    func stop() {
        wantsRunning = false
        send("cloud.sessions.stop")
    }

    /// Keeps `lastSeq`: live seqs keep rising across a reset, and a backfill racing the reset
    /// must not hand back the entries the viewer just cleared.
    func resetScrollback() {
        send("cloud.sessions.resetScrollback")
    }

    func loadOlder() { send("cloud.sessions.loadOlder") }

    func query(_ sql: String) async throws -> DBResultSet {
        guard let result = try await daemon.request("cloud.sessions.query", CloudArea.QueryParams(id: id, sql: sql),
                                                    as: DBResultSet.self) else {
            throw DBError.sqlite(RPCError.disconnected.message)
        }
        return result
    }

    func dispose() {
        watchTask?.cancel()
        watchTask = nil
        onEntries = nil
        onOlder = nil
        onState = nil
        onReset = nil
        send("cloud.sessions.close")
    }

    private func send(_ method: String) {
        let params = CloudArea.SessionIDParams(id: id)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call(method, params) }
    }
}
