import XCTest
@testable import Jaca

/// Live: a headless Chrome, started with the arguments This Mac's capture uses, loads an HTTPS
/// page through the proxy and gets the overridden response. This proves Chrome accepts Jaca's CA
/// through the SPKI flag. Skipped without Google Chrome; nothing reaches the network.
final class LiveChromeOverrideTests: XCTestCase {
    private struct PageRule: InterceptResolving {
        func resolve(_ request: InterceptedRequest,
                     capabilities: InterceptCapabilities) -> (InterceptDecision, InterceptSkipReason?) {
            guard request.url == "https://override.jaca.test/" else { return (.proceed, .noRuleMatched) }
            return (InterceptDecision(action: .respond(InterceptedResponse(
                statusCode: 200, headers: [HeaderPair(name: "Content-Type", value: "text/html")],
                body: Data("<html><body><p id=x>overridden by jaca</p></body></html>".utf8))), ruleID: UUID()), nil)
        }
    }

    private struct NoOrigin: OriginRequesting {
        func perform(_ request: InterceptedRequest) async -> InterceptedResponse {
            InterceptedResponse(statusCode: 0, error: "offline in this test")
        }
    }

    func test_chromeLoadsTheOverriddenPageThroughTheProxy() throws {
        guard let app = ChromeBrowser.appURL() else { throw XCTSkip("Google Chrome isn't installed") }
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("jaca-live-chrome-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let ca = try CertificateAuthority(directory: dir.appendingPathComponent("ca"))
        let proxy = ProxyServer(port: 0, ca: ca, pipeline: InterceptPipeline(resolver: PageRule(), origin: NoOrigin()),
                                bindHost: "127.0.0.1") { _ in }
        try proxy.start()
        defer { proxy.stop() }

        let chrome = Process()
        chrome.executableURL = ChromeBrowser.executable(in: app)
        chrome.arguments = ChromeBrowser.arguments(proxyPort: proxy.boundPort, spkiHash: ca.rootSPKIHash,
                                                   profile: dir.appendingPathComponent("profile"))
            + ["--headless=new", "--disable-gpu", "--dump-dom", "https://override.jaca.test/"]
        let out = Pipe()
        chrome.standardOutput = out
        chrome.standardError = FileHandle.nullDevice
        try chrome.run()
        let killer = DispatchWorkItem { if chrome.isRunning { chrome.terminate() } }
        DispatchQueue.global().asyncAfter(deadline: .now() + 30, execute: killer)
        let dom = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        chrome.waitUntilExit()
        killer.cancel()

        XCTAssertTrue(dom.contains("overridden by jaca"), "Chrome printed: \(dom.prefix(500))")
    }
}
