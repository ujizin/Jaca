import XCTest
@testable import Jaca

/// The MITM proxy runs each request through the override pipeline: a matching rule answers it,
/// any other request goes to the origin. Stubs stand in for the rules and the origin, so nothing
/// leaves the machine.
final class ProxyOverrideTests: XCTestCase {
    private struct StubResolver: InterceptResolving {
        let ruleID: UUID
        func resolve(_ request: InterceptedRequest,
                     capabilities: InterceptCapabilities) -> (InterceptDecision, InterceptSkipReason?) {
            guard request.url.hasSuffix("/mocked"), request.transport == .mitmProxy,
                  capabilities == .desktopTerminated else { return (.proceed, .noRuleMatched) }
            return (InterceptDecision(action: .respond(InterceptedResponse(
                statusCode: 418, headers: [HeaderPair(name: "Content-Type", value: "text/plain")],
                body: Data("mock".utf8))), ruleID: ruleID), nil)
        }
    }

    private struct StubOrigin: OriginRequesting {
        func perform(_ request: InterceptedRequest) async -> InterceptedResponse {
            InterceptedResponse(statusCode: 200, body: Data(("origin " + request.url).utf8),
                                       responseStart: Date(), responseEnd: Date())
        }
    }

    private var dir: URL!
    private var ca: CertificateAuthority!
    private let sink = ProxyServerTests.Sink()

    override func setUpWithError() throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent("jaca-ca-\(UUID().uuidString)", isDirectory: true)
        ca = try CertificateAuthority(directory: dir)
    }

    override func tearDown() {
        try? FileManager.default.removeItem(at: dir)
    }

    private func curl(_ port: Int, _ url: String) throws -> (code: String, body: String) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
        process.arguments = ["-s", "--max-time", "10", "-w", "\n%{http_code}",
                             "-x", "http://127.0.0.1:\(port)", "--cacert", dir.appendingPathComponent("rootCA.pem").path, url]
        let out = Pipe()
        process.standardOutput = out
        try process.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        let lines = String(decoding: data, as: UTF8.self).components(separatedBy: "\n")
        return (lines.last ?? "", lines.dropLast().joined(separator: "\n"))
    }

    private func completed(_ url: String) -> NetworkTransaction? {
        let deadline = Date().addingTimeInterval(3)
        while Date() < deadline {
            if let txn = sink.transactions.first(where: { $0.url == url && !$0.isInFlight }) { return txn }
            Thread.sleep(forTimeInterval: 0.05)
        }
        return nil
    }

    func test_aMatchingRuleAnswers_andOtherRequestsReachTheOrigin() throws {
        let ruleID = UUID()
        let pipeline = InterceptPipeline(resolver: StubResolver(ruleID: ruleID), origin: StubOrigin())
        let proxy = ProxyServer(port: 0, ca: ca, pipeline: pipeline, bindHost: "127.0.0.1") { [sink] in sink.add($0) }
        try proxy.start()
        defer { proxy.stop() }

        let mocked = try curl(proxy.boundPort, "https://api.jaca.test/mocked")
        XCTAssertEqual(mocked.code, "418")
        XCTAssertEqual(mocked.body, "mock")
        let mockedTxn = completed("https://api.jaca.test/mocked")
        XCTAssertEqual(mockedTxn?.statusCode, 418)
        XCTAssertEqual(mockedTxn?.overriddenByRuleID, ruleID)

        let real = try curl(proxy.boundPort, "https://api.jaca.test/real")
        XCTAssertEqual(real.code, "200")
        XCTAssertEqual(real.body, "origin https://api.jaca.test/real")
        let realTxn = completed("https://api.jaca.test/real")
        XCTAssertEqual(realTxn?.statusCode, 200)
        XCTAssertNil(realTxn?.overriddenByRuleID)
    }
}
