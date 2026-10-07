import Foundation

/// What a method handler knows about the request it serves.
struct DaemonRequestContext: Sendable {
    let peer: DaemonPeer
    let bus: DaemonEventBus
}

/// Maps method names to typed handlers. Handlers never crash the daemon: a params decode
/// failure becomes `invalidParams`, a thrown `RPCError` is sent as is, and any other error
/// becomes `failed` with its description.
final class DaemonRouter: @unchecked Sendable {
    typealias Handler = @Sendable (_ line: Data, _ id: RPCID?, _ ctx: DaemonRequestContext) async -> Data

    struct MethodInfo: Codable, Sendable, Equatable {
        var name: String
        var summary: String
        /// The shapes of `params` and `result`, for the methods that declare them.
        var params: DaemonSchema?
        var result: DaemonSchema?

        init(name: String, summary: String, params: DaemonSchema? = nil, result: DaemonSchema? = nil) {
            self.name = name
            self.summary = summary
            self.params = params
            self.result = result
        }

        private enum CodingKeys: String, CodingKey { case name, summary, params, result }

        /// Tolerant: the schemas arrived after the first version of this message.
        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            self.init(name: try c.decode(String.self, forKey: .name),
                      summary: (try? c.decodeIfPresent(String.self, forKey: .summary)) ?? "",
                      params: try? c.decodeIfPresent(DaemonSchema.self, forKey: .params),
                      result: try? c.decodeIfPresent(DaemonSchema.self, forKey: .result))
        }
    }

    struct TopicInfo: Codable, Sendable, Equatable {
        var name: String
        var summary: String
        var retained: Bool
    }

    private let lock = NSLock()
    private var handlers: [String: Handler] = [:]
    private var methodInfo: [String: MethodInfo] = [:]
    private var topicInfo: [String: TopicInfo] = [:]
    private var concurrentMethods: Set<String> = []

    /// Registers `method`. `P` is decoded from `params` (absent params decode from `{}`), and
    /// the returned `R` becomes `result`.
    func register<P: Decodable, R: Encodable>(
        _ method: String,
        _ summary: String,
        takes: DaemonSchema? = nil,
        returns: DaemonSchema? = nil,
        concurrent: Bool = false,
        _ body: @escaping @Sendable (P, DaemonRequestContext) async throws -> R
    ) {
        register(method, summary, params: P.self, takes: takes, returns: returns, concurrent: concurrent, body)
    }

    /// `concurrent: false` (the default): requests on one connection run one at a time, in the
    /// order they arrived, so a client may pipeline "open" then "select" and rely on the order.
    /// `concurrent: true` for slow read-only work (`du`, gcloud, SQL, bodies), which then runs
    /// alongside instead of holding up everything sent after it.
    ///
    /// `takes` and `returns` describe `params` and `result` in `api.describe`.
    func register<P: Decodable, R: Encodable>(
        _ method: String,
        _ summary: String,
        params: P.Type,
        takes: DaemonSchema? = nil,
        returns: DaemonSchema? = nil,
        concurrent: Bool = false,
        _ body: @escaping @Sendable (P, DaemonRequestContext) async throws -> R
    ) {
        let handler: Handler = { line, id, ctx in
            let params: P
            do {
                params = try JSONDecoder.daemon.decode(RPCRequestParams<P>.self, from: line).params
            } catch {
                return DaemonLine.error(id: id, .invalidParams(RPCError.describing(error)))
            }
            do {
                let result = try await body(params, ctx)
                return try DaemonLine.encode(RPCResponseEnvelope(id: id, result: result))
            } catch let error as RPCError {
                return DaemonLine.error(id: id, error)
            } catch {
                return DaemonLine.error(id: id, .failed(error.localizedDescription))
            }
        }
        lock.lock()
        handlers[method] = handler
        methodInfo[method] = MethodInfo(name: method, summary: summary, params: takes, result: returns)
        if concurrent { concurrentMethods.insert(method) } else { concurrentMethods.remove(method) }
        lock.unlock()
    }

    /// Whether a request line may run alongside others on its connection.
    func isConcurrent(line: Data) -> Bool {
        guard let method = (try? JSONDecoder.daemon.decode(RPCHeader.self, from: line))?.method else { return false }
        return lock.withLock { concurrentMethods.contains(method) }
    }

    /// Documents an event topic (or a topic family, e.g. `logs.lines.<session>`) for `api.describe`.
    func describeTopic(_ name: String, _ summary: String, retained: Bool = false) {
        lock.lock(); topicInfo[name] = TopicInfo(name: name, summary: summary, retained: retained); lock.unlock()
    }

    var methods: [MethodInfo] {
        lock.lock(); defer { lock.unlock() }
        return methodInfo.values.sorted { $0.name < $1.name }
    }

    var topics: [TopicInfo] {
        lock.lock(); defer { lock.unlock() }
        return topicInfo.values.sorted { $0.name < $1.name }
    }

    /// Handles one inbound line. Returns the response line, or nil for a notification (a
    /// request without an id), which gets no response.
    func handle(line: Data, ctx: DaemonRequestContext) async -> Data? {
        let header: RPCHeader
        do {
            header = try JSONDecoder.daemon.decode(RPCHeader.self, from: line)
        } catch {
            return DaemonLine.error(id: nil, .parseError("Not a JSON object: \(RPCError.describing(error))"))
        }
        guard let method = header.method, !method.isEmpty else {
            return DaemonLine.error(id: header.id, .invalidRequest("Missing 'method'."))
        }
        let handler = lock.withLock { handlers[method] }
        guard let handler else {
            return header.id == nil ? nil : DaemonLine.error(id: header.id, .methodNotFound(method))
        }
        let response = await handler(line, header.id, ctx)
        return header.id == nil ? nil : response
    }
}
