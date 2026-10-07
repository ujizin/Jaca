import Foundation

/// A database file discovered on the device for an app.
struct RemoteDB: Identifiable, Hashable, Sendable, Codable {
    var id: String { path }
    let name: String   // file name, e.g. "app.db"
    let path: String   // Android: "databases/app.db"; iOS Sim: absolute container path
}

/// A table in the (pulled) database, with its row count.
struct DBTable: Identifiable, Hashable, Sendable, Codable {
    var id: String { name }
    let name: String
    let rowCount: Int
}

/// The result of a query: ordered columns and rows (a nil cell is SQL NULL).
struct DBResultSet: Sendable, Codable, Equatable {
    let columns: [String]
    let rows: [[String?]]
}

enum DBError: Error, LocalizedError {
    case notDebuggable
    case command(String)
    case sqlite(String)
    case unsupportedPlatform
    case readOnly

    var errorDescription: String? {
        switch self {
        case .notDebuggable:
            return "The app must be debuggable to read its database (run-as failed)."
        case .command(let m): return m
        case .sqlite(let m): return "SQLite: \(m)"
        case .unsupportedPlatform:
            return "Database browsing isn't supported on this platform yet."
        case .readOnly:
            return "Only read-only queries (SELECT/WITH/PRAGMA/EXPLAIN) are allowed."
        }
    }
}

/// Bounds on one read of a pulled database, for a caller that runs someone else's SQL.
struct DBReadLimits: Sendable, Equatable {
    /// Most rows one result may hold. nil when the statement already bounds them.
    var maxRows: Int?
    /// Most cell text (UTF-8 bytes) one result may hold.
    var maxBytes: Int
    /// Seconds a statement may run before it is interrupted.
    var deadline: TimeInterval

    /// The cap a result of `rows` rows and `bytes` bytes of cell text is over, if any.
    func exceeded(rows: Int, bytes: Int) -> DBLimitExceeded? {
        if let maxRows, rows > maxRows { return .rows(maxRows) }
        if bytes > maxBytes { return .bytes(maxBytes) }
        return nil
    }

    static func bytes(in row: [String?]) -> Int {
        row.reduce(0) { $0 + ($1?.utf8.count ?? 0) }
    }
}

/// A bounded read went over a cap. Carries the cap, not the size reached.
enum DBLimitExceeded: Error, Equatable {
    case rows(Int)
    case bytes(Int)
}

/// Stops one bounded read: on `cancel()`, or once the deadline set when the read starts passes.
/// The reading thread polls it from SQLite's progress handler.
final class DBReadToken: @unchecked Sendable {
    private let lock = NSLock()
    private var cancelled = false
    private var stopAt: TimeInterval?

    func cancel() { lock.withLock { cancelled = true } }

    func start(deadline seconds: TimeInterval) {
        let at = ProcessInfo.processInfo.systemUptime + seconds
        lock.withLock { stopAt = at }
    }

    var shouldStop: Bool {
        lock.withLock { cancelled || (stopAt.map { ProcessInfo.processInfo.systemUptime >= $0 } ?? false) }
    }
}
