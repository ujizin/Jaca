import Foundation

/// Captures a Chrome window on this Mac: a MITM proxy on loopback, and Chrome launched with its
/// own profile pointed at it. With override services, the proxy answers matching requests from
/// the rules, as the agent does on a device.
@MainActor
final class MacBrowserCaptureSource: CaptureSource {
    private let ca: CertificateAuthority?
    private let intercept: InterceptServices?
    private var proxy: ProxyServer?
    private var chrome: Process?
    private weak var sink: CaptureSink?

    init(ca: CertificateAuthority?, intercept: InterceptServices?) {
        self.ca = ca
        self.intercept = intercept
    }

    var interceptCapabilities: InterceptCapabilities { intercept == nil ? [] : .desktopTerminated }
    /// Nothing to arm: the proxy is the only path, so the rules run whenever it listens.
    var interceptWired: Bool { intercept != nil }

    func start(into sink: CaptureSink) {
        self.sink = sink
        guard let ca else {
            sink.capture(didChangeStatus: "Couldn’t read Jaca’s certificate. Check ~/.jaca/jacad.log.")
            return
        }
        let pipeline = intercept?.pipeline(for: .mitmProxy, deviceID: Device.thisMac.id, appID: nil)
        let server = ProxyServer(port: 0, ca: ca, pipeline: pipeline,
                                 target: ProxyInterceptTarget(deviceID: Device.thisMac.id),
                                 bindHost: "127.0.0.1",
                                 onTransaction: { [weak sink] txn in
                                     Task { @MainActor in sink?.capture(didReceive: txn) }
                                 })
        do {
            try server.start()
        } catch {
            sink.capture(didChangeStatus: "Couldn’t start the proxy: \(error.localizedDescription)")
            return
        }
        proxy = server
        sink.capture(didBindPort: server.boundPort)
        openBrowser()
    }

    /// Opens the Chrome window, or a new one after the user closed it.
    func openBrowser() {
        guard let proxy, let ca else { return }
        if let chrome, chrome.isRunning { return }
        switch ChromeBrowser.launch(proxyPort: proxy.boundPort, spkiHash: ca.rootSPKIHash) {
        case .success(let process):
            chrome = process
            sink?.capture(didChangeStatus: "Capturing Chrome on this Mac")
        case .failure(let error):
            sink?.capture(didChangeStatus: error.message)
        }
    }

    func stop() {
        if let chrome, chrome.isRunning { chrome.terminate() }
        chrome = nil
        proxy?.stop()
        proxy = nil
    }
}
