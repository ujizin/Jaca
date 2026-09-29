import Foundation

/// A log session running in `jacad`, as a `LogFeed` for a `LogSession` tab.
///
/// On every (re)connection it opens the session by id — attaching when it still exists (the
/// app relaunched), recreating it when the daemon restarted — then backfills from the daemon's
/// replay buffer and follows live `logs.lines.<id>` batches. Lines are deduplicated by seq, so
/// a backfill overlapping live batches never shows a line twice.
@MainActor
final class RemoteLogFeed: LogFeed {
    let id: UUID
    let device: Device
    private(set) var state: LogStreamState
    var onLines: (([LogLine]) -> Void)?
    var onState: ((LogStreamState) -> Void)?

    private let daemon: DaemonConnector
    private var displayName: String
    private var watchTask: Task<Void, Never>?
    private var lastSeq: UInt64?
    /// Whether the stream should run, so a session recreated after a daemon restart resumes.
    private var wantsRunning: Bool
    private var backfilling = false
    private let commands = DaemonCommandQueue()

    init(id: UUID, device: Device, package: String, displayName: String, autoStart: Bool,
         daemon: DaemonConnector) {
        self.id = id
        self.device = device
        self.daemon = daemon
        self.displayName = displayName
        self.wantsRunning = autoStart
        var initial = LogStreamState()
        initial.package = package
        state = initial
        watch()
    }

    private func watch() {
        // `events.dropped` notices for the lines topic arrive on this same stream.
        let topics = [LogsArea.linesTopic(id), LogsArea.stateTopic(id)]
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
        case LogsArea.linesTopic(id):
            guard let batch = try? event.decode([LogLine].self) else { return }
            deliver(batch)
        case LogsArea.stateTopic(id):
            guard let next = try? event.decode(LogStreamState.self) else { return }
            wantsRunning = next.isRunning || next.isConnecting
            setState(next)
        case "events.dropped":
            // This client fell behind and missed batches: fetch them from the replay buffer.
            Task { await backfill() }
        default:
            break
        }
    }

    private func setState(_ next: LogStreamState) {
        guard next != state else { return }
        state = next
        onState?(next)
    }

    private func deliver(_ batch: [LogLine]) {
        let fresh = lastSeq.map { last in batch.filter { $0.seq > last } } ?? batch
        guard let last = fresh.last else { return }
        lastSeq = last.seq
        onLines?(fresh)
    }

    /// Opens (or reattaches to) the session, then backfills what this tab hasn't seen.
    private func openAndBackfill() async {
        // After a daemon restart the session is new: continue our seqs past the last one seen.
        let seqStart = lastSeq.map { ($0 / 8 + 1) * 8 } ?? 0
        let params = LogsArea.OpenParams(id: id, device: device, package: state.package,
                                         displayName: displayName, autoStart: wantsRunning, seqStart: seqStart)
        guard let info = await daemon.call("logs.open", params, as: LogsArea.SessionInfo.self) else { return }
        wantsRunning = info.state.isRunning || info.state.isConnecting || wantsRunning
        setState(info.state)
        await backfill()
    }

    private func backfill() async {
        guard !backfilling else { return }
        backfilling = true
        defer { backfilling = false }
        let params = LogsArea.RangeParams(id: id, afterSeq: lastSeq, limit: nil)
        guard let lines = await daemon.call("logs.range", params, as: [LogLine].self) else { return }
        deliver(lines)
    }

    /// The daemon went away: the stream isn't running from this tab's point of view.
    private func lostDaemon() {
        var next = state
        next.isRunning = false
        next.isConnecting = false
        setState(next)
    }

    // MARK: - LogFeed

    func start() {
        wantsRunning = true
        send("logs.start")
    }

    func stop() {
        wantsRunning = false
        send("logs.stop")
    }

    func connect() {
        wantsRunning = true
        send("logs.connect")
    }

    func setPackage(_ package: String) {
        var next = state
        next.package = package
        setState(next)
        let params = LogsArea.PackageParams(id: id, package: package)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call("logs.setPackage", params) }
    }

    func clearStatus() { send("logs.clearStatus") }
    func resetBodyPairing() { send("logs.resetPairing") }
    func clearDeviceBuffer() { send("logs.clearDeviceBuffer") }

    func close() {
        watchTask?.cancel()
        watchTask = nil
        onLines = nil
        onState = nil
        let params = LogsArea.IDParams(id: id)
        commands.enqueue { [daemon] in let _: Bool? = await daemon.call("logs.close", params) }
    }

    func rename(_ name: String) { displayName = name }

    private func send(_ method: String) {
        let params = LogsArea.IDParams(id: id)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call(method, params) }
    }
}
