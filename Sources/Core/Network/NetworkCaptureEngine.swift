import Foundation

/// Capture-side state of a network tab: which source was chosen and how it's doing. Published by
/// `jacad` on `net.state.<id>`.
struct NetworkCaptureState: Codable, Equatable, Sendable {
    var isRunning = false
    var isConnecting = false
    var statusMessage: String?
    var boundPort = 0
    /// Proxy started but the CA isn't confirmed — the view surfaces the setup dialog.
    var proxyNeedsSetup = false
    /// Ground truth that the CA is trusted: flips true once a real HTTPS request is decrypted.
    var caReady = false
    /// The chosen capture source (registry id), and whether the user has chosen one.
    var selectedSourceID: String?
    var hasSelectedMode = false
    /// Agent mode: the debuggable app to attach to. nil otherwise.
    var targetPackage: String?
}

extension NetworkCaptureState {
    private enum CodingKeys: String, CodingKey {
        case isRunning, isConnecting, statusMessage, boundPort, proxyNeedsSetup, caReady
        case selectedSourceID, hasSelectedMode, targetPackage
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            isRunning: try c.decodeIfPresent(Bool.self, forKey: .isRunning) ?? false,
            isConnecting: try c.decodeIfPresent(Bool.self, forKey: .isConnecting) ?? false,
            statusMessage: try c.decodeIfPresent(String.self, forKey: .statusMessage),
            boundPort: try c.decodeIfPresent(Int.self, forKey: .boundPort) ?? 0,
            proxyNeedsSetup: try c.decodeIfPresent(Bool.self, forKey: .proxyNeedsSetup) ?? false,
            caReady: try c.decodeIfPresent(Bool.self, forKey: .caReady) ?? false,
            selectedSourceID: try c.decodeIfPresent(String.self, forKey: .selectedSourceID),
            hasSelectedMode: try c.decodeIfPresent(Bool.self, forKey: .hasSelectedMode) ?? false,
            targetPackage: try c.decodeIfPresent(String.self, forKey: .targetPackage)
        )
    }
}

/// Where a `NetworkSession` tab gets its transactions and capture state: an in-process
/// `NetworkCaptureEngine`, or a capture running in `jacad`.
@MainActor
protocol NetworkFeed: AnyObject {
    var state: NetworkCaptureState { get }
    /// New or updated transactions (upserts by id), in arrival order.
    var onTransactions: (([NetworkTransaction]) -> Void)? { get set }
    var onState: ((NetworkCaptureState) -> Void)? { get set }
    /// Whether transactions arrive without bodies, to be fetched with `bodies(for:)`.
    var bodiesOnDemand: Bool { get }

    /// Choose a capture source and start it.
    func select(sourceID: String, package: String?)
    /// Restore the chosen source without starting.
    func restoreMode(sourceID: String?, package: String?)
    /// Back to the source chooser.
    func reopenChooser()
    /// Restart the chosen source.
    func resume()
    func stop()
    func clearProxyNeedsSetup()
    /// Forget captured transactions (the tab cleared its list).
    func clear()
    func bodies(for id: UUID) async -> (req: Data?, resp: Data?)
    /// The whole capture as HAR, from the side that holds the bodies.
    func harData() async -> Data?
    /// Ends the capture for good (the tab closed).
    func close()
}

/// One network capture with everything that isn't a view: choosing a `CaptureSource` from
/// `CaptureSourceRegistry`, prechecking and running it, and receiving its events as the
/// `CaptureSink`. Transactions are handed on as they arrive.
///
/// The app runs one per network tab when the daemon is off; `jacad` runs agent captures.
@MainActor
final class NetworkCaptureEngine: NetworkFeed, CaptureSink {
    let id: UUID
    let device: Device

    private(set) var state = NetworkCaptureState() {
        didSet { if state != oldValue { onState?(state) } }
    }
    var onTransactions: (([NetworkTransaction]) -> Void)?
    var onState: ((NetworkCaptureState) -> Void)?
    var bodiesOnDemand: Bool { false }
    /// Provides the transactions to export as HAR (the view's list, in-process).
    var harSource: (() -> [NetworkTransaction])?

    private let adbURL: URL?
    private let ca: CertificateAuthority?
    private let companion: CompanionHub?
    private let deviceContext: () -> DeviceContext?
    private var current: CaptureSource?

    init(id: UUID = UUID(), device: Device, adbURL: URL?, ca: CertificateAuthority?,
         companion: CompanionHub?, deviceContext: @escaping () -> DeviceContext?) {
        self.id = id
        self.device = device
        self.adbURL = adbURL
        self.ca = ca
        self.companion = companion
        self.deviceContext = deviceContext
    }

    private var currentDescriptor: CaptureSourceDescriptor? {
        state.selectedSourceID.flatMap { CaptureSourceRegistry.descriptor(id: $0) }
    }

    // MARK: - Source selection

    func select(sourceID: String, package: String?) {
        guard let descriptor = CaptureSourceRegistry.descriptor(id: sourceID) else { return }
        if state.isRunning { stop() }
        state.targetPackage = descriptor.needsPackage ? package : nil
        state.selectedSourceID = descriptor.id
        state.hasSelectedMode = true
        start()
    }

    func restoreMode(sourceID: String?, package: String?) {
        guard let sourceID, let descriptor = CaptureSourceRegistry.descriptor(id: sourceID) else { return }
        state.selectedSourceID = descriptor.id
        state.targetPackage = descriptor.kind == .agent ? package : nil
        state.hasSelectedMode = true
    }

    func reopenChooser() {
        if state.isRunning { stop() }
        state.hasSelectedMode = false
        state.proxyNeedsSetup = false
    }

    func resume() {
        guard state.hasSelectedMode, let descriptor = currentDescriptor else { return }
        select(sourceID: descriptor.id, package: state.targetPackage)
    }

    func start() {
        guard !state.isRunning, !state.isConnecting, state.hasSelectedMode, let descriptor = currentDescriptor else { return }
        state.statusMessage = nil
        guard let precheck = descriptor.precheck else { launch(descriptor); return }
        // A source that needs the device reachable (agent) verifies first and surfaces a
        // clear message instead of silently failing.
        state.isConnecting = true
        Task { @MainActor in
            let error = await precheck(makeContext())
            state.isConnecting = false
            if let error { state.statusMessage = error; return }
            launch(descriptor)
        }
    }

    private func launch(_ descriptor: CaptureSourceDescriptor) {
        guard !state.isRunning else { return }
        state.isRunning = true
        let source = descriptor.make(makeContext())
        current = source
        source.start(into: self)
    }

    private func makeContext() -> CaptureContext {
        CaptureContext(device: device, adbURL: adbURL, ca: ca, deviceContext: deviceContext(),
                       targetPackage: state.targetPackage, companion: companion)
    }

    func stop() {
        guard state.isRunning else { return }
        state.isRunning = false
        state.proxyNeedsSetup = false
        current?.stop()
        current = nil
    }

    func clearProxyNeedsSetup() { state.proxyNeedsSetup = false }
    func clear() {}
    func bodies(for id: UUID) async -> (req: Data?, resp: Data?) { (nil, nil) }
    func harData() async -> Data? { HARExport.data(from: harSource?() ?? []) }

    func close() {
        stop()
        onTransactions = nil
        onState = nil
    }

    // MARK: - CaptureSink

    func capture(didReceive transaction: NetworkTransaction) {
        // First successfully MITM'd HTTPS request confirms the CA is trusted.
        if transaction.scheme == "https", transaction.error == nil {
            state.caReady = true
            state.proxyNeedsSetup = false
        }
        onTransactions?([transaction])
    }

    func capture(didChangeStatus status: String?) { state.statusMessage = status }
    func capture(didBindPort port: Int) { state.boundPort = port }
    func captureNeedsSetup() {
        if !state.caReady { state.proxyNeedsSetup = true }
    }
}
