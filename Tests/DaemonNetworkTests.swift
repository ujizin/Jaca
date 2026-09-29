import XCTest
@testable import Jaca

@MainActor
final class DaemonNetworkTests: XCTestCase {
    private let device = Device(id: "emu-1", platform: .android, model: "Pixel", state: .connected)

    private func waitUntil(_ timeout: Duration = .seconds(3), _ cond: () -> Bool) async throws {
        let deadline = ContinuousClock.now + timeout
        while !cond() {
            guard ContinuousClock.now < deadline else { return XCTFail("condition not met in time") }
            try await Task.sleep(for: .milliseconds(20))
        }
    }

    private func txn(_ i: Int, body: String? = nil) -> NetworkTransaction {
        var t = NetworkTransaction(method: "GET", url: "https://api.example.com/\(i)", host: "api.example.com",
                                   scheme: "https", requestHeaders: [HeaderPair(name: "Accept", value: "*/*")])
        t.statusCode = 200
        t.responseBody = body.map { Data($0.utf8) }
        t.responseBytes = t.responseBody?.count ?? 0
        t.finishedAt = t.startedAt.addingTimeInterval(0.1)
        return t
    }

    func test_transaction_roundTripsAndStripsBodies() throws {
        let t = txn(1, body: "{\"ok\":true}")
        let back = try JSONDecoder.daemon.decode(NetworkTransaction.self, from: JSONEncoder.daemon.encode(t))
        XCTAssertEqual(back.id, t.id)
        XCTAssertEqual(back.responseBody, t.responseBody)
        XCTAssertEqual(back.requestHeaders, t.requestHeaders)
        let stripped = t.strippingBodies()
        XCTAssertNil(stripped.responseBody)
        XCTAssertTrue(stripped.bodiesEvicted)
        XCTAssertFalse(txn(2).strippingBodies().bodiesEvicted, "nothing to fetch when there was no body")
    }

    func test_remoteTab_receivesUpsertsAndFetchesBodiesOnDemand() async throws {
        let daemon = try TestDaemon()
        let dir = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("nb-\(UUID().uuidString.prefix(8))")
        defer { try? FileManager.default.removeItem(at: dir) }
        let captures = NetworkArea.Captures(bus: daemon.server.bus, bodiesInMemory: 2)
        NetworkArea.install(on: daemon.server, captures: captures)
        let connector = DaemonConnector(paths: daemon.paths, executable: nil, enabledAreas: [.network])
        let ca = try XCTUnwrap(try? CertificateAuthority(directory: dir))

        let id = UUID()
        let feed = RemoteNetworkFeed(id: id, device: device, autoStart: false, daemon: connector)
        let tab = NetworkSession(id: id, device: device, feed: feed, ca: ca, adbURL: nil, isRemote: true)
        try await waitUntil { captures.sessions[id] != nil }
        let engine = try XCTUnwrap(captures.sessions[id]?.engine)

        // The capture source reports transactions (a real agent would); an update upserts by id.
        var first = txn(0, body: "zero")
        engine.capture(didReceive: first)
        for i in 1...3 { engine.capture(didReceive: txn(i, body: "body-\(i)")) }
        first.statusCode = 304
        engine.capture(didReceive: first)
        try await waitUntil { tab.transactions.count == 4 && tab.transactions.first?.statusCode == 304 }
        XCTAssertTrue(tab.transactions.allSatisfy { $0.responseBody == nil && $0.bodiesEvicted }, "bodies stay in the daemon")
        XCTAssertTrue(tab.caReady, "an HTTPS transaction confirms the CA")

        // Selecting the oldest fetches its body; it was spilled to the daemon's disk cache.
        tab.selectedID = first.id
        try await waitUntil { tab.selected?.responseBody != nil }
        XCTAssertEqual(tab.selected?.responseBody, Data("zero".utf8))

        let har = await tab.harData()
        XCTAssertNotNil(har)

        // Relaunch: a tab with the same id attaches and resyncs the list.
        let feed2 = RemoteNetworkFeed(id: id, device: device, autoStart: false, daemon: connector)
        let tab2 = NetworkSession(id: id, device: device, feed: feed2, ca: ca, adbURL: nil, isRemote: true)
        try await waitUntil { tab2.transactions.count == 4 }

        tab2.close()
        try await waitUntil { captures.sessions[id] == nil }
    }

    func test_agentCapture_onMissingDevice_reportsThroughTheDaemon() async throws {
        let adb = try XCTUnwrap(AndroidToolchain.adbURL(), "adb not found")
        _ = adb
        let daemon = try TestDaemon()
        let captures = NetworkArea.Captures(bus: daemon.server.bus, bodyCache: nil)
        NetworkArea.install(on: daemon.server, captures: captures)
        let connector = DaemonConnector(paths: daemon.paths, executable: nil, enabledAreas: [.network])
        let dir = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("nb-\(UUID().uuidString.prefix(8))")
        defer { try? FileManager.default.removeItem(at: dir) }
        let ca = try XCTUnwrap(try? CertificateAuthority(directory: dir))
        let ghost = Device(id: "no-such-device-zzz", platform: .android, model: "Ghost", state: .connected)

        let id = UUID()
        let feed = RemoteNetworkFeed(id: id, device: ghost, autoStart: false, daemon: connector)
        let tab = NetworkSession(id: id, device: ghost, feed: feed, ca: ca, adbURL: adb, isRemote: true)
        try await waitUntil { captures.sessions[id] != nil }
        tab.startAgentCapture(package: "com.example.missing")
        try await waitUntil(.seconds(8)) { tab.statusMessage != nil && !tab.isConnecting }
        XCTAssertFalse(tab.isRunning)
        tab.close()
    }
}
