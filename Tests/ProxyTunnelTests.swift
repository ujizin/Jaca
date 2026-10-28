import Network
import XCTest
@testable import Jaca

/// `CONNECT` tunnels: the target's port is kept, and a tunnel that isn't TLS (a `ws://`
/// WebSocket from Chrome) is relayed to its target as it is.
final class ProxyTunnelTests: XCTestCase {
    func test_connectTarget_andTunnelURL() {
        XCTAssertTrue(ProxyServer.connectTarget("example.com:443") == ("example.com", 443))
        XCTAssertTrue(ProxyServer.connectTarget("app.local:3000") == ("app.local", 3000))
        XCTAssertTrue(ProxyServer.connectTarget("[::1]:3000") == ("::1", 3000))
        XCTAssertTrue(ProxyServer.connectTarget("example.com") == ("example.com", 443))
        XCTAssertEqual(ProxyServer.tunnelURL(host: "example.com", port: 443, path: "/a?b"), "https://example.com/a?b")
        XCTAssertEqual(ProxyServer.tunnelURL(host: "app.local", port: 3000, path: "/"), "https://app.local:3000/")
        XCTAssertEqual(ProxyServer.tunnelURL(host: "::1", port: 3000, path: "/"), "https://[::1]:3000/")
    }

    func test_aTunnelThatIsNotTLS_isRelayedBothWays() throws {
        let echo = try EchoServer()
        defer { echo.stop() }
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("jaca-tunnel-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let proxy = ProxyServer(port: 0, ca: try CertificateAuthority(directory: dir), bindHost: "127.0.0.1") { _ in }
        try proxy.start()
        defer { proxy.stop() }

        let client = RawClient(port: proxy.boundPort)
        defer { client.stop() }
        client.send("CONNECT 127.0.0.1:\(echo.port) HTTP/1.1\r\nHost: 127.0.0.1:\(echo.port)\r\n\r\n")
        XCTAssertTrue(client.waitFor("200 Connection Established"), "got: \(client.text)")
        client.send("GET /socket HTTP/1.1\r\nUpgrade: websocket\r\n\r\n")
        XCTAssertTrue(client.waitFor("echo:GET /socket"), "got: \(client.text)")
        client.send("second")
        XCTAssertTrue(client.waitFor("echo:second"), "got: \(client.text)")
    }
}

/// Answers every read with "echo:" and what it read.
private final class EchoServer: @unchecked Sendable {
    private let listener: NWListener
    var port: UInt16 { listener.port?.rawValue ?? 0 }

    init() throws {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        listener = try NWListener(using: params)
        let ready = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { if case .ready = $0 { ready.signal() } }
        listener.newConnectionHandler = { connection in
            connection.start(queue: .global())
            Self.echo(connection)
        }
        listener.start(queue: .global())
        _ = ready.wait(timeout: .now() + 5)
    }

    func stop() { listener.cancel() }

    private static func echo(_ connection: NWConnection) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 65536) { data, _, done, error in
            if let data, !data.isEmpty {
                connection.send(content: Data("echo:".utf8) + data, completion: .contentProcessed { _ in })
            }
            if done || error != nil { connection.cancel() } else { echo(connection) }
        }
    }
}

/// A TCP client that keeps everything it has read as text.
private final class RawClient: @unchecked Sendable {
    private let connection: NWConnection
    private let lock = NSLock()
    private var received = Data()

    init(port: Int) {
        connection = NWConnection(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: UInt16(port))!, using: .tcp)
        connection.start(queue: .global())
        receive()
    }

    var text: String { lock.lock(); defer { lock.unlock() }; return String(decoding: received, as: UTF8.self) }

    func send(_ s: String) { connection.send(content: Data(s.utf8), completion: .contentProcessed { _ in }) }

    func waitFor(_ needle: String, timeout: TimeInterval = 5) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if text.contains(needle) { return true }
            usleep(20_000)
        }
        return false
    }

    func stop() { connection.cancel() }

    private func receive() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 65536) { [weak self] data, _, done, error in
            guard let self else { return }
            if let data { self.lock.lock(); self.received.append(data); self.lock.unlock() }
            if !done, error == nil { self.receive() }
        }
    }
}
