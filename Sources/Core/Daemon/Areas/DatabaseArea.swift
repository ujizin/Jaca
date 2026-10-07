import Foundation

/// Read-only browsing of an app's SQLite databases in the daemon. A session belongs to one
/// device: `database.databases` lists an app's databases, `database.pull` copies one into a
/// temp dir, and `database.rows` / `database.query` read that copy. The copy is a snapshot;
/// pull again to refresh it.
///
/// A session holds one copy. Reads name the database they expect, so a client whose pull was
/// overtaken by another one gets an error instead of rows from the other database.
///
/// The area has no topics, so nothing tells the daemon a client is still there. Every call
/// marks its session as used, and a session idle for `orphanTimeout` is closed and its copy
/// deleted.
enum DatabaseArea {
    struct OpenParams: Codable, Sendable { var id: UUID; var device: Device }
    struct IDParams: Codable, Sendable { var id: UUID }
    struct DatabasesParams: Codable, Sendable { var id: UUID; var package: String }
    /// `database` is the `path` of a `RemoteDB` this session listed for `package`.
    struct PullParams: Codable, Sendable { var id: UUID; var package: String; var database: String }
    /// `database` is the path the client pulled; the read fails when the session holds another.
    struct RowsParams: Codable, Sendable {
        var id: UUID
        var database: String
        var table: String
        var limit: Int?
        var offset: Int?
    }
    struct QueryParams: Codable, Sendable { var id: UUID; var database: String; var sql: String }
    struct SessionInfo: Codable, Sendable, Equatable {
        var id: UUID
        /// False when this open created the session (the daemon didn't have it).
        var existed: Bool
    }

    static let defaultLimit = 100
    static let maxLimit = 1_000
    /// Most rows `database.query` returns.
    static let maxQueryRows = 50_000
    /// Most cell text one `database.rows` or `database.query` result holds.
    static let maxResultBytes = 64 * 1_048_576
    /// Seconds one read may run.
    static let readDeadline: TimeInterval = 30

    static let readLimits = DBReadLimits(maxRows: maxQueryRows, maxBytes: maxResultBytes, deadline: readDeadline)

    /// The page a `database.rows` call reads: `limit` within 1...`maxLimit`, `offset` never negative.
    static func page(limit: Int?, offset: Int?) -> (limit: Int, offset: Int) {
        (min(max(limit ?? defaultLimit, 1), maxLimit), max(offset ?? 0, 0))
    }

    /// What a client is told when a result went over a cap.
    static func limitError(_ exceeded: DBLimitExceeded) -> RPCError {
        switch exceeded {
        case .rows(let max):
            return .failed("The result has more than \(max) rows. Add a LIMIT to the query.")
        case .bytes(let max):
            return .failed("The result is larger than \(max / 1_048_576) MB. Select fewer rows or columns.")
        }
    }

    /// A package that will reach a device shell: valid and not empty.
    static func requirePackage(_ package: String) throws {
        try DaemonInput.package(package)
        guard !package.isEmpty else { throw RPCError.invalidParams("Not a package or bundle id: \(package)") }
    }

    /// The `jaca-db-<UUID>` directory `DatabaseService.pull` created for a pulled file. nil for a
    /// file anywhere else, so closing a session can only delete a directory a pull made.
    static func pullDirectory(of localDB: URL) -> URL? {
        let dir = localDB.deletingLastPathComponent()
        return dir.lastPathComponent.hasPrefix("jaca-db-") ? dir : nil
    }

    static func removePull(_ localDB: URL?) {
        guard let localDB, let dir = pullDirectory(of: localDB) else { return }
        try? FileManager.default.removeItem(at: dir)
    }

    @MainActor
    final class Registry {
        struct Pulled {
            var package: String
            var path: String
            var local: URL
        }

        final class Hosted {
            let device: Device
            /// What `database.databases` last returned for each package: the only databases a
            /// pull accepts.
            var listings: [String: [RemoteDB]] = [:]
            var pulled: Pulled?
            var lastUsed: Date
            /// Reads still running, interrupted when the session closes.
            var reads: [DBReadToken] = []
            init(device: Device, now: Date) {
                self.device = device
                self.lastUsed = now
            }
        }

        typealias ListDatabases = @MainActor (Device, String) async throws -> [RemoteDB]
        typealias Pull = @MainActor (Device, RemoteDB, String) async throws -> URL

        private(set) var sessions: [UUID: Hosted] = [:]
        let orphanTimeout: TimeInterval
        let limits: DBReadLimits
        private let listDatabases: ListDatabases
        private let pullDatabase: Pull
        /// Reads the pulled copy; it runs no process, so it needs no adb.
        private let reader = DatabaseService(adbURL: nil)

        /// `listDatabases` and `pull` default to `DatabaseService` with the configured adb.
        init(orphanTimeout: TimeInterval = 600, limits: DBReadLimits = DatabaseArea.readLimits,
             listDatabases: ListDatabases? = nil, pull: Pull? = nil) {
            self.orphanTimeout = orphanTimeout
            self.limits = limits
            self.listDatabases = listDatabases ?? { device, package in
                try await Registry.service().listDatabases(device: device, appID: package)
            }
            self.pullDatabase = pull ?? { device, db, package in
                try await Registry.service().pull(device: device, db: db, appID: package)
            }
        }

        /// Resolved per call, so a changed adb path setting applies to the next one. The service's
        /// own work (adb, file copies) runs off the main actor.
        private static func service() -> DatabaseService {
            DatabaseService(adbURL: AndroidToolchain.adbURL(override: JacaDefaults.shared.string(forKey: DevicesEngine.adbPathKey)))
        }

        var isBusy: Bool { !sessions.isEmpty }

        func open(_ p: OpenParams, now: Date = Date()) -> SessionInfo {
            if let existing = sessions[p.id] {
                existing.lastUsed = now
                return SessionInfo(id: p.id, existed: true)
            }
            sessions[p.id] = Hosted(device: p.device, now: now)
            return SessionInfo(id: p.id, existed: false)
        }

        func databases(_ p: DatabasesParams) async throws -> [RemoteDB] {
            try await use(p.id) { hosted in
                let list = try await self.listDatabases(hosted.device, p.package)
                hosted.listings[p.package] = list
                return list
            }
        }

        func pull(_ p: PullParams) async throws -> [DBTable] {
            try await use(p.id) { hosted in
                guard let db = hosted.listings[p.package]?.first(where: { $0.path == p.database }) else {
                    throw RPCError.failed("No database \(p.database) in session \(p.id.uuidString).")
                }
                let local = try await self.pullDatabase(hosted.device, db, p.package)
                let reader = self.reader
                let tables: [DBTable]
                do {
                    tables = try await Task.detached { try reader.tables(localDB: local) }.value
                } catch {
                    DatabaseArea.removePull(local)
                    throw error
                }
                // Closed while pulling: nothing owns the copy any more.
                guard self.sessions[p.id] === hosted else {
                    DatabaseArea.removePull(local)
                    throw RPCError.failed("No database session \(p.id.uuidString).")
                }
                if hosted.pulled?.local != local { DatabaseArea.removePull(hosted.pulled?.local) }
                hosted.pulled = Pulled(package: p.package, path: p.database, local: local)
                return tables
            }
        }

        func rows(_ p: RowsParams) async throws -> DBResultSet {
            try await use(p.id) { hosted in
                let local = try self.pulled(hosted, p.id, database: p.database)
                let page = DatabaseArea.page(limit: p.limit, offset: p.offset)
                // The page bounds the rows; only the size and the time are left to cap.
                var limits = self.limits
                limits.maxRows = nil
                let reader = self.reader, table = p.table, bounds = limits
                return try await self.read(hosted) { token in
                    try reader.rows(localDB: local, table: table, limit: page.limit, offset: page.offset,
                                    limits: bounds, token: token)
                }
            }
        }

        func query(_ p: QueryParams) async throws -> DBResultSet {
            try await use(p.id) { hosted in
                let local = try self.pulled(hosted, p.id, database: p.database)
                guard DatabaseService.isReadOnly(p.sql) else { throw DBError.readOnly }
                let reader = self.reader, sql = p.sql, limits = self.limits
                return try await self.read(hosted) { token in
                    try reader.query(localDB: local, sql: sql, limits: limits, token: token)
                }
            }
        }

        func close(_ id: UUID) -> Bool {
            guard let hosted = sessions.removeValue(forKey: id) else { return false }
            hosted.reads.forEach { $0.cancel() }
            DatabaseArea.removePull(hosted.pulled?.local)
            hosted.pulled = nil
            return true
        }

        /// Closes sessions no call has started or finished for `orphanTimeout`, interrupting a
        /// read still running in one.
        func reapIdle(now: Date = Date()) {
            for (id, hosted) in sessions where now.timeIntervalSince(hosted.lastUsed) >= orphanTimeout {
                DaemonLog.info("closing database session \(id): unused for \(orphanTimeout)s")
                _ = close(id)
            }
        }

        /// The session's copy, when it is the database the caller expects.
        private func pulled(_ hosted: Hosted, _ id: UUID, database: String) throws -> URL {
            guard let pulled = hosted.pulled, pulled.path == database else {
                throw RPCError.failed("No database pulled in session \(id.uuidString).")
            }
            return pulled.local
        }

        /// Runs one bounded read off the main actor. Closing the session interrupts it.
        private func read(_ hosted: Hosted,
                          _ work: @escaping @Sendable (DBReadToken) throws -> DBResultSet) async throws -> DBResultSet {
            let token = DBReadToken()
            hosted.reads.append(token)
            defer { hosted.reads.removeAll { $0 === token } }
            do {
                return try await Task.detached { try work(token) }.value
            } catch let exceeded as DBLimitExceeded {
                throw DatabaseArea.limitError(exceeded)
            }
        }

        /// Runs one call on a session, marking it used when the call starts and when it ends.
        private func use<R>(_ id: UUID, _ body: @MainActor (Hosted) async throws -> R) async throws -> R {
            guard let hosted = sessions[id] else { throw RPCError.failed("No database session \(id.uuidString).") }
            hosted.lastUsed = Date()
            defer { hosted.lastUsed = Date() }
            return try await body(hosted)
        }
    }

    @MainActor
    static func install(on server: DaemonServer, registry: Registry? = nil) {
        let registry = registry ?? Registry(orphanTimeout: DaemonDefaults.orphanTimeout)
        server.keep(registry)
        server.addBusyCheck("database") { registry.isBusy }
        server.keep(TaskBox(Task { @MainActor [weak registry] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(min(60, max(1, registry?.orphanTimeout ?? 60) / 4)))
                registry?.reapIdle()
            }
        }))

        let r = server.router
        r.register("database.open", "Opens (or attaches to, by id) a database session for a device.",
                   params: OpenParams.self) { p, _ in
            try DaemonInput.device(p.device)
            return await registry.open(p)
        }
        r.register("database.databases", "An app's SQLite databases on the session's device. Each is {name, path}.",
                   params: DatabasesParams.self, concurrent: true) { p, _ in
            try DatabaseArea.requirePackage(p.package)
            return try await registry.databases(p)
        }
        r.register("database.pull", "Copies a database listed for the package (by its path) off the device, replacing the session's copy. Returns its tables as {name, rowCount}.",
                   params: PullParams.self, concurrent: true) { p, _ in
            try DatabaseArea.requirePackage(p.package)
            return try await registry.pull(p)
        }
        r.register("database.rows", "A page of a table from the pulled database, named by its path (limit 1...1000, offset from 0). Returns {columns, rows}; a null cell is SQL NULL.",
                   params: RowsParams.self, concurrent: true) { p, _ in
            try await registry.rows(p)
        }
        r.register("database.query", "Runs read-only SQL (SELECT / WITH / PRAGMA / EXPLAIN) over the pulled database, named by its path. Returns {columns, rows}, at most 50000 rows.",
                   params: QueryParams.self, concurrent: true) { p, _ in
            try await registry.query(p)
        }
        r.register("database.close", "Closes a session and deletes its pulled copy. Returns whether it existed.",
                   params: IDParams.self) { p, _ in
            await registry.close(p.id)
        }
    }
}
