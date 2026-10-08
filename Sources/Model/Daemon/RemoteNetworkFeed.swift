import Foundation

/// An agent capture running in `jacad`, as a `NetworkFeed` for a `NetworkSession` tab.
///
/// On every (re)connection it opens the capture by id (attaching, or recreating it with the
/// chosen source after a daemon restart) and resyncs the transaction list. Transactions arrive
/// without bodies; the tab fetches them with `bodies(for:)` when one is shown.
@MainActor
final class RemoteNetworkFeed: NetworkFeed {
    let id: UUID
    let device: Device
    private(set) var state = NetworkCaptureState()
    var onTransactions: (([NetworkTransaction]) -> Void)?
    var onState: ((NetworkCaptureState) -> Void)?
    var bodiesOnDemand: Bool { true }

    private let daemon: DaemonConnector
    private var watchTask: Task<Void, Never>?
    private let commands = DaemonCommandQueue()
    private var wantsRunning: Bool
    private var resyncing = false
    /// A resync was asked for while one ran: run again once it ends.
    private var resyncAgain = false
    /// Clears sent but not yet done in the daemon. Upserts arriving meanwhile were published
    /// before the clear and would bring cleared rows back, so they're dropped; the list is
    /// fetched again once the clear lands.
    private var clearsPending = 0
    private var clearGeneration = 0

    init(id: UUID, device: Device, autoStart: Bool, daemon: DaemonConnector) {
        self.id = id
        self.device = device
        self.wantsRunning = autoStart
        self.daemon = daemon
        watch()
    }

    private func watch() {
        watchTask = daemon.watch(
            [NetworkArea.transactionsTopic(id), NetworkArea.stateTopic(id)],
            onUnavailable: { [weak self] in self?.lostDaemon() },
            onSubscribed: { [weak self] in await self?.openAndResync() }
        ) { [weak self] event in
            self?.handle(event)
        }
    }

    private func handle(_ event: DaemonEventLine) {
        switch event.topic {
        case NetworkArea.transactionsTopic(id):
            guard clearsPending == 0, let batch = try? event.decode([NetworkTransaction].self) else { return }
            onTransactions?(batch)
        case NetworkArea.stateTopic(id):
            guard let next = try? event.decode(NetworkCaptureState.self) else { return }
            wantsRunning = next.isRunning || next.isConnecting
            setState(next)
        case "events.dropped":
            // A lost state is fetched again (reopening returns it). Missed upserts: fetch the whole
            // list again (upserts are idempotent by id).
            if (try? event.decode(DroppedEvents.self))?.topic == NetworkArea.stateTopic(id) {
                Task { [weak self] in
                    // Not after the tab closed: that would recreate the capture.
                    guard let self, self.watchTask != nil else { return }
                    await self.openAndResync()
                }
            } else {
                Task { await resync() }
            }
        default:
            break
        }
    }

    private func setState(_ next: NetworkCaptureState) {
        guard next != state else { return }
        state = next
        onState?(next)
    }

    private func openAndResync() async {
        // Never auto-started: reattaching ignores it, and a capture recreated after a daemon
        // restart comes back stopped, since starting the agent relaunches the user's app.
        let params = NetworkArea.OpenParams(id: id, device: device, sourceID: state.selectedSourceID,
                                            package: state.targetPackage, autoStart: false)
        guard let info = await daemon.call("network.open", params, as: NetworkArea.SessionInfo.self) else { return }
        // Same rule as the log and cloud feeds: the daemon's run state wins for a capture it had.
        if info.existed { wantsRunning = info.state.isRunning || info.state.isConnecting }
        setState(info.state)
        await resync()
    }

    private func resync() async {
        guard !resyncing else { resyncAgain = true; return }
        resyncing = true
        defer { resyncing = false }
        repeat {
            resyncAgain = false
            let generation = clearGeneration
            guard let all = await daemon.call("network.transactions", NetworkArea.IDParams(id: id),
                                              as: [NetworkTransaction].self) else { return }
            if !all.isEmpty, clearsPending == 0, generation == clearGeneration { onTransactions?(all) }
        } while resyncAgain
    }

    private func lostDaemon() {
        var next = state
        next.isRunning = false
        next.isConnecting = false
        setState(next)
    }

    // MARK: - NetworkFeed

    func select(sourceID: String, package: String?) {
        guard let descriptor = CaptureSourceRegistry.descriptor(id: sourceID) else { return }
        // Remember the choice locally too, so a daemon restart can recreate the capture. Same
        // package rule as `NetworkCaptureEngine.select`.
        var next = state
        next.selectedSourceID = sourceID
        next.targetPackage = descriptor.needsPackage ? package : nil
        next.hasSelectedMode = true
        setState(next)
        wantsRunning = true
        let params = NetworkArea.SelectParams(id: id, sourceID: sourceID, package: package)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call("network.select", params) }
    }

    func restoreMode(sourceID: String?, package: String?) {
        // Same rule as `NetworkCaptureEngine.restoreMode`.
        guard let sourceID, let descriptor = CaptureSourceRegistry.descriptor(id: sourceID) else { return }
        var next = state
        next.selectedSourceID = sourceID
        next.targetPackage = descriptor.kind == .agent ? package : nil
        next.hasSelectedMode = true
        setState(next)
        // Applied by the next `network.open` (the capture may not exist in the daemon yet).
    }

    func reopenChooser() {
        wantsRunning = false
        send("network.reopenChooser")
    }

    func resume() {
        guard state.hasSelectedMode else { return }
        if let sourceID = state.selectedSourceID {
            select(sourceID: sourceID, package: state.targetPackage)
        }
    }

    func stop() {
        wantsRunning = false
        send("network.stop")
    }

    func clearProxyNeedsSetup() { send("network.clearProxyNeedsSetup") }
    func restartForInterceptChange() { send("network.restartForInterceptChange") }
    func relaunchToAttach() { send("network.relaunchToAttach") }
    func clear() {
        clearsPending += 1
        clearGeneration += 1
        let params = NetworkArea.IDParams(id: id)
        commands.enqueue { [weak self, daemon] in
            let _: RPCEmpty? = await daemon.call("network.clear", params)
            guard let self else { return }
            self.clearsPending -= 1
            if self.clearsPending == 0 { await self.resync() }
        }
    }

    func bodies(for transaction: UUID) async -> (req: Data?, resp: Data?)? {
        guard let b = await daemon.call("network.body", NetworkArea.BodyParams(id: id, transaction: transaction),
                                        as: NetworkArea.Bodies?.self) ?? nil else { return nil }
        return (b.request, b.response)
    }

    func harData() async -> Data? {
        await daemon.call("network.exportHAR", NetworkArea.IDParams(id: id), as: Data?.self) ?? nil
    }

    func close() {
        watchTask?.cancel()
        watchTask = nil
        onTransactions = nil
        onState = nil
        send("network.close")
    }

    private func send(_ method: String) {
        let params = NetworkArea.IDParams(id: id)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call(method, params) }
    }
}
