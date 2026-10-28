import Network
import XCTest
@testable import Jaca

/// The proxy's upstream client passes cookies through untouched: each Set-Cookie reaches the
/// client on its own, and no cookie jar adds cookies to the client's requests.
final class UpstreamClientCookieTests: XCTestCase {
    func test_splitSetCookie_keepsExpiresDatesWhole() {
        XCTAssertEqual(UpstreamClient.splitSetCookie(
            "ssm=ssm; Path=/; Secure, sessionid=6ae1; Path=/; Expires=Wed, 28 Oct 2026 14:25:25 GMT; Max-Age=1800, _sessionid=6ae1; HttpOnly"),
            ["ssm=ssm; Path=/; Secure",
             "sessionid=6ae1; Path=/; Expires=Wed, 28 Oct 2026 14:25:25 GMT; Max-Age=1800",
             "_sessionid=6ae1; HttpOnly"])
        XCTAssertEqual(UpstreamClient.splitSetCookie("a=1"), ["a=1"])
        XCTAssertEqual(UpstreamClient.splitSetCookie("a=x,y; Path=/"), ["a=x,y; Path=/"])
        XCTAssertEqual(UpstreamClient.splitSetCookie(""), [])
    }

    func test_send_returnsEachCookie_andAddsNone() async throws {
        let server = try CookieServer()
        defer { server.stop() }
        let client = UpstreamClient()
        let url = URL(string: "http://127.0.0.1:\(server.port)/")!

        let first = await client.send(URLRequest(url: url))
        XCTAssertNil(first.error)
        let cookies = first.headers.filter { $0.0.caseInsensitiveCompare("Set-Cookie") == .orderedSame }.map(\.1)
        XCTAssertEqual(cookies, ["a=1; Path=/", "b=2; Path=/; Expires=Wed, 28 Oct 2026 14:25:25 GMT"])

        _ = await client.send(URLRequest(url: url))
        XCTAssertEqual(server.cookieHeaders(), [nil, nil], "the client sent a cookie it never set")
    }
}

/// Answers every request with two Set-Cookie headers and records each request's Cookie header.
private final class CookieServer: @unchecked Sendable {
    private let listener: NWListener
    private let lock = NSLock()
    private var seen: [String?] = []
    var port: UInt16 { listener.port?.rawValue ?? 0 }

    init() throws {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        listener = try NWListener(using: params)
        let ready = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { if case .ready = $0 { ready.signal() } }
        listener.newConnectionHandler = { [weak self] connection in
            connection.start(queue: .global())
            self?.serve(connection)
        }
        listener.start(queue: .global())
        _ = ready.wait(timeout: .now() + 5)
    }

    func stop() { listener.cancel() }

    func cookieHeaders() -> [String?] { lock.lock(); defer { lock.unlock() }; return seen }

    private func serve(_ connection: NWConnection, buffer: Data = Data()) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 65536) { [weak self] data, _, done, error in
            guard let self, error == nil else { connection.cancel(); return }
            var buffer = buffer + (data ?? Data())
            while let end = buffer.range(of: Data("\r\n\r\n".utf8)) {
                let head = String(decoding: buffer[..<end.lowerBound], as: UTF8.self)
                buffer.removeSubrange(..<end.upperBound)
                let cookie = head.components(separatedBy: "\r\n")
                    .first { $0.lowercased().hasPrefix("cookie:") }
                    .map { String($0.dropFirst("cookie:".count)).trimmingCharacters(in: .whitespaces) }
                self.lock.lock(); self.seen.append(cookie); self.lock.unlock()
                let reply = "HTTP/1.1 200 OK\r\nSet-Cookie: a=1; Path=/\r\n"
                    + "Set-Cookie: b=2; Path=/; Expires=Wed, 28 Oct 2026 14:25:25 GMT\r\nContent-Length: 2\r\n\r\nok"
                connection.send(content: Data(reply.utf8), completion: .contentProcessed { _ in })
            }
            if done { connection.cancel() } else { self.serve(connection, buffer: buffer) }
        }
    }
}
