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
            guard let batch = try? event.decode([NetworkTransaction].self) else { return }
            onTransactions?(batch)
        case NetworkArea.stateTopic(id):
            guard let next = try? event.decode(NetworkCaptureState.self) else { return }
            wantsRunning = next.isRunning || next.isConnecting
            setState(next)
        case "events.dropped":
            // Missed upserts: fetch the whole list again (upserts are idempotent by id).
            Task { await resync() }
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
        let params = NetworkArea.OpenParams(id: id, device: device, sourceID: state.selectedSourceID,
                                            package: state.targetPackage, autoStart: wantsRunning)
        guard let info = await daemon.call("network.open", params, as: NetworkArea.SessionInfo.self) else { return }
        setState(info.state)
        await resync()
    }

    private func resync() async {
        guard !resyncing else { return }
        resyncing = true
        defer { resyncing = false }
        guard let all = await daemon.call("network.transactions", NetworkArea.IDParams(id: id),
                                          as: [NetworkTransaction].self), !all.isEmpty else { return }
        onTransactions?(all)
    }

    private func lostDaemon() {
        var next = state
        next.isRunning = false
        next.isConnecting = false
        setState(next)
    }

    // MARK: - NetworkFeed

    func select(sourceID: String, package: String?) {
        // Remember the choice locally too, so a daemon restart can recreate the capture.
        var next = state
        next.selectedSourceID = sourceID
        next.targetPackage = package
        next.hasSelectedMode = true
        setState(next)
        wantsRunning = true
        let params = NetworkArea.SelectParams(id: id, sourceID: sourceID, package: package)
        commands.enqueue { [daemon] in let _: RPCEmpty? = await daemon.call("network.select", params) }
    }

    func restoreMode(sourceID: String?, package: String?) {
        var next = state
        next.selectedSourceID = sourceID
        next.targetPackage = package
        next.hasSelectedMode = sourceID != nil
        setState(next)
        // Applied by the next `network.open` (the capture may not exist in the daemon yet).
    }

    func reopenChooser() {
        wantsRunning = false
        send("network.reopenChooser")
    }

    func resume() {
        guard state.hasSelectedMode else { return }
        wantsRunning = true
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
    func clear() { send("network.clear") }

    func bodies(for transaction: UUID) async -> (req: Data?, resp: Data?) {
        let b = await daemon.call("network.body", NetworkArea.BodyParams(id: id, transaction: transaction),
                                  as: NetworkArea.Bodies.self)
        return (b?.request, b?.response)
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
