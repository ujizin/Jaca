import Foundation

/// A log session running in `jacad`, as a `LogFeed` for a `LogSession` tab.
///
/// On every (re)connection it opens the session by id, attaching when it still exists (the
/// app relaunched) or recreating it when the daemon restarted, then backfills from the daemon's
/// replay buffer and follows live `logs.lines.<id>` batches. Lines are deduplicated by seq, so
/// a backfill overlapping live batches never shows a line twice.
///
/// When the daemon can't be reached while the tab wants to stream, the tab switches to an
/// in-process `LogStreamEngine` for the rest of its life, so pressing play never silently does
/// nothing.
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
    /// Live batches held back while a gap (`events.dropped`) is being filled, so `lastSeq` can't
    /// move past the gap before its lines arrive. nil when not filling.
    private var held: [[LogLine]]?
    /// Another drop arrived while filling: fill again before releasing `held`.
    private var gapAgain = false
    /// The in-process stream this tab switched to when the daemon was unreachable.
    private var fallback: LogStreamEngine?
    private let makeLocal: (_ seqStart: UInt64, _ displayName: @escaping @MainActor () -> String) -> LogStreamEngine

    init(id: UUID, device: Device, package: String, displayName: String, autoStart: Bool,
         daemon: DaemonConnector,
         makeLocal: @escaping (_ seqStart: UInt64, _ displayName: @escaping @MainActor () -> String) -> LogStreamEngine) {
        self.id = id
        self.device = device
        self.daemon = daemon
        self.makeLocal = makeLocal
        self.displayName = displayName
        self.wantsRunning = autoStart
        var initial = LogStreamState()
        initial.package = package
        state = initial
        watch()
    }

    private func watch() {
        // `events.dropped` notices for both topics arrive on this same stream.
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
            if held != nil { held?.append(batch) } else { deliver(batch) }
        case LogsArea.stateTopic(id):
            guard let next = try? event.decode(LogStreamState.self) else { return }
            wantsRunning = next.isRunning || next.isConnecting
            setState(next)
        case "events.dropped":
            let lost = (try? event.decode(DroppedEvents.self))?.topic
            // A lost state is fetched again (reopening returns it).
            if lost == LogsArea.stateTopic(id) { Task { await openAndBackfill() }; return }
            // This client fell behind and missed batches. The notice arrives right before the
            // first batch after the gap, so hold live batches until the gap is filled.
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
        let seqStart = SeqCounter.slot(after: lastSeq)
        let params = LogsArea.OpenParams(id: id, device: device, package: state.package,
                                         displayName: displayName, autoStart: wantsRunning, seqStart: seqStart)
        let info: LogsArea.SessionInfo
        do {
            guard let opened = try await daemon.request("logs.open", params, as: LogsArea.SessionInfo.self) else { return }
            info = opened
        } catch let error as RPCError where error.code == RPCError.disconnectedCode {
            return   // the connection dropped: the watch resubscribes and opens again
        } catch {
            // The daemon answered but couldn't open it (say, it resolves no adb): a tab that
            // wants to stream does so in-process, where the engine reports why it can't.
            DaemonLog.error("logs.open: \(error.localizedDescription)")
            if wantsRunning, fallback == nil { useFallback().connect() }
            return
        }
        // The daemon's run state wins for a session it already had (a dropped "stopped" state
        // left ours stale); a new one was started from ours (`autoStart`).
        if info.existed { wantsRunning = info.state.isRunning || info.state.isConnecting }
        setState(info.state)
        await backfill()
    }

    /// Fetches everything after the last line delivered, then releases the batches held meanwhile
    /// (deduplicated by seq, so overlap with the fetched lines is harmless).
    private func fillGap() async {
        repeat {
            gapAgain = false
            let params = LogsArea.RangeParams(id: id, afterSeq: lastSeq, limit: nil)
            if let lines = await daemon.call("logs.range", params, as: [LogLine].self) { deliver(lines) }
        } while gapAgain
        let release = held ?? []
        held = nil
        release.forEach(deliver)
    }

    private func backfill() async {
        guard !backfilling else { return }
        backfilling = true
        defer { backfilling = false }
        let params = LogsArea.RangeParams(id: id, afterSeq: lastSeq, limit: nil)
        guard let lines = await daemon.call("logs.range", params, as: [LogLine].self) else { return }
        deliver(lines)
    }

    /// The daemon can't be reached. A tab that wants to stream switches to the in-process engine;
    /// otherwise it just reads as stopped.
    private func lostDaemon() {
        guard fallback == nil else { return }
        if wantsRunning {
            useFallback().connect()
            return
        }
        var next = state
        next.isRunning = false
        next.isConnecting = false
        setState(next)
    }

    /// Switches this tab to an in-process stream for good: stops watching the daemon, and hands
    /// the engine the target and a seq past everything already shown.
    @discardableResult
    private func useFallback() -> LogStreamEngine {
        if let fallback { return fallback }
        watchTask?.cancel()
        watchTask = nil
        held = nil
        // The daemon may still be running this session (a transient failure): stop it there, so
        // one tab doesn't stream twice and record two history runs.
        let params = LogsArea.IDParams(id: id)
        commands.enqueue { [daemon] in let _: Bool? = await daemon.call("logs.close", params) }
        let fallbackName = displayName
        let engine = makeLocal(SeqCounter.slot(after: lastSeq)) { [weak self] in self?.displayName ?? fallbackName }
        if !state.package.isEmpty {
            engine.setPackage(state.package)
            engine.seedPIDs(state.pids ?? [])
        }
        engine.onLines = { [weak self] in self?.deliver($0) }
        engine.onState = { [weak self] in self?.setState($0) }
        fallback = engine
        DaemonLog.info("log tab \(id): jacad unreachable, streaming in-process")
        return engine
    }

    /// The daemon is unusable right now (unreachable after a connect attempt).
    private var daemonUnavailable: Bool {
        if case .unavailable = daemon.state { return true }
        return false
    }

    // MARK: - LogFeed

    func start() {
        wantsRunning = true
        if let fallback { return fallback.start() }
        if daemonUnavailable { return useFallback().start() }
        send("logs.start")
    }

    func stop() {
        wantsRunning = false
        if let fallback { return fallback.stop() }
        send("logs.stop")
    }

    func connect() {
        wantsRunning = true
        if let fallback { return fallback.connect() }
        if daemonUnavailable { return useFallback().connect() }
        send("logs.connect")
    }

    func setPackage(_ package: String) {
        if let fallback { return fallback.setPackage(package) }
        var next = state
        next.package = package
        setState(next)
        let params = LogsArea.PackageParams(id: id, package: package)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call("logs.setPackage", params) }
    }

    func resetBodyPairing() {
        if let fallback { return fallback.resetBodyPairing() }
        send("logs.resetPairing")
    }

    func clearDeviceBuffer() {
        if let fallback { return fallback.clearDeviceBuffer() }
        send("logs.clearDeviceBuffer")
    }

    func close() {
        watchTask?.cancel()
        watchTask = nil
        onLines = nil
        onState = nil
        if let fallback { return fallback.close() }
        let params = LogsArea.IDParams(id: id)
        commands.enqueue { [daemon] in let _: Bool? = await daemon.call("logs.close", params) }
    }

    func rename(_ name: String) {
        displayName = name
        guard fallback == nil else { return }
        let params = LogsArea.RenameParams(id: id, name: name)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call("logs.rename", params) }
    }

    private func send(_ method: String) {
        let params = LogsArea.IDParams(id: id)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call(method, params) }
    }
}
