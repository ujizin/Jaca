import Foundation

struct HeaderPair: Sendable, Hashable, Identifiable {
    let name: String
    let value: String
    var id: String { name + ":" + value }
}

/// A single captured HTTP(S) request/response, with timing and sizes — the unit
/// shown in the network inspector list and detail panes.
struct NetworkTransaction: Identifiable, Sendable, Hashable {
    let id: UUID
    var method: String
    var url: String
    var host: String
    var scheme: String

    var requestHeaders: [HeaderPair]
    var requestBody: Data?

    var statusCode: Int?
    var responseHeaders: [HeaderPair]
    var responseBody: Data?
    var responseContentType: String?

    var startedAt: Date
    var responseReceivedAt: Date?   // time to first byte
    var finishedAt: Date?

    var requestBytes: Int
    var responseBytes: Int
    var error: String?

    /// Initiating call stack — only the in-process agent can provide this; nil for proxy capture.
    var callStack: [String]? = nil

    /// True once this (older) transaction's bodies have been spilled to the on-disk
    /// cache and cleared from memory; the detail view loads them back on demand.
    var bodiesEvicted = false

    init(id: UUID = UUID(), method: String, url: String, host: String, scheme: String,
         requestHeaders: [HeaderPair] = [], requestBody: Data? = nil, startedAt: Date = Date()) {
        self.id = id
        self.method = method
        self.url = url
        self.host = host
        self.scheme = scheme
        self.requestHeaders = requestHeaders
        self.requestBody = requestBody
        self.statusCode = nil
        self.responseHeaders = []
        self.responseBody = nil
        self.responseContentType = nil
        self.startedAt = startedAt
        self.responseReceivedAt = nil
        self.finishedAt = nil
        self.requestBytes = requestBody?.count ?? 0
        self.responseBytes = 0
        self.error = nil
    }

    /// Total wall-clock duration once finished, in seconds.
    var duration: TimeInterval? {
        guard let finishedAt else { return nil }
        return finishedAt.timeIntervalSince(startedAt)
    }

    /// Time-to-first-byte in seconds.
    var ttfb: TimeInterval? {
        guard let responseReceivedAt else { return nil }
        return responseReceivedAt.timeIntervalSince(startedAt)
    }

    var path: String {
        URLComponents(string: url)?.path.isEmpty == false
            ? URLComponents(string: url)!.path
            : (url.hasPrefix(scheme) ? url : "/")
    }

    var isInFlight: Bool { finishedAt == nil && error == nil }

    var statusText: String {
        if let error { return "ERR" }
        guard let statusCode else { return "…" }
        return "\(statusCode)"
    }
}

// MARK: - Wire format (daemon)

extension HeaderPair: Codable {
    private enum CodingKeys: String, CodingKey { case name, value }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(name: try c.decodeIfPresent(String.self, forKey: .name) ?? "",
                  value: try c.decodeIfPresent(String.self, forKey: .value) ?? "")
    }
}

extension NetworkTransaction: Codable {
    private enum CodingKeys: String, CodingKey {
        case id, method, url, host, scheme, requestHeaders, requestBody, statusCode, responseHeaders
        case responseBody, responseContentType, startedAt, responseReceivedAt, finishedAt
        case requestBytes, responseBytes, error, callStack, bodiesEvicted
    }

    /// Tolerant: only the id is required. Bodies are base64 (`Data`'s default), and usually
    /// omitted on the wire — see `strippingBodies()`.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(id: try c.decode(UUID.self, forKey: .id),
                  method: try c.decodeIfPresent(String.self, forKey: .method) ?? "",
                  url: try c.decodeIfPresent(String.self, forKey: .url) ?? "",
                  host: try c.decodeIfPresent(String.self, forKey: .host) ?? "",
                  scheme: try c.decodeIfPresent(String.self, forKey: .scheme) ?? "",
                  requestHeaders: CloudPersistence.decodeArrayField(HeaderPair.self, in: c, forKey: .requestHeaders),
                  requestBody: try c.decodeIfPresent(Data.self, forKey: .requestBody),
                  startedAt: try c.decodeIfPresent(Date.self, forKey: .startedAt) ?? Date(timeIntervalSince1970: 0))
        statusCode = try c.decodeIfPresent(Int.self, forKey: .statusCode)
        responseHeaders = CloudPersistence.decodeArrayField(HeaderPair.self, in: c, forKey: .responseHeaders)
        responseBody = try c.decodeIfPresent(Data.self, forKey: .responseBody)
        responseContentType = try c.decodeIfPresent(String.self, forKey: .responseContentType)
        responseReceivedAt = try c.decodeIfPresent(Date.self, forKey: .responseReceivedAt)
        finishedAt = try c.decodeIfPresent(Date.self, forKey: .finishedAt)
        requestBytes = try c.decodeIfPresent(Int.self, forKey: .requestBytes) ?? 0
        responseBytes = try c.decodeIfPresent(Int.self, forKey: .responseBytes) ?? 0
        error = try c.decodeIfPresent(String.self, forKey: .error)
        callStack = try c.decodeIfPresent([String].self, forKey: .callStack)
        bodiesEvicted = try c.decodeIfPresent(Bool.self, forKey: .bodiesEvicted) ?? false
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(id, forKey: .id)
        try c.encode(method, forKey: .method)
        try c.encode(url, forKey: .url)
        try c.encode(host, forKey: .host)
        try c.encode(scheme, forKey: .scheme)
        if !requestHeaders.isEmpty { try c.encode(requestHeaders, forKey: .requestHeaders) }
        try c.encodeIfPresent(requestBody, forKey: .requestBody)
        try c.encodeIfPresent(statusCode, forKey: .statusCode)
        if !responseHeaders.isEmpty { try c.encode(responseHeaders, forKey: .responseHeaders) }
        try c.encodeIfPresent(responseBody, forKey: .responseBody)
        try c.encodeIfPresent(responseContentType, forKey: .responseContentType)
        try c.encode(startedAt, forKey: .startedAt)
        try c.encodeIfPresent(responseReceivedAt, forKey: .responseReceivedAt)
        try c.encodeIfPresent(finishedAt, forKey: .finishedAt)
        try c.encode(requestBytes, forKey: .requestBytes)
        try c.encode(responseBytes, forKey: .responseBytes)
        try c.encodeIfPresent(error, forKey: .error)
        try c.encodeIfPresent(callStack, forKey: .callStack)
        if bodiesEvicted { try c.encode(true, forKey: .bodiesEvicted) }
    }

    /// The transaction without its bodies, marked so the viewer fetches them on demand.
    func strippingBodies() -> NetworkTransaction {
        var copy = self
        if copy.requestBody != nil || copy.responseBody != nil { copy.bodiesEvicted = true }
        copy.requestBody = nil
        copy.responseBody = nil
        return copy
    }
}
