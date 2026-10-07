import XCTest
@testable import Jaca

/// The database area: wire formats, the session guards, and reads of a pulled copy. The device
/// side (adb, simctl) is replaced by closures that hand out SQLite files built here.
@MainActor
final class DatabaseAreaTests: XCTestCase {
    private let device = Device(id: "emu-1", platform: .android, model: "Pixel", state: .connected)
    private let listed = RemoteDB(name: "app.db", path: "databases/app.db")
    private var made: [URL] = []

    override func tearDown() async throws {
        for dir in made { try? FileManager.default.removeItem(at: dir) }
        made = []
    }

    /// A two-row database inside its own `jaca-db-…` directory, as `DatabaseService.pull` leaves one.
    private func makeDB(_ sql: String = "CREATE TABLE people(id INTEGER, name TEXT, note TEXT); "
                        + "INSERT INTO people VALUES (1,'Ada',NULL); INSERT INTO people VALUES (2,'Grace','hi');") throws -> URL {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("jaca-db-test-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        made.append(dir)
        let url = dir.appendingPathComponent("app.db")
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sqlite3")
        p.arguments = [url.path, sql]
        try p.run(); p.waitUntilExit()
        try XCTSkipUnless(p.terminationStatus == 0, "sqlite3 CLI unavailable")
        return url
    }

    /// A registry whose device lists `listed` and whose pulls hand out `copies` in order.
    private func registry(copies: [URL] = [], orphanTimeout: TimeInterval = 600,
                          limits: DBReadLimits = DatabaseArea.readLimits,
                          onPull: @escaping @MainActor (RemoteDB, String) -> Void = { _, _ in }) -> DatabaseArea.Registry {
        var remaining = copies
        let listed = self.listed
        return DatabaseArea.Registry(
            orphanTimeout: orphanTimeout, limits: limits,
            listDatabases: { _, _ in [listed] },
            pull: { _, db, package in
                onPull(db, package)
                guard !remaining.isEmpty else { throw DBError.command("couldn't pull the database") }
                return remaining.removeFirst()
            })
    }

    private func assertFails<T>(_ expected: RPCError, _ call: () async throws -> T,
                                file: StaticString = #filePath, line: UInt = #line) async {
        do {
            _ = try await call()
            XCTFail("expected \(expected.message)", file: file, line: line)
        } catch let error as RPCError {
            XCTAssertEqual(error, expected, file: file, line: line)
        } catch {
            XCTFail("unexpected \(error)", file: file, line: line)
        }
    }

    // MARK: - Wire formats

    func test_remoteDB_encodesNameAndPathOnly() throws {
        let data = try JSONEncoder.daemon.encode([listed])
        XCTAssertEqual(String(decoding: data, as: UTF8.self), #"[{"name":"app.db","path":"databases/app.db"}]"#)
        XCTAssertEqual(try JSONDecoder.daemon.decode([RemoteDB].self, from: data), [listed])
    }

    func test_dbTable_encodesNameAndRowCountOnly() throws {
        let table = DBTable(name: "people", rowCount: 2)
        let data = try JSONEncoder.daemon.encode([table])
        XCTAssertEqual(String(decoding: data, as: UTF8.self), #"[{"name":"people","rowCount":2}]"#)
        XCTAssertEqual(try JSONDecoder.daemon.decode([DBTable].self, from: data), [table])
    }

    func test_resultSet_encodesNullCells() throws {
        let rs = DBResultSet(columns: ["id", "note"], rows: [["1", nil]])
        let data = try JSONEncoder.daemon.encode(rs)
        XCTAssertEqual(String(decoding: data, as: UTF8.self), #"{"columns":["id","note"],"rows":[["1",null]]}"#)
    }

    func test_sessionInfo_encodesIdAndExisted() throws {
        let id = UUID()
        let data = try JSONEncoder.daemon.encode(DatabaseArea.SessionInfo(id: id, existed: false))
        XCTAssertEqual(String(decoding: data, as: UTF8.self), #"{"existed":false,"id":"\#(id.uuidString)"}"#)
    }

    // MARK: - Pure logic

    func test_page_clampsLimitAndOffset() {
        XCTAssertTrue(DatabaseArea.page(limit: 50, offset: 100) == (50, 100))
        XCTAssertTrue(DatabaseArea.page(limit: 0, offset: -5) == (1, 0))
        XCTAssertTrue(DatabaseArea.page(limit: -3, offset: 0) == (1, 0))
        XCTAssertTrue(DatabaseArea.page(limit: 5_000, offset: 7) == (1_000, 7))
        XCTAssertTrue(DatabaseArea.page(limit: nil, offset: nil) == (100, 0))
    }

    func test_pullDirectory_isOnlyAPullsOwnDirectory() {
        let pulled = URL(fileURLWithPath: "/tmp/jaca-db-ABC/app.db")
        XCTAssertEqual(DatabaseArea.pullDirectory(of: pulled)?.path, "/tmp/jaca-db-ABC")
        XCTAssertNil(DatabaseArea.pullDirectory(of: URL(fileURLWithPath: "/Users/me/Documents/app.db")))
        XCTAssertNil(DatabaseArea.pullDirectory(of: URL(fileURLWithPath: "/app.db")))
    }

    // MARK: - Sessions

    func test_open_isIdempotentById() {
        let reg = registry()
        let id = UUID()
        XCTAssertEqual(reg.open(.init(id: id, device: device)), .init(id: id, existed: false))
        XCTAssertEqual(reg.open(.init(id: id, device: device)), .init(id: id, existed: true))
        XCTAssertEqual(reg.sessions.count, 1)
        XCTAssertTrue(reg.isBusy)
        XCTAssertTrue(reg.close(id))
        XCTAssertFalse(reg.close(id))
        XCTAssertFalse(reg.isBusy)
    }

    func test_unknownSession_failsEveryCall() async {
        let reg = registry()
        let id = UUID()
        let expected = RPCError.failed("No database session \(id.uuidString).")
        await assertFails(expected) { try await reg.databases(.init(id: id, package: "com.example")) }
        await assertFails(expected) { try await reg.pull(.init(id: id, package: "com.example", database: "databases/app.db")) }
        await assertFails(expected) { try await reg.rows(.init(id: id, database: listed.path, table: "people", limit: 10, offset: 0)) }
        await assertFails(expected) { try await reg.query(.init(id: id, database: listed.path, sql: "SELECT 1")) }
    }

    func test_pull_refusesAPathTheSessionDidNotList() async throws {
        var pulls = 0
        let reg = registry(copies: [try makeDB()], onPull: { _, _ in pulls += 1 })
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))

        // Nothing listed yet: even the real path is refused.
        await assertFails(.failed("No database databases/app.db in session \(id.uuidString).")) {
            try await reg.pull(.init(id: id, package: "com.example", database: "databases/app.db"))
        }
        _ = try await reg.databases(.init(id: id, package: "com.example"))
        let path = "databases/app.db; rm -rf /"
        await assertFails(.failed("No database \(path) in session \(id.uuidString).")) {
            try await reg.pull(.init(id: id, package: "com.example", database: path))
        }
        XCTAssertEqual(pulls, 0, "an unlisted path never reaches the device")
    }

    func test_rowsAndQuery_needAPulledDatabase() async throws {
        let reg = registry()
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))
        let expected = RPCError.failed("No database pulled in session \(id.uuidString).")
        await assertFails(expected) { try await reg.rows(.init(id: id, database: listed.path, table: "people", limit: 10, offset: 0)) }
        await assertFails(expected) { try await reg.query(.init(id: id, database: listed.path, sql: "SELECT 1")) }
    }

    func test_pullThenRowsAndQuery_readTheCopy() async throws {
        var pulled: [(RemoteDB, String)] = []
        let reg = registry(copies: [try makeDB()], onPull: { pulled.append(($0, $1)) })
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))
        let dbs = try await reg.databases(.init(id: id, package: "com.example"))
        XCTAssertEqual(dbs, [listed])

        let tables = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
        XCTAssertEqual(tables, [DBTable(name: "people", rowCount: 2)])
        XCTAssertEqual(pulled.map(\.0), [listed])
        XCTAssertEqual(pulled.map(\.1), ["com.example"])

        let page = try await reg.rows(.init(id: id, database: listed.path, table: "people", limit: 1, offset: 1))
        XCTAssertEqual(page, DBResultSet(columns: ["id", "name", "note"], rows: [["2", "Grace", "hi"]]))
        // A limit of 0 reads one row and a negative offset reads from the start.
        let clamped = try await reg.rows(.init(id: id, database: listed.path, table: "people", limit: 0, offset: -4))
        XCTAssertEqual(clamped.rows, [["1", "Ada", nil]])

        let counted = try await reg.query(.init(id: id, database: listed.path, sql: "SELECT count(*) AS n FROM people"))
        XCTAssertEqual(counted, DBResultSet(columns: ["n"], rows: [["2"]]))
    }

    func test_query_rejectsWrites() async throws {
        let reg = registry(copies: [try makeDB()])
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))
        _ = try await reg.databases(.init(id: id, package: "com.example"))
        _ = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
        do {
            _ = try await reg.query(.init(id: id, database: listed.path, sql: "DELETE FROM people"))
            XCTFail("expected a refusal")
        } catch DBError.readOnly {
        } catch {
            XCTFail("unexpected \(error)")
        }
        let rows = try await reg.query(.init(id: id, database: listed.path, sql: "SELECT id FROM people"))
        XCTAssertEqual(rows.rows.count, 2, "the refused statement changed nothing")
    }

    /// A listing belongs to its package: a path listed for one app is not pullable for another.
    func test_pull_looksThePathUpInItsOwnPackagesListing() async throws {
        var pulled: [String] = []
        let reg = registry(copies: [try makeDB()], onPull: { pulled.append($1) })
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))
        _ = try await reg.databases(.init(id: id, package: "com.example"))
        await assertFails(.failed("No database \(listed.path) in session \(id.uuidString).")) {
            try await reg.pull(.init(id: id, package: "com.other", database: self.listed.path))
        }
        XCTAssertEqual(pulled, [])

        // Listing a second package keeps the first one's listing.
        _ = try await reg.databases(.init(id: id, package: "com.other"))
        _ = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
        XCTAssertEqual(pulled, ["com.example"])
        XCTAssertEqual(reg.sessions[id]?.pulled?.package, "com.example")
        XCTAssertEqual(reg.sessions[id]?.pulled?.path, listed.path)
    }

    /// A read names the database it expects; when the session holds another one it reads nothing.
    func test_rowsAndQuery_refuseADatabaseThatIsNotThePulledOne() async throws {
        let reg = registry(copies: [try makeDB()])
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))
        _ = try await reg.databases(.init(id: id, package: "com.example"))
        _ = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
        let expected = RPCError.failed("No database pulled in session \(id.uuidString).")
        await assertFails(expected) {
            try await reg.rows(.init(id: id, database: "databases/other.db", table: "people", limit: 10, offset: 0))
        }
        await assertFails(expected) {
            try await reg.query(.init(id: id, database: "databases/other.db", sql: "SELECT 1"))
        }
        await assertFails(expected) { try await reg.query(.init(id: id, database: "", sql: "SELECT 1")) }
    }

    // MARK: - Limits

    private static let endless = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) "

    /// A session with `sql`'s database pulled, read under `limits`.
    private func pulledSession(limits: DBReadLimits, sql: String? = nil) async throws -> (DatabaseArea.Registry, UUID) {
        let reg = registry(copies: [try sql.map { try makeDB($0) } ?? makeDB()], limits: limits)
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))
        _ = try await reg.databases(.init(id: id, package: "com.example"))
        _ = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
        return (reg, id)
    }

    func test_defaultLimits_andTheirMessages() {
        XCTAssertEqual(DatabaseArea.readLimits, DBReadLimits(maxRows: 50_000, maxBytes: 67_108_864, deadline: 30))
        XCTAssertEqual(DatabaseArea.limitError(.rows(DatabaseArea.maxQueryRows)),
                       .failed("The result has more than 50000 rows. Add a LIMIT to the query."))
        XCTAssertEqual(DatabaseArea.limitError(.bytes(DatabaseArea.maxResultBytes)),
                       .failed("The result is larger than 64 MB. Select fewer rows or columns."))
    }

    func test_limits_exceeded_isOverTheCapNotAtIt() {
        let limits = DBReadLimits(maxRows: 3, maxBytes: 10, deadline: 1)
        XCTAssertNil(limits.exceeded(rows: 3, bytes: 10))
        XCTAssertEqual(limits.exceeded(rows: 4, bytes: 0), .rows(3))
        XCTAssertEqual(limits.exceeded(rows: 1, bytes: 11), .bytes(10))
        XCTAssertNil(DBReadLimits(maxRows: nil, maxBytes: 10, deadline: 1).exceeded(rows: 1_000_000, bytes: 10))
        XCTAssertEqual(DBReadLimits.bytes(in: ["abc", nil, "é"]), 5)
    }

    func test_query_overTheRowCap_fails() async throws {
        let (reg, id) = try await pulledSession(limits: DBReadLimits(maxRows: 10, maxBytes: 1_000_000, deadline: 30))
        await assertFails(DatabaseArea.limitError(.rows(10))) {
            try await reg.query(.init(id: id, database: self.listed.path, sql: Self.endless + "SELECT x FROM c"))
        }
        // At the cap is fine, and so is an ordinary query.
        let atCap = try await reg.query(.init(id: id, database: listed.path, sql: Self.endless + "SELECT x FROM c LIMIT 10"))
        XCTAssertEqual(atCap.rows.count, 10)
        let people = try await reg.query(.init(id: id, database: listed.path, sql: "SELECT name FROM people ORDER BY id"))
        XCTAssertEqual(people, DBResultSet(columns: ["name"], rows: [["Ada"], ["Grace"]]))
    }

    func test_rowsAndQuery_overTheByteCap_fail() async throws {
        let big = "CREATE TABLE big(v TEXT); " + (1...4).map { _ in "INSERT INTO big VALUES (hex(zeroblob(2000)));" }.joined(separator: " ")
        let (reg, id) = try await pulledSession(limits: DBReadLimits(maxRows: 1_000, maxBytes: 10_000, deadline: 30), sql: big)
        // Four cells of 4000 characters: two fit, the third goes over.
        let expected = DatabaseArea.limitError(.bytes(10_000))
        await assertFails(expected) {
            try await reg.rows(.init(id: id, database: self.listed.path, table: "big", limit: 100, offset: 0))
        }
        await assertFails(expected) {
            try await reg.query(.init(id: id, database: self.listed.path, sql: "SELECT v FROM big"))
        }
        let two = try await reg.rows(.init(id: id, database: listed.path, table: "big", limit: 2, offset: 0))
        XCTAssertEqual(two.rows.count, 2)
    }

    /// A statement that never yields a row is interrupted at the deadline, with SQLite's message.
    func test_query_pastTheDeadline_isInterrupted() async throws {
        let (reg, id) = try await pulledSession(limits: DBReadLimits(maxRows: 10, maxBytes: 1_000_000, deadline: 0.2))
        let started = Date()
        do {
            _ = try await reg.query(.init(id: id, database: listed.path, sql: Self.endless + "SELECT count(*) FROM c"))
            XCTFail("expected an interrupt")
        } catch let error as DBError {
            XCTAssertEqual(error.errorDescription, "SQLite: interrupted")
        }
        XCTAssertLessThan(Date().timeIntervalSince(started), 10)
        XCTAssertEqual(reg.sessions[id]?.reads.count, 0)
        // The session still reads.
        let one = try await reg.query(.init(id: id, database: listed.path, sql: "SELECT 1 AS one"))
        XCTAssertEqual(one.rows, [["1"]])
    }

    func test_close_interruptsARunningRead() async throws {
        let (reg, id) = try await pulledSession(limits: DBReadLimits(maxRows: 10, maxBytes: 1_000_000, deadline: 120))
        let path = listed.path
        let read = Task { @MainActor in
            try await reg.query(.init(id: id, database: path, sql: Self.endless + "SELECT count(*) FROM c"))
        }
        let deadline = ContinuousClock.now + .seconds(3)
        while reg.sessions[id]?.reads.isEmpty != false {
            guard ContinuousClock.now < deadline else { return XCTFail("the read never started") }
            try await Task.sleep(for: .milliseconds(10))
        }
        XCTAssertTrue(reg.close(id))
        do {
            _ = try await read.value
            XCTFail("expected an interrupt")
        } catch let error as DBError {
            XCTAssertEqual(error.errorDescription, "SQLite: interrupted")
        }
    }

    func test_reapIdle_interruptsAReadInAnIdleSession() async throws {
        let (reg, id) = try await pulledSession(limits: DBReadLimits(maxRows: 10, maxBytes: 1_000_000, deadline: 120))
        let path = listed.path
        let read = Task { @MainActor in
            try await reg.query(.init(id: id, database: path, sql: Self.endless + "SELECT count(*) FROM c"))
        }
        let deadline = ContinuousClock.now + .seconds(3)
        while reg.sessions[id]?.reads.isEmpty != false {
            guard ContinuousClock.now < deadline else { return XCTFail("the read never started") }
            try await Task.sleep(for: .milliseconds(10))
        }
        reg.reapIdle(now: Date().addingTimeInterval(601))
        XCTAssertNil(reg.sessions[id])
        do {
            _ = try await read.value
            XCTFail("expected an interrupt")
        } catch let error as DBError {
            XCTAssertEqual(error.errorDescription, "SQLite: interrupted")
        }
    }

    /// `DatabaseService.pull` removes the directory it made when the pull fails.
    func test_failedServicePull_leavesNoDirectoryBehind() async throws {
        func pullDirs() -> Set<String> {
            let names = (try? FileManager.default.contentsOfDirectory(atPath: FileManager.default.temporaryDirectory.path)) ?? []
            // `jaca-db-` and a UUID, nothing else: the directories this file makes carry a `test-` infix.
            return Set(names.filter { $0.hasPrefix("jaca-db-") && UUID(uuidString: String($0.dropFirst(8))) != nil })
        }
        let before = pullDirs()
        let simulator = Device(id: "SIM-1", platform: .iosSimulator, model: "iPhone", state: .booted)
        let missing = RemoteDB(name: "gone.db", path: "/nonexistent-\(UUID().uuidString)/gone.db")
        do {
            _ = try await DatabaseService(adbURL: nil).pull(device: simulator, db: missing, appID: "com.example")
            XCTFail("expected the pull to fail")
        } catch let error as DBError {
            XCTAssertEqual(error.errorDescription, "the pulled database was empty")
        }
        XCTAssertEqual(pullDirs().subtracting(before), [])
    }

    // MARK: - Temp dirs

    func test_secondPull_deletesThePreviousCopy_andCloseDeletesTheLast() async throws {
        let first = try makeDB(), second = try makeDB()
        let reg = registry(copies: [first, second])
        let id = UUID()
        _ = reg.open(.init(id: id, device: device))
        _ = try await reg.databases(.init(id: id, package: "com.example"))

        _ = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: first.path))
        _ = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: first.deletingLastPathComponent().path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: second.path))

        // A failed pull keeps the copy the session already has.
        do {
            _ = try await reg.pull(.init(id: id, package: "com.example", database: listed.path))
            XCTFail("expected the pull to fail")
        } catch let error as DBError {
            XCTAssertEqual(error.errorDescription, "couldn't pull the database")
        }
        XCTAssertTrue(FileManager.default.fileExists(atPath: second.path))
        _ = try await reg.rows(.init(id: id, database: listed.path, table: "people", limit: 1, offset: 0))

        XCTAssertTrue(reg.close(id))
        XCTAssertFalse(FileManager.default.fileExists(atPath: second.deletingLastPathComponent().path))
    }

    func test_reapIdle_closesSessionsUnusedForTheTimeout() async throws {
        let copy = try makeDB()
        let reg = registry(copies: [copy], orphanTimeout: 60)
        let idle = UUID(), used = UUID()
        let start = Date()
        _ = reg.open(.init(id: idle, device: device), now: start)
        _ = try await reg.databases(.init(id: idle, package: "com.example"))
        _ = try await reg.pull(.init(id: idle, package: "com.example", database: listed.path))

        reg.reapIdle(now: Date().addingTimeInterval(59))
        XCTAssertNotNil(reg.sessions[idle], "not idle long enough")

        // A call (here an open by id) marks the session used.
        _ = reg.open(.init(id: used, device: device), now: Date().addingTimeInterval(100))
        reg.reapIdle(now: Date().addingTimeInterval(120))
        XCTAssertNil(reg.sessions[idle])
        XCTAssertNotNil(reg.sessions[used])
        XCTAssertFalse(FileManager.default.fileExists(atPath: copy.deletingLastPathComponent().path))
    }

    // MARK: - Over the socket

    func test_overTheSocket_resultsAndErrorsKeepTheirShape() async throws {
        let daemon = try TestDaemon()
        let reg = registry(copies: [try makeDB()])
        DatabaseArea.install(on: daemon.server, registry: reg)
        let client = try await daemon.client()
        let id = UUID()

        let opened: DatabaseArea.SessionInfo = try await client.call("database.open", DatabaseArea.OpenParams(id: id, device: device))
        XCTAssertEqual(opened, .init(id: id, existed: false))
        let dbs: [RemoteDB] = try await client.call("database.databases", DatabaseArea.DatabasesParams(id: id, package: "com.example"))
        XCTAssertEqual(dbs, [listed])
        let tables: [DBTable] = try await client.call("database.pull", DatabaseArea.PullParams(id: id, package: "com.example", database: listed.path))
        XCTAssertEqual(tables, [DBTable(name: "people", rowCount: 2)])

        let raw = try await client.callRaw(
            "database.rows", paramsJSON: Data(#"{"id":"\#(id.uuidString)","database":"databases/app.db","table":"people","limit":1,"offset":0}"#.utf8))
        XCTAssertTrue(String(decoding: raw, as: UTF8.self).contains(#""result":{"columns":["id","name","note"],"rows":[["1","Ada",null]]}"#),
                      String(decoding: raw, as: UTF8.self))

        // A service error reaches the client as its own description.
        await assertFails(.failed(DBError.readOnly.errorDescription ?? "")) {
            try await client.call("database.query", DatabaseArea.QueryParams(id: id, database: listed.path, sql: "DROP TABLE people"), as: DBResultSet.self)
        }
        do {
            _ = try await client.call("database.query", DatabaseArea.QueryParams(id: id, database: listed.path, sql: "SELECT * FROM missing"), as: DBResultSet.self)
            XCTFail("expected an error")
        } catch let error as RPCError {
            XCTAssertEqual(error.code, RPCError.failedCode)
            XCTAssertTrue(error.message.hasPrefix("SQLite: "), error.message)
        }

        // Ids that would reach a shell are refused before the registry sees them.
        for (method, params) in [
            ("database.open", #"{"id":"\#(UUID().uuidString)","device":{"id":"-s evil","platform":"android","model":"","state":"connected"}}"#),
            ("database.databases", #"{"id":"\#(id.uuidString)","package":"com.x; reboot"}"#),
            ("database.databases", #"{"id":"\#(id.uuidString)","package":""}"#),
            ("database.pull", #"{"id":"\#(id.uuidString)","package":"com.x; reboot","database":"databases/app.db"}"#),
            ("database.pull", #"{"id":"\#(id.uuidString)","package":"","database":"databases/app.db"}"#),
            // The pre-review shapes: a pull without its package, a read without its database.
            ("database.pull", #"{"id":"\#(id.uuidString)","database":"databases/app.db"}"#),
            ("database.rows", #"{"id":"\#(id.uuidString)","table":"people","limit":1,"offset":0}"#),
            ("database.query", #"{"id":"\#(id.uuidString)","sql":"SELECT 1"}"#),
        ] {
            do {
                _ = try await client.callRaw(method, paramsJSON: Data(params.utf8))
                XCTFail("expected \(method) to refuse \(params)")
            } catch let error as RPCError {
                XCTAssertEqual(error.code, RPCError.invalidParamsCode, params)
            }
        }

        let status = await daemon.server.status()
        XCTAssertEqual(status.busy, ["database"])
        let closed: Bool = try await client.call("database.close", DatabaseArea.IDParams(id: id))
        XCTAssertTrue(closed)
        let again: Bool = try await client.call("database.close", DatabaseArea.IDParams(id: id))
        XCTAssertFalse(again)
    }
}
