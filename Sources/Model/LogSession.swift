import Foundation
import Observation

/// One tab: a single running (or stopped) log stream bound to a device + filter,
/// with an editable display name. The stream itself (source, reconnects, markers, PID
/// tracking, prettifying, history) is a `LogFeed`: an in-process `LogStreamEngine`, or a
/// session running in `jacad`. This type is the view: the scrollback ring, the tab's filter,
/// the visible slice and display rows, crash navigation. Batches arrive on a ~30ms cadence.
@MainActor
@Observable
final class LogSession: WorkspaceTab {
    let id: UUID
    var displayName: String {
        didSet {
            onStateChanged?()
            feed.rename(displayName)
        }
    }
    let device: Device

    /// Called when persisted state (filter/package/name) changes, so the open-tabs
    /// snapshot is saved immediately rather than only on quit.
    var onStateChanged: (() -> Void)?

    /// Shared per-device state (installed-app list), set by AppModel and reused
    /// across all tabs for this device — see `DeviceContext`.
    var deviceContext: DeviceContext?

    private(set) var filter: LogFilter
    private(set) var isRunning = false
    private(set) var isConnecting = false
    /// Whether the stream runs in `jacad` (daemon mode) rather than in-process.
    let isRemote: Bool
    private(set) var visible: [LogLine] = []
    /// Maps the virtualized list's fixed-height **display rows** to `visible` entries:
    /// a log with embedded `\n`s spans several rows. Kept in lockstep with `visible`
    /// so the table can render multi-line logs without losing the uniform-row fast path.
    private(set) var displayMap = DisplayLineMap()
    /// Cumulative display rows trimmed off the front (the row-unit analogue of
    /// `droppedCount`), so the list can shift the viewport to stay put through a trim.
    private(set) var droppedDisplayRows = 0
    private(set) var totalCount = 0
    private(set) var droppedCount = 0
    /// Bumped whenever `visible` is replaced wholesale (clear / filter change) — as
    /// opposed to an append. The virtualized list uses it to choose a full reload vs
    /// a cheap row-count update.
    private(set) var listEpoch = 0
    /// Seqs of detected crashes (FATAL EXCEPTION / native fatal), oldest→newest.
    private(set) var crashSeqs: [UInt64] = []
    /// Which crash the up/down navigation is currently on (nil = none selected yet).
    private(set) var crashCursor: Int?
    var crashCount: Int { crashSeqs.count }
    var lastCrashSeq: UInt64? { crashSeqs.last }
    /// Set to a line seq to request the list scroll to it (crash navigation).
    var scrollTarget: UInt64?
    var followTail = true
    private(set) var statusMessage: String?

    /// Seqs of prettified response bodies the user has collapsed to a single line
    /// (double-click toggles). Default is expanded; only huge payloads are usually
    /// folded. Read by the render path via `displayMessage`/`effectiveLineCount`.
    private(set) var collapsedBodies: Set<UInt64> = []
    /// Invoked each time the stream starts (so history recording works whether the
    /// tab is auto-started or started later by the user). In-process streams only: a
    /// daemon session records its own history.
    var onStarted: (() -> Void)? {
        didSet { (feed as? LogStreamEngine)?.onStarted = onStarted }
    }

    /// Subtitle for the tab/strip: device model + active filter summary.
    var subtitle: String {
        var parts = [device.displayModel]
        if !filter.packageLabel.isEmpty { parts.append(filter.packageLabel) }
        if !filter.query.isEmpty { parts.append("“\(filter.query)”") }
        if filter.minLevel != .verbose { parts.append("≥\(filter.minLevel.short)") }
        return parts.joined(separator: " · ")
    }

    // MARK: - Display rows (multi-line)

    /// Total fixed-height rows the list renders (≥ `visible.count`; multi-line logs
    /// contribute more than one).
    var displayRowCount: Int { displayMap.totalRows }

    /// The `(logIndex, subLine)` a display row maps to. Callers gate on `displayRowCount`.
    func locate(displayRow row: Int) -> (log: Int, sub: Int) { displayMap.locate(row: row) }

    /// First display row of a `visible` entry (for crash scroll-to).
    func firstDisplayRow(ofLog i: Int) -> Int { displayMap.firstRow(ofLog: i) }

    /// The contiguous display-row range a `visible` entry occupies (for selection).
    func displayRowRange(ofLog i: Int) -> ClosedRange<Int> { displayMap.rows(ofLog: i) }

    /// The `visible` index of a line by its seq (crash navigation), or nil.
    func logIndex(forSeq seq: UInt64) -> Int? { visible.firstIndex { $0.seq == seq } }

    /// The text a `visible` entry currently renders as: the compact one-liner when a
    /// prettified body is collapsed, otherwise the full (expanded) message.
    func displayMessage(_ line: LogLine) -> String {
        if let compact = line.bodyCompact, collapsedBodies.contains(line.seq) { return compact }
        return line.message
    }

    /// Display-row count for a `visible` entry, honouring a collapsed body.
    private func effectiveLineCount(_ line: LogLine) -> Int {
        LogTextLines.count(displayMessage(line))
    }

    /// Toggles a prettified body between expanded (multi-line) and collapsed (one line).
    /// No-op for non-body lines. The entry's first row keeps its position, so a collapse
    /// just folds the rows below it upward like a disclosure; a rebuilt display map +
    /// epoch bump tells the list to reload.
    func toggleBodyCollapsed(logIndex i: Int) {
        guard i >= 0, i < visible.count, visible[i].bodyCompact != nil else { return }
        let seq = visible[i].seq
        if !collapsedBodies.insert(seq).inserted { collapsedBodies.remove(seq) }
        displayMap.rebuild(lineCounts: visible.map { effectiveLineCount($0) })
        listEpoch &+= 1
    }

    /// Maps a set of selected display rows back to the unique `visible` entries they
    /// belong to, in order — so selecting any sub-line of a multi-line log copies the
    /// whole entry exactly once.
    func logIndices(forDisplayRows rows: IndexSet) -> [Int] {
        var seen = Set<Int>()
        var out: [Int] = []
        for r in rows where r < displayMap.totalRows {
            let li = displayMap.locate(row: r).log
            if seen.insert(li).inserted { out.append(li) }
        }
        return out
    }

    private let feed: LogFeed
    let adbURL: URL

    private var ring: [LogLine] = []
    // Virtualized rendering makes display cost independent of buffer size, so we keep
    // a large scrollback and drop far less. (Memory ≈ this × ~300 B.)
    private let ringCap = 500_000
    private var compiledRegex: NSRegularExpression?
    private var recomputeToken = 0

    /// An in-process stream (daemon mode off, and tests).
    convenience init(
        id: UUID = UUID(),
        device: Device,
        makeSource: @escaping @Sendable (_ bundleID: String) -> LogSource?,
        adbURL: URL,
        filter: LogFilter = LogFilter(),
        displayName: String? = nil,
        makeConsoleSource: (@Sendable (_ bundleID: String) -> LogSource?)? = nil,
        onPersist: (@Sendable (UUID, [LogLine]) -> Void)? = nil
    ) {
        let engine = LogStreamEngine(
            id: id, device: device, adbURL: adbURL, package: filter.packageLabel,
            makeSource: makeSource, makeConsoleSource: makeConsoleSource, onPersist: onPersist,
            prettifyEnabled: { LogBodyPrettifyStore.shared.enabled })
        self.init(id: id, device: device, feed: engine, adbURL: adbURL, filter: filter,
                  displayName: displayName, isRemote: false)
    }

    /// A tab over any feed (a daemon session in daemon mode).
    init(id: UUID, device: Device, feed: LogFeed, adbURL: URL, filter: LogFilter = LogFilter(),
         displayName: String? = nil, isRemote: Bool) {
        self.id = id
        self.device = device
        self.feed = feed
        self.adbURL = adbURL
        self.filter = filter
        self.isRemote = isRemote
        self.displayName = displayName ?? device.displayModel
        self.compiledRegex = filter.compiledRegex()
        feed.onLines = { [weak self] in self?.append($0) }
        feed.onState = { [weak self] in self?.apply($0) }
        apply(feed.state)
    }

    // MARK: - Lifecycle

    func start() { feed.start() }

    func stop() { feed.stop() }

    /// Ends the stream for good (the tab is closing).
    func close() { feed.close() }

    /// Mirrors the feed's stream state; the package PIDs become the tab filter's PID set.
    private func apply(_ state: LogStreamState) {
        isRunning = state.isRunning
        isConnecting = state.isConnecting
        if statusMessage != state.statusMessage { statusMessage = state.statusMessage }
        let pids = state.pids.map(Set.init)
        if filter.pids != pids {
            filter.pids = pids
            compiledRegex = filter.compiledRegex()
            recomputeVisible()
        }
    }

    /// Navigate to the next crash (downward / newer). With nothing selected yet it
    /// jumps to the first; it wraps to the top after the last.
    func nextCrash() { moveCrash(forward: true) }
    /// Navigate to the previous crash (upward / older). With nothing selected it jumps
    /// to the last; it wraps to the bottom before the first.
    func previousCrash() { moveCrash(forward: false) }

    private func moveCrash(forward: Bool) {
        guard let target = Self.nextCrashIndex(cursor: crashCursor, count: crashSeqs.count, forward: forward)
        else { return }
        crashCursor = target
        followTail = false
        scrollTarget = crashSeqs[target]
    }

    /// Cycling crash index: nil + forward → first, nil + back → last; otherwise step
    /// and wrap. Returns nil when there are no crashes.
    nonisolated static func nextCrashIndex(cursor: Int?, count: Int, forward: Bool) -> Int? {
        guard count > 0 else { return nil }
        if let c = cursor { return forward ? (c + 1) % count : (c - 1 + count) % count }
        return forward ? 0 : count - 1
    }

    func toggle() { isRunning ? stop() : connect() }

    /// Verifies the device is reachable (and, for Android, that a filtered package
    /// is installed) before starting the stream, surfacing a clear message if not.
    /// Used by restored/stopped tabs to (re)connect with feedback.
    func connect() { feed.connect() }

    /// Clears the in-app scrollback (does not touch the device buffer).
    func clear() {
        recomputeToken &+= 1   // invalidate any in-flight background recompute
        ring.removeAll(keepingCapacity: true)
        visible.removeAll(keepingCapacity: true)
        displayMap.removeAll()
        totalCount = 0
        droppedCount = 0
        droppedDisplayRows = 0
        crashSeqs.removeAll()
        crashCursor = nil
        collapsedBodies.removeAll()
        feed.resetBodyPairing()   // forget any half-seen BODY START pair
        listEpoch &+= 1
    }

    /// Clears the device-side logcat buffer too (`adb logcat -c`).
    func clearDeviceBuffer() {
        clear()
        feed.clearDeviceBuffer()
    }

    // MARK: - Filtering

    func setMinLevel(_ level: LogLevel) { mutateFilter { $0.minLevel = level } }
    func setRegex(_ on: Bool) { mutateFilter { $0.isRegex = on } }
    func setQuery(_ text: String) { mutateFilter { $0.query = text } }
    func setHideSystemLogs(_ on: Bool) { mutateFilter { $0.hideSystemLogs = on } }

    /// Applies the global message-exclusion rules and re-filters (no persist callback —
    /// the rules live in `LogExclusionStore`, not the per-tab descriptor).
    func applyExclusions(_ rules: [LogExcludeRule]) {
        filter.exclusions = rules
        compiledRegex = filter.compiledRegex()
        recomputeVisible()
    }

    /// Sets the package filter: the feed targets the app (PID polling, stdout capture,
    /// iOS re-scoping) and reports its PIDs back through `apply`.
    func setPackage(_ package: String) {
        mutateFilter {
            $0.packageLabel = package
            $0.processNameQuery = ""
        }
        feed.setPackage(package)
    }

    private func mutateFilter(_ change: (inout LogFilter) -> Void) {
        change(&filter)
        compiledRegex = filter.compiledRegex()
        recomputeVisible()
        onStateChanged?()   // persist filter/package changes right away
    }

    /// Re-filters the whole ring off the main thread (it can be 100k lines), then
    /// assigns on main — so changing the level/query/package never freezes the UI.
    /// A token discards stale results; a catch-up pass re-adds lines that streamed
    /// in while the background filter ran.
    private func recomputeVisible() {
        recomputeToken &+= 1
        let token = recomputeToken
        let snapshot = ring
        let f = filter
        let lastSeq = snapshot.last?.seq
        Task.detached(priority: .userInitiated) {
            let regex = f.compiledRegex()
            let result = snapshot.filter { f.matches($0, regex: regex) }
            await MainActor.run { [weak self] in
                guard let self, token == self.recomputeToken else { return }
                var out = result
                if let lastSeq {
                    for line in self.ring where line.seq > lastSeq
                        && self.filter.matches(line, regex: self.compiledRegex) {
                        out.append(line)
                    }
                } else {
                    out = self.ring.filter { self.filter.matches($0, regex: self.compiledRegex) }
                }
                self.visible = out
                self.displayMap.rebuild(lineCounts: out.map { self.effectiveLineCount($0) })
                self.listEpoch &+= 1
            }
        }
    }

    // MARK: - Internals

    /// Appends a batch from the feed: ring (bounded), then the lines the tab's filter keeps.
    private func append(_ batch: [LogLine]) {
        guard !batch.isEmpty else { return }
        ring.append(contentsOf: batch)
        totalCount += batch.count

        if ring.count > ringCap {
            let overflow = ring.count - ringCap
            ring.removeFirst(overflow)
            droppedCount += overflow
            let minSeq = ring.first?.seq ?? 0
            var drop = 0
            while drop < visible.count && visible[drop].seq < minSeq { drop += 1 }
            if drop > 0 {
                visible.removeFirst(drop)
                droppedDisplayRows += displayMap.removeFirst(drop)
            }
        }
        for line in batch where filter.matches(line, regex: compiledRegex) {
            visible.append(line)
            displayMap.append(lineCount: effectiveLineCount(line))
            // Counted from the engine's crash markers, which are always visible, so the badge,
            // the navigation and the 💥 lines agree (the marker sits right after the crash).
            if line.isMarker, line.markerCritical { crashSeqs.append(line.seq) }
        }
    }


    /// Installed apps/packages on this device, for the filter dropdown.
    func installedApps() async -> [AppEntry] {
        await InstalledApps.list(for: device, adbURL: adbURL)
    }

    /// Last good app list (synchronous), so the picker shows something instantly while
    /// `installedApps()` refreshes — never a blank "No apps found" when we have a cache.
    func cachedApps() -> [AppEntry] {
        InstalledApps.cached(for: device)
    }

    // MARK: - Export

    func exportText() -> String {
        visible.map { $0.raw }.joined(separator: "\n")
    }
}
