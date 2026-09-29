import Foundation

/// The `jacad` wire protocol: newline-delimited JSON-RPC 2.0 over a Unix domain socket.
/// Every message is one JSON object on one line. See `docs/daemon-plan.md`.
///
/// - Request: `{"jsonrpc":"2.0","id":1,"method":"gradle.list","params":{}}`
/// - Response: `{"jsonrpc":"2.0","id":1,"result":…}` or `{"jsonrpc":"2.0","id":1,"error":{…}}`
/// - Event (only on a connection that subscribed to the topic):
///   `{"jsonrpc":"2.0","method":"event","params":{"topic":"gradle.daemons","data":…}}`
///
/// Decoding is split in two passes so the transport never needs to know payload types: a
/// cheap header decode (`RPCHeader`) routes the line, then the handler or caller decodes the
/// same line again with its concrete envelope (`RPCRequestEnvelope<P>`, `RPCResponseEnvelope<R>`,
/// `RPCEventEnvelope<T>`).
enum DaemonProtocol {
    /// Bumped on any incompatible change to a method, a topic, or a payload shape. A client
    /// and daemon with different versions refuse to talk (the app restarts the daemon).
    static let version = 1

    /// Longest accepted line, in bytes. A longer line closes the connection rather than
    /// buffering without bound.
    static let maxLineBytes = 32 * 1024 * 1024
}

/// Where the daemon keeps its socket, lock and log. Defaults to `~/.jaca`; `JACA_DAEMON_DIR`
/// overrides it (tests and UI tests use a private directory so they never touch the real one).
struct DaemonPaths: Sendable, Equatable {
    let directory: URL

    var socket: URL { directory.appendingPathComponent("jacad.sock") }
    var lock: URL { directory.appendingPathComponent("jacad.lock") }
    var log: URL { directory.appendingPathComponent("jacad.log") }

    static var `default`: DaemonPaths {
        if let override = ProcessInfo.processInfo.environment["JACA_DAEMON_DIR"], !override.isEmpty {
            return DaemonPaths(directory: URL(fileURLWithPath: override, isDirectory: true))
        }
        let home = FileManager.default.homeDirectoryForCurrentUser
        return DaemonPaths(directory: home.appendingPathComponent(".jaca", isDirectory: true))
    }

    /// macOS limits `sockaddr_un.sun_path` to 104 bytes including the terminator.
    var socketPathFits: Bool { socket.path.utf8.count < 104 }
}

// MARK: - Ids

/// A JSON-RPC id: a number or a string.
enum RPCID: Codable, Hashable, Sendable, CustomStringConvertible {
    case number(Int)
    case string(String)

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if let n = try? c.decode(Int.self) { self = .number(n); return }
        self = .string(try c.decode(String.self))
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .number(let n): try c.encode(n)
        case .string(let s): try c.encode(s)
        }
    }

    var description: String {
        switch self {
        case .number(let n): return String(n)
        case .string(let s): return s
        }
    }
}

// MARK: - Errors

/// A JSON-RPC error object. Standard codes plus a Jaca range from -32000.
struct RPCError: Error, Codable, Sendable, Equatable, LocalizedError {
    var code: Int
    var message: String

    static let parseErrorCode = -32700
    static let invalidRequestCode = -32600
    static let methodNotFoundCode = -32601
    static let invalidParamsCode = -32602
    static let internalErrorCode = -32603
    /// The operation ran and failed (a service error, a missing session…).
    static let failedCode = -32000
    /// The client and daemon speak different protocol versions.
    static let versionMismatchCode = -32001
    /// The connection closed before a response arrived.
    static let disconnectedCode = -32002

    static func parseError(_ m: String) -> RPCError { RPCError(code: parseErrorCode, message: m) }
    static func invalidRequest(_ m: String) -> RPCError { RPCError(code: invalidRequestCode, message: m) }
    static func methodNotFound(_ method: String) -> RPCError {
        RPCError(code: methodNotFoundCode, message: "Unknown method: \(method)")
    }
    static func invalidParams(_ m: String) -> RPCError { RPCError(code: invalidParamsCode, message: m) }
    static func internalError(_ m: String) -> RPCError { RPCError(code: internalErrorCode, message: m) }
    static func failed(_ m: String) -> RPCError { RPCError(code: failedCode, message: m) }
    static let disconnected = RPCError(code: disconnectedCode, message: "The daemon connection closed.")

    var errorDescription: String? { message }
}

extension RPCError {
    /// Describes a decoding failure by its coding path so a client can see which field was wrong.
    static func describing(_ error: Error) -> String {
        guard let e = error as? DecodingError else { return String(describing: error) }
        func path(_ ctx: DecodingError.Context) -> String {
            let p = ctx.codingPath.map { $0.intValue.map(String.init) ?? $0.stringValue }.joined(separator: ".")
            return p.isEmpty ? "(root)" : p
        }
        switch e {
        case .keyNotFound(let key, let ctx):
            let prefix = path(ctx)
            return "missing field '\(prefix == "(root)" ? key.stringValue : prefix + "." + key.stringValue)'"
        case .typeMismatch(_, let ctx), .valueNotFound(_, let ctx):
            return "wrong type at '\(path(ctx))': \(ctx.debugDescription)"
        case .dataCorrupted(let ctx):
            return "invalid value at '\(path(ctx))': \(ctx.debugDescription)"
        @unknown default:
            return String(describing: e)
        }
    }
}

// MARK: - Envelopes

/// The routing fields of any incoming line. Params, result and event data are ignored here.
struct RPCHeader: Decodable {
    var id: RPCID?
    var method: String?
    var error: RPCError?
    /// Present on events: `params.topic`.
    var params: TopicOnly?

    struct TopicOnly: Decodable { var topic: String? }

    private enum CodingKeys: String, CodingKey { case id, method, error, params }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try? c.decodeIfPresent(RPCID.self, forKey: .id)
        method = try? c.decodeIfPresent(String.self, forKey: .method)
        error = try? c.decodeIfPresent(RPCError.self, forKey: .error)
        // Request params may be any shape; only an event's `{topic}` object matters here.
        params = try? c.decodeIfPresent(TopicOnly.self, forKey: .params)
    }
}

/// Empty params / result.
struct RPCEmpty: Codable, Sendable, Equatable {
    init() {}
    init(from decoder: Decoder) throws {}
    func encode(to encoder: Encoder) throws { _ = encoder.container(keyedBy: AnyKey.self) }
    private struct AnyKey: CodingKey {
        var stringValue: String; var intValue: Int? { nil }
        init?(stringValue: String) { self.stringValue = stringValue }
        init?(intValue: Int) { nil }
    }
}

struct RPCRequestEnvelope<P: Encodable>: Encodable {
    var jsonrpc = "2.0"
    var id: RPCID?
    var method: String
    var params: P
}

/// Decodes a request's params. Missing or `null` params decode as `P` from an empty object,
/// so a method whose params are all optional can be called with none.
struct RPCRequestParams<P: Decodable>: Decodable {
    var params: P

    private enum CodingKeys: String, CodingKey { case params }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        if try !c.contains(.params) || c.decodeNil(forKey: .params) {
            params = try JSONDecoder.daemon.decode(P.self, from: Data("{}".utf8))
        } else {
            params = try c.decode(P.self, forKey: .params)
        }
    }
}

struct RPCResponseEnvelope<R: Encodable>: Encodable {
    var jsonrpc = "2.0"
    var id: RPCID?
    var result: R
}

struct RPCErrorEnvelope: Encodable {
    var jsonrpc = "2.0"
    var id: RPCID?
    var error: RPCError
}

struct RPCResultOnly<R: Decodable>: Decodable { var result: R }

struct RPCEventEnvelope<T: Codable>: Codable {
    var jsonrpc = "2.0"
    var method = "event"
    var params: Payload

    struct Payload: Codable {
        var topic: String
        var data: T
    }

    init(topic: String, data: T) { params = Payload(topic: topic, data: data) }
}

// MARK: - Coders

extension JSONEncoder {
    /// The encoder for every wire message: compact (no newlines), ISO-8601 dates with
    /// fractional seconds, sorted keys so fixtures are stable.
    static var daemon: JSONEncoder {
        let e = JSONEncoder()
        e.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        e.dateEncodingStrategy = .custom { date, encoder in
            var c = encoder.singleValueContainer()
            try c.encode(DaemonDates.format(date))
        }
        return e
    }
}

extension JSONDecoder {
    static var daemon: JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let c = try decoder.singleValueContainer()
            if let s = try? c.decode(String.self), let date = DaemonDates.parse(s) { return date }
            if let n = try? c.decode(Double.self) { return Date(timeIntervalSince1970: n) }
            throw DecodingError.dataCorruptedError(in: c, debugDescription: "expected an ISO-8601 date")
        }
        return d
    }
}

enum DaemonDates {
    private static let lock = NSLock()
    private static let withFraction: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
    private static let plain: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    static func format(_ date: Date) -> String {
        lock.lock(); defer { lock.unlock() }
        return withFraction.string(from: date)
    }

    static func parse(_ s: String) -> Date? {
        lock.lock(); defer { lock.unlock() }
        return withFraction.date(from: s) ?? plain.date(from: s)
    }
}

// MARK: - Framing

enum DaemonLine {
    /// Encodes a message as one wire line, newline included.
    static func encode<T: Encodable>(_ value: T) throws -> Data {
        var data = try JSONEncoder.daemon.encode(value)
        data.append(0x0A)
        return data
    }

    /// An error line for a request. Never throws: an error that can't be encoded (it always
    /// can) falls back to a fixed internal-error line.
    static func error(id: RPCID?, _ error: RPCError) -> Data {
        (try? encode(RPCErrorEnvelope(id: id, error: error)))
            ?? Data("{\"error\":{\"code\":-32603,\"message\":\"encoding failed\"},\"jsonrpc\":\"2.0\"}\n".utf8)
    }
}
