import Foundation

/// Thread-safe hand-off buffer between the background poll consumer and the main-actor flush
/// loop (the `CloudLogEntry` analogue of `LineBuffer`).
final class CloudEntryBuffer: @unchecked Sendable {
    private let lock = NSLock()
    private var entries: [CloudLogEntry] = []

    func append(_ batch: [CloudLogEntry]) {
        lock.lock(); entries.append(contentsOf: batch); lock.unlock()
    }

    func drain(max: Int) -> [CloudLogEntry] {
        lock.lock(); defer { lock.unlock() }
        if entries.count <= max {
            let out = entries
            entries.removeAll(keepingCapacity: true)
            return out
        }
        let out = Array(entries.prefix(max))
        entries.removeFirst(max)
        return out
    }
}

/// The server-side query a Cloud Logging tab streams: project, log name, structured query,
/// time window, and an optional raw filter from a Logs Explorer URL (which wins when set).
struct CloudStreamConfig: Codable, Equatable, Sendable {
    var projectID: String
    var logName: String?
    var query: CloudLogQuery = CloudLogQuery()
    var timeRange: CloudTimeRange = .last(minutes: 15)
    var rawFilter: String?
}

/// Stream-side state of a Cloud Logging tab. Published by `jacad` on `cloud.sstate.<id>`.
struct CloudStreamState: Codable, Equatable, Sendable {
    var isRunning = false
    /// The backfill query is in flight.
    var isLoading = false
    var statusMessage: String?
    /// A page of older logs is being fetched.
    var olderLoading = false
    /// Whether older logs might still exist (false once a short/empty page comes back).
    var hasMoreOlder = true
    /// Whether the per-session SQL database exists (the session has been started once).
    var hasData = false
}

extension CloudStreamState {
    private enum CodingKeys: String, CodingKey { case isRunning, isLoading, statusMessage, olderLoading, hasMoreOlder, hasData }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            isRunning: try c.decodeIfPresent(Bool.self, forKey: .isRunning) ?? false,
            isLoading: try c.decodeIfPresent(Bool.self, forKey: .isLoading) ?? false,
            statusMessage: try c.decodeIfPresent(String.self, forKey: .statusMessage),
            olderLoading: try c.decodeIfPresent(Bool.self, forKey: .olderLoading) ?? false,
            hasMoreOlder: try c.decodeIfPresent(Bool.self, forKey: .hasMoreOlder) ?? true,
            hasData: try c.decodeIfPresent(Bool.self, forKey: .hasData) ?? false
        )
    }
}

/// Where a `CloudLogSession` tab gets its entries and stream state: an in-process
/// `CloudStreamEngine`, or a session running in `jacad`.
@MainActor
protocol CloudFeed: AnyObject {
    var state: CloudStreamState { get }
    /// New entries from the live poll, oldest first, seq-stamped.
    var onEntries: (([CloudLogEntry]) -> Void)? { get set }
    /// A page of older entries (oldest first, seqs below everything loaded) to prepend.
    var onOlder: (([CloudLogEntry]) -> Void)? { get set }
    var onState: ((CloudStreamState) -> Void)? { get set }
    /// The stream's history was lost (a daemon restart recreated the session): the tab should
    /// drop its scrollback, since new seqs may collide with the old ones.
    var onReset: (() -> Void)? { get set }

    func start(_ config: CloudStreamConfig)
    func stop()
    /// The tab cleared its scrollback: forget loaded ids and the older-page cursor.
    func resetScrollback()
    func loadOlder()
    func clearStatus()
    /// Runs read-only SQL over the captured entries.
    func query(_ sql: String) async throws -> DBResultSet
    /// Stops and deletes the session's database (the tab closed).
    func dispose()
}

/// One Cloud Logging stream with everything that isn't a view: interval polling
/// (`CloudLogPoller`), seq stamping, the per-session SQLite database for SQL mode, label-key
/// detection, and backward pagination. Entries accumulate off-main and are handed out on a
/// ~30ms timer.
@MainActor
final class CloudStreamEngine: CloudFeed {
    let id: UUID
    private(set) var state = CloudStreamState() {
        didSet { if state != oldValue { onState?(state) } }
    }
    var onEntries: (([CloudLogEntry]) -> Void)?
    var onOlder: (([CloudLogEntry]) -> Void)?
    var onState: ((CloudStreamState) -> Void)?
    var onReset: (() -> Void)?   // never fires in-process

    /// Live entries are stamped from a high base so older pages can be assigned strictly-lower
    /// seqs (the ring stays sorted ascending = chronological), with room to spare under the cap.
    nonisolated static let forwardSeqBase: UInt64 = 1 << 40
    nonisolated static let seqStride: UInt64 = 8
    private let olderPageSize = 1_000

    private let cli: @MainActor () -> GcloudCLI?
    private let recordLabels: @MainActor (Set<String>, _ project: String, _ logName: String) -> Void
    private let markUnauthenticated: @MainActor () -> Void
    private let makePoll: (GcloudCLI, CloudStreamConfig) -> AsyncStream<CloudPollEvent>
    private let makeDatabase: (UUID) -> CloudLogDatabase?

    private var config: CloudStreamConfig?
    private let pending = CloudEntryBuffer()
    private let seq: SeqCounter
    private var consumeTask: Task<Void, Never>?
    private var flushTask: Task<Void, Never>?
    /// Holding the stream keeps the poller alive; dropping/cancelling it stops the poll.
    private var pollStream: AsyncStream<CloudPollEvent>?
    private var database: CloudLogDatabase?

    /// Lowest seq assigned so far — the next older page is stamped below this.
    private var oldestSeq = CloudStreamEngine.forwardSeqBase
    /// Timestamp of the oldest entry handed out: the cursor for the next older page.
    private var oldestTimestamp: Date?
    /// Every loaded insertId, so older pages don't re-add an entry already shown.
    private var knownInsertIds = Set<String>()

    init(id: UUID = UUID(),
         seqStart: UInt64 = CloudStreamEngine.forwardSeqBase,
         cli: @escaping @MainActor () -> GcloudCLI?,
         recordLabels: @escaping @MainActor (Set<String>, String, String) -> Void,
         markUnauthenticated: @escaping @MainActor () -> Void,
         makePoll: ((GcloudCLI, CloudStreamConfig) -> AsyncStream<CloudPollEvent>)? = nil,
         makeDatabase: ((UUID) -> CloudLogDatabase?)? = nil) {
        self.id = id
        self.seq = SeqCounter(start: seqStart)
        self.cli = cli
        self.recordLabels = recordLabels
        self.markUnauthenticated = markUnauthenticated
        self.makePoll = makePoll ?? { cli, c in
            CloudLogPoller(cli: cli, project: c.projectID, logName: c.logName, query: c.query,
                           timeRange: c.timeRange, rawFilter: c.rawFilter).stream()
        }
        self.makeDatabase = makeDatabase ?? { CloudLogDatabase(sessionID: $0) }
    }

    // MARK: - Lifecycle

    func start(_ config: CloudStreamConfig) {
        guard !state.isRunning else { return }
        guard let cli = cli() else {
            state.statusMessage = "gcloud isn't installed."
            return
        }
        self.config = config
        if database == nil { database = makeDatabase(id) }
        state.isRunning = true
        state.isLoading = true
        state.statusMessage = nil
        state.hasData = database != nil
        let stream = makePoll(cli, config)
        pollStream = stream
        startFlushLoop()
        consumeTask = Task { [weak self] in await self?.consume(stream) }
    }

    func stop() {
        guard state.isRunning else { return }
        state.isRunning = false
        state.isLoading = false
        consumeTask?.cancel(); consumeTask = nil    // cancels the poller via the stream's onTermination
        flushTask?.cancel(); flushTask = nil
        pollStream = nil
        flush(max: .max)                              // drain anything left
    }

    func clearStatus() { state.statusMessage = nil }

    func resetScrollback() {
        oldestSeq = Self.forwardSeqBase
        oldestTimestamp = nil
        knownInsertIds.removeAll(keepingCapacity: true)
        state.hasMoreOlder = true
        state.olderLoading = false
    }

    /// Stops, then deletes the per-session SQLite file.
    func dispose() {
        stop()
        onEntries = nil
        onOlder = nil
        onState = nil
        let db = database
        database = nil
        Task { await db?.deleteFile() }
    }

    func query(_ sql: String) async throws -> DBResultSet {
        guard let database else { throw DBError.sqlite("No data captured yet — start the session first.") }
        return try await database.query(sql)
    }

    // MARK: - Load older (backward pagination)

    /// Fetches the next page of **older** logs (before the oldest loaded) while the live poll
    /// keeps running. No-ops while a page is in flight, when there's nothing older, or before
    /// anything has loaded.
    func loadOlder() {
        guard !state.olderLoading, state.hasMoreOlder, let cursor = oldestTimestamp,
              let config, let cli = cli() else { return }
        state.olderLoading = true
        let size = olderPageSize
        Task { [weak self] in
            let filter = CloudFilter.build(
                logName: config.logName, time: "timestamp<=\(CloudTimestamp.quote(cursor))",
                query: config.query, rawFilter: config.rawFilter)
            let page = try? await cli.read(project: config.projectID, filter: filter, order: "desc", limit: size)
            self?.appendOlder(page, requested: size)
        }
    }

    /// Integrates a fetched older page (newest-first from gcloud `order=desc`): dedup against
    /// already-loaded ids, stamp strictly-lower seqs (oldest-first), persist, hand out.
    private func appendOlder(_ page: [CloudLogEntry]?, requested: Int) {
        state.olderLoading = false
        guard let page else { return }                       // transient error → user can retry
        if page.count < requested { state.hasMoreOlder = false }
        let freshNewestFirst = page.filter { $0.insertId.isEmpty || !knownInsertIds.contains($0.insertId) }
        guard !freshNewestFirst.isEmpty else { return }
        let assigned = Self.assignOlderSeqs(Array(freshNewestFirst.reversed()), below: oldestSeq, stride: Self.seqStride)
        oldestSeq = assigned.first?.seq ?? oldestSeq
        if let first = assigned.first { oldestTimestamp = min(oldestTimestamp ?? first.timestamp, first.timestamp) }
        for e in assigned where !e.insertId.isEmpty { knownInsertIds.insert(e.insertId) }

        if let database { Task { await database.appendEntries(assigned) } }
        if let config { recordLabels(LabelDetector.keys(in: assigned), config.projectID, config.logName ?? "") }
        onOlder?(assigned)
    }

    /// Assigns strictly-decreasing seqs to an oldest-first page so it slots just below `oldestSeq`
    /// (keeps the ring sorted ascending = chronological). Pure → unit-tested.
    nonisolated static func assignOlderSeqs(_ oldestFirst: [CloudLogEntry], below oldestSeq: UInt64, stride: UInt64) -> [CloudLogEntry] {
        let k = UInt64(oldestFirst.count)
        return oldestFirst.enumerated().map { i, entry in
            var e = entry
            e.seq = oldestSeq - (k - UInt64(i)) * stride
            return e
        }
    }

    // MARK: - Stream consumption

    private func consume(_ stream: AsyncStream<CloudPollEvent>) async {
        let buffer = pending, counter = seq
        for await event in stream {
            switch event {
            case .batch(let entries):
                var stamped = entries
                for i in stamped.indices { stamped[i].seq = counter.next() }
                buffer.append(stamped)
            case .caughtUp:
                state.isLoading = false
            case .error(let error):
                state.statusMessage = error.errorDescription
                state.isLoading = false
                if case .notAuthenticated = error { markUnauthenticated() }
            }
        }
        state.isLoading = false
    }

    private func startFlushLoop() {
        flushTask = Task { [weak self] in
            while let self, self.state.isRunning, !Task.isCancelled {
                try? await Task.sleep(for: .milliseconds(30))
                self.flush()
            }
        }
    }

    private func flush(max: Int = 4_000) {
        let drained = pending.drain(max: max)
        guard !drained.isEmpty else { return }
        // Persist to the per-session SQLite (off main) for the SQL mode.
        if let database { Task { await database.appendEntries(drained) } }
        // Auto-detect label keys, cached globally per project + log name ("" = project-wide).
        if let config { recordLabels(LabelDetector.keys(in: drained), config.projectID, config.logName ?? "") }
        for e in drained where !e.insertId.isEmpty { knownInsertIds.insert(e.insertId) }
        if oldestTimestamp == nil, let first = drained.first { oldestTimestamp = first.timestamp }
        onEntries?(drained)
    }
}
