import Foundation

/// An inclusive range of HTTP status codes.
struct NetworkStatusRange: Codable, Sendable, Equatable {
    var min: Int
    var max: Int

    init(min: Int, max: Int) {
        self.min = min
        self.max = max
    }

    /// `404`, a class (`5xx`) or a range (`400-499`). nil for anything else.
    init?(token: String) {
        let token = token.trimmingCharacters(in: .whitespaces).lowercased()
        if token.count == 3, token.hasSuffix("xx"), let hundreds = token.first?.wholeNumberValue, (1...5).contains(hundreds) {
            self.init(min: hundreds * 100, max: hundreds * 100 + 99)
            return
        }
        let bounds = token.split(separator: "-", omittingEmptySubsequences: false)
        guard (1...2).contains(bounds.count), let low = Self.code(bounds[0]), let high = Self.code(bounds[bounds.count - 1]),
              low <= high else { return nil }
        self.init(min: low, max: high)
    }

    /// A comma-separated list of tokens (`404,5xx`). nil when any token is unreadable or there is none.
    static func parse(_ text: String) -> [NetworkStatusRange]? {
        let ranges = text.split(separator: ",", omittingEmptySubsequences: false).map { NetworkStatusRange(token: String($0)) }
        guard !ranges.isEmpty, !ranges.contains(where: { $0 == nil }) else { return nil }
        return ranges.compactMap { $0 }
    }

    func contains(_ code: Int) -> Bool { code >= min && code <= max }

    private static func code(_ text: Substring) -> Int? {
        guard text.count == 3, text.allSatisfy({ $0.isASCII && $0.isNumber }), let code = Int(text),
              (100...599).contains(code) else { return nil }
        return code
    }

    private enum CodingKeys: String, CodingKey { case min, max }

    /// Tolerant: a missing bound leaves that side open.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(min: (try? c.decodeIfPresent(Int.self, forKey: .min)) ?? 0,
                  max: (try? c.decodeIfPresent(Int.self, forKey: .max)) ?? Int.max)
    }
}

/// Which captured transactions a search returns. Every set field must match. Pure.
struct NetworkSearch: Sendable, Equatable {
    /// Part of the host, case-insensitive.
    var host: String?
    var method: String?
    /// Any of these ranges. A transaction with no response yet matches none.
    var status: [NetworkStatusRange] = []
    /// Only transactions that failed: a transport error, or a 4xx or 5xx status.
    var failed = false
    /// The start of the transaction id, case-insensitive.
    var idPrefix: String?

    static func isFailed(_ txn: NetworkTransaction) -> Bool {
        txn.error != nil || (txn.statusCode ?? 0) >= 400
    }

    func matches(_ txn: NetworkTransaction) -> Bool {
        if let host, !host.isEmpty, !txn.host.localizedCaseInsensitiveContains(host) { return false }
        if let method, !method.isEmpty, txn.method.caseInsensitiveCompare(method) != .orderedSame { return false }
        if !status.isEmpty {
            guard let code = txn.statusCode, status.contains(where: { $0.contains(code) }) else { return false }
        }
        if failed, !Self.isFailed(txn) { return false }
        if let idPrefix, !idPrefix.isEmpty, !txn.id.uuidString.lowercased().hasPrefix(idPrefix.lowercased()) { return false }
        return true
    }
}
