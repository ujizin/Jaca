import Foundation

/// Stream-side state of a log session: everything that isn't a view choice. Published by
/// `jacad` on `logs.state.<id>`.
struct LogStreamState: Codable, Equatable, Sendable {
    var isRunning = false
    var isConnecting = false
    var statusMessage: String?
    /// The targeted app (package / bundle id / iOS process name); empty = whole device.
    var package = ""
    /// Every PID the targeted package has had this session (accumulated, never cleared, so a
    /// crashing/relaunching app keeps its logs). nil = no PID filter.
    var pids: [Int32]?
}

extension LogStreamState {
    private enum CodingKeys: String, CodingKey { case isRunning, isConnecting, statusMessage, package, pids }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            isRunning: try c.decodeIfPresent(Bool.self, forKey: .isRunning) ?? false,
            isConnecting: try c.decodeIfPresent(Bool.self, forKey: .isConnecting) ?? false,
            statusMessage: try c.decodeIfPresent(String.self, forKey: .statusMessage),
            package: try c.decodeIfPresent(String.self, forKey: .package) ?? "",
            pids: try c.decodeIfPresent([Int32].self, forKey: .pids)
        )
    }
}

/// Where a `LogSession` gets its lines and stream state: an in-process `LogStreamEngine`, or a
/// session running in `jacad`.
@MainActor
protocol LogFeed: AnyObject {
    var state: LogStreamState { get }
    /// Processed lines (seq-stamped, prettified, markers included), in order, in batches.
    var onLines: (([LogLine]) -> Void)? { get set }
    var onState: ((LogStreamState) -> Void)? { get set }

    func start()
    func stop()
    /// Checks the device (and package) before starting, reporting a status message if not.
    func connect()
    func setPackage(_ package: String)
    /// The tab was renamed (history records the name when a run starts).
    func rename(_ name: String)
    /// Forgets a half-seen response-body pair (the view cleared its scrollback).
    func resetBodyPairing()
    func clearDeviceBuffer()
    /// Ends the session for good (the tab closed).
    func close()
}

/// Thread-safe hand-off buffer between the background stream consumer and the
/// main-actor flush loop, so we never mutate state per line.
final class LineBuffer: @unchecked Sendable {
    private let lock = NSLock()
    private var lines: [LogLine] = []

    func append(_ line: LogLine) {
        lock.lock(); lines.append(line); lock.unlock()
    }

    /// Drains up to `max` lines (oldest first), leaving any remainder for the next
    /// tick so a big burst is spread across flushes instead of one main-thread hit.
    func drain(max: Int) -> [LogLine] {
        lock.lock(); defer { lock.unlock() }
        if lines.count <= max {
            let out = lines
            lines.removeAll(keepingCapacity: true)
            return out
        }
        let out = Array(lines.prefix(max))
        lines.removeFirst(max)
        return out
    }
}

/// Monotonic, thread-safe id source. The session stamps every line (and marker)
/// with it so ids stay unique + ordered across stream reconnects (each `LogSource`
/// restart would otherwise reset its own seq to 0).
final class SeqCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var value: UInt64 = 0
    private let stride: UInt64
    /// `stride` leaves room between consecutive lines so a line the prettifier splits into
    /// several entries (metadata head / JSON body / trailing) can get distinct, in-order
    /// sub-seqs (`base, base+1, …`) without colliding with the next line — keeping `seq`
    /// strictly increasing for the front-trim and persistence ordering. Far more than the
    /// ≤ 3 parts a split ever produces.
    init(start: UInt64 = 0, stride: UInt64 = 8) { self.value = start; self.stride = stride }
    func next() -> UInt64 { lock.lock(); defer { lock.unlock() }; let v = value; value &+= stride; return v }

    /// The first line seq past `seq` for a counter with the default stride: where a new stream
    /// continues after lines (and their sub-seqs) already shown.
    static func slot(after seq: UInt64?, stride: UInt64 = 8) -> UInt64 {
        // Wrapping arithmetic like `next()`: the seq can come off the wire.
        seq.map { ($0 / stride &+ 1) &* stride } ?? 0
    }
}

/// One device log stream with everything that isn't a view: the source and its automatic
/// reconnect, seq stamping, Jaca markers (reconnects, app death/restart, crashes), package PID
/// tracking, simulator stdout capture, JSON body prettifying and history persistence. Lines
/// accumulate off-main and are handed out in batches on a ~30ms timer.
///
/// The app runs one per log tab when the daemon is off; `jacad` runs them for daemon-mode tabs
/// and any other client.
@MainActor
final class LogStreamEngine: LogFeed {
    let id: UUID
    let device: Device

    private(set) var state = LogStreamState() {
        didSet { if state != oldValue { onState?(state) } }
    }
    var onLines: (([LogLine]) -> Void)?
    var onState: ((LogStreamState) -> Void)?
    /// Called each time the stream starts (history records a session run).
    var onStarted: (() -> Void)?
    /// Called once by `close()` (history ends the run).
    var onClosed: (() -> Void)?
    private var closed = false
    /// Bumped by every connect and stop; a device check finishing under an older one is stale.
    private var connectGeneration = 0

    private let adbURL: URL?
    private let makeSource: @Sendable (_ bundleID: String) -> LogSource?
    private let makeConsoleSource: (@Sendable (_ bundleID: String) -> LogSource?)?
    private let onPersist: (@Sendable (UUID, [LogLine]) -> Void)?
    private let prettifyEnabled: @MainActor () -> Bool

    private var source: LogSource?
    /// Set when the primary source is being swapped on purpose (iOS app-target
    /// change), so the reconnect loop skips the "stream lost" warning.
    private var swappingSource = false
    private var consoleSource: LogSource?
    private let seq: SeqCounter
    private let pending = LineBuffer()
    private var bodyPrettifier = LogBodyPrettifier()

    // Package liveness, for death/restart markers.
    private var appWasAlive = false
    private var sawAppAlive = false
    private var accumulatedPids: Set<Int32> = []

    private let maxPerFlush = 4_000           // bound main-thread work per tick
    private var consumeTask: Task<Void, Never>?
    private var flushTask: Task<Void, Never>?
    private var pidTask: Task<Void, Never>?
    private var consoleTask: Task<Void, Never>?

    init(id: UUID = UUID(),
         device: Device,
         adbURL: URL?,
         package: String = "",
         seqStart: UInt64 = 0,
         makeSource: (@Sendable (String) -> LogSource?)? = nil,
         makeConsoleSource: (@Sendable (String) -> LogSource?)? = nil,
         onPersist: (@Sendable (UUID, [LogLine]) -> Void)? = nil,
         prettifyEnabled: @escaping @MainActor () -> Bool = { LogBodyPrettifyStore.isEnabled() }) {
        self.id = id
        self.device = device
        self.adbURL = adbURL
        self.makeSource = makeSource ?? LogSources.primary(for: device, adbURL: adbURL)
        // Each default stands alone: an explicit console factory is used even with the default primary.
        self.makeConsoleSource = makeConsoleSource ?? (makeSource == nil ? LogSources.console(for: device) : nil)
        self.onPersist = onPersist
        self.prettifyEnabled = prettifyEnabled
        self.seq = SeqCounter(start: seqStart)
        if !package.isEmpty { applyPackage(package) }
    }

    // MARK: - Lifecycle

    func start() {
        guard !state.isRunning else { return }
        state.isRunning = true
        state.statusMessage = nil
        onStarted?()
        startFlushLoop()
        restartPIDPollingIfNeeded()
        restartConsoleCaptureIfNeeded()
        consumeTask = Task.detached(priority: .utility) { [weak self] in
            await self?.consumeLoop()
        }
    }

    /// Connects the source and streams; if the stream ends while we're still running
    /// (device unplugged, adb restarted, …) it injects a reconnect marker and retries
    /// forever — automatic reconnection. Every line is re-stamped with our monotonic
    /// seq so ids stay unique across reconnects.
    private func consumeLoop() async {
        let buffer = pending, counter = seq
        var disconnected = false
        while await state.isRunning, !Task.isCancelled {
            guard let stream = await openStream() else {   // couldn't spawn the tool
                if !disconnected { injectMarker("✕ can’t reach \(device.displayModel) — retrying…"); disconnected = true }
                try? await Task.sleep(for: .seconds(2)); continue
            }
            if disconnected { injectMarker("✓ \(device.displayModel) reconnected"); disconnected = false }
            for await line in stream {
                var l = line; l.seq = counter.next(); buffer.append(l)
            }
            // stream ended
            guard await state.isRunning, !Task.isCancelled else { break }
            // A deliberate source swap (iOS app-target change) just stopped the old
            // source — reconnect immediately with the new target, no "lost" warning.
            if await takeSwappingSource() { continue }
            injectMarker("✕ log stream to \(device.displayModel) lost — reconnecting…")
            disconnected = true
            try? await Task.sleep(for: .seconds(1))
        }
    }

    private func openStream() -> AsyncStream<LogLine>? {
        let s = makeSource(state.package)
        source = s
        return try? s?.start()
    }

    private func takeSwappingSource() -> Bool {
        let v = swappingSource; swappingSource = false; return v
    }

    /// Injects a synthetic, always-visible marker line (thread-safe; callable off-main).
    nonisolated func injectMarker(_ message: String, critical: Bool = false) {
        var m = LogLine.marker(message, critical: critical)
        m.seq = seq.next()
        pending.append(m)
    }

    func stop() {
        // A stop during the device check cancels the start that check would make.
        connectGeneration &+= 1
        if state.isConnecting { state.isConnecting = false }
        guard state.isRunning else { return }
        state.isRunning = false
        source?.stop(); source = nil
        consoleSource?.stop(); consoleSource = nil
        consumeTask?.cancel(); consumeTask = nil
        flushTask?.cancel(); flushTask = nil
        pidTask?.cancel(); pidTask = nil
        consoleTask?.cancel(); consoleTask = nil
        flush(max: .max)  // drain everything that's left
        // Emit any response body still being reassembled across chunks (the stream ended
        // mid-body) so held fragments aren't lost.
        let leftover = bodyPrettifier.finalize()
        if !leftover.isEmpty {
            for var l in leftover { l.seq = seq.next(); pending.append(l) }
            flush(max: .max)
        }
    }

    func close() {
        closed = true
        stop()
        onLines = nil
        onState = nil
        let closedHandler = onClosed
        onClosed = nil
        closedHandler?()
    }

    /// Carries over the target app's PIDs seen by another engine (the daemon's, when this one
    /// takes over in-process), so lines from an earlier run of the app stay visible.
    func seedPIDs(_ pids: [Int32]) {
        guard state.pids != nil, !pids.isEmpty else { return }
        accumulatedPids.formUnion(pids)
        state.pids = accumulatedPids.sorted()
    }

    /// Verifies the device is reachable (and, for Android, that a filtered package
    /// is installed) before starting the stream, surfacing a clear message if not.
    func connect() {
        guard !state.isRunning, !state.isConnecting else { return }
        state.isConnecting = true
        state.statusMessage = nil
        connectGeneration &+= 1
        let generation = connectGeneration
        Task { @MainActor in
            let available = await checkDeviceAvailable()
            // Stopped or closed while checking: nobody wants this stream any more.
            guard !closed, generation == connectGeneration else { return }
            guard available else {
                state.isConnecting = false
                state.statusMessage = deviceUnavailableMessage
                return
            }
            // Soft check: warn (but still connect) if a filtered package is missing.
            let package = state.package
            if device.platform == .android, !package.isEmpty, await !isPackageInstalled(package) {
                state.statusMessage = "App “\(package)” isn’t installed on \(device.displayModel)."
            }
            guard !closed, generation == connectGeneration else { return }
            state.isConnecting = false
            // `start` clears the status line; the soft warning has to be set after it, or it
            // never shows.
            let warning = state.statusMessage
            start()
            if let warning { state.statusMessage = warning }
        }
    }

    func rename(_ name: String) {}   // in-process, history reads the tab's name when a run starts

    private var deviceUnavailableMessage: String {
        switch device.platform {
        case .android:
            return "\(device.displayModel) isn’t connected — plug it in and authorize USB debugging."
        case .iosSimulator:
            return "\(device.displayModel) isn’t booted — start the simulator and try again."
        case .iosDevice:
            return "\(device.displayModel) isn’t connected — plug it in and trust this Mac."
        }
    }

    private func checkDeviceAvailable() async -> Bool {
        switch device.platform {
        case .android:
            guard let adbURL else { return false }
            let r = try? await CommandRunner.run(adbURL, ["-s", device.id, "get-state"])
            return r?.exitCode == 0 && (r?.stdout.contains("device") ?? false)
        case .iosSimulator:
            let r = try? await CommandRunner.run(AppleToolchain.xcrun, ["simctl", "list", "devices", "booted"])
            return r?.stdout.contains(device.id) ?? false
        case .iosDevice:
            let r = try? await CommandRunner.run(AppleToolchain.xcrun, ["devicectl", "list", "devices"], timeout: 12)
            return r?.stdout.contains(device.id) ?? false
        }
    }

    private func isPackageInstalled(_ package: String) async -> Bool {
        guard let adbURL else { return false }
        let r = try? await CommandRunner.run(adbURL, ["-s", device.id, "shell", "pm", "list", "packages", package])
        return r?.stdout.contains("package:\(package)") ?? false
    }

    /// Clears the device-side logcat buffer (`adb logcat -c`).
    func clearDeviceBuffer() {
        guard device.platform == .android, let url = adbURL else { return }
        let serial = device.id
        Task.detached { await AndroidLogSource.clearBuffer(adbURL: url, serial: serial) }
    }

    func resetBodyPairing() { bodyPrettifier = LogBodyPrettifier() }

    // MARK: - Package target

    /// Targets an app: stores it and (re)starts PID polling so the filter survives the app
    /// being killed/relaunched (PIDs change).
    func setPackage(_ package: String) {
        applyPackage(package)
        restartPIDPollingIfNeeded()
        restartConsoleCaptureIfNeeded()
        restartPrimaryForTargetChange()
    }

    private func applyPackage(_ package: String) {
        accumulatedPids.removeAll()   // new target → forget the previous app's PIDs
        appWasAlive = false; sawAppAlive = false
        state.package = package
        switch device.platform {
        case .android, .iosSimulator:
            // Both filter by PID — the bundle id never appears as the process name on iOS,
            // but the unified log carries the process id (resolved by polling).
            state.pids = package.isEmpty ? nil : []
        case .iosDevice:
            // The structured (LoggingSupport) source narrows to the targeted app's process
            // itself, so no per-line PID filter is needed.
            state.pids = nil
        }
    }

    /// Physical iOS re-scopes its *primary* structured source when the targeted app
    /// changes: an empty target streams the whole device, a name scopes to that app's
    /// process. Stopping the current source lets the reconnect loop respawn with the new
    /// scope; the source emits its own "▶︎ structured device logs (…)" marker.
    private func restartPrimaryForTargetChange() {
        guard state.isRunning, device.platform == .iosDevice else { return }
        swappingSource = true
        source?.stop()
    }

    // MARK: - Flushing

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
        // Auto-prettify detected JSON response bodies before they're stored, so every view,
        // re-filtering and history all see the expanded form. Gated per flush, so turning
        // the toggle off only affects subsequent lines. An inline body splits into several
        // entries; each part gets a distinct sub-seq within the slot the SeqCounter reserved
        // for the original, keeping seq order intact.
        var batch: [LogLine]
        if prettifyEnabled() {
            batch = []
            batch.reserveCapacity(drained.count)
            for original in drained {
                let parts = bodyPrettifier.transform(original)
                for (k, p) in parts.enumerated() {
                    var pp = p; pp.seq = original.seq &+ UInt64(k); batch.append(pp)
                }
            }
        } else {
            batch = drained
        }
        onPersist?(id, batch)

        // Crash markers for crashes in the targeted app (or anywhere, with no target). The
        // marker lands in the next flush, right behind the crash.
        let pids = state.pids.map(Set.init)
        for line in batch where !line.isMarker && CrashDetector.isCrash(line) {
            if let pids, !line.isConsoleOutput, !pids.contains(line.pid) { continue }
            injectMarker("💥 \(CrashDetector.label(line))", critical: true)
        }
        onLines?(batch)
    }

    // MARK: - PID polling

    /// Polls `pidof <package>` while a package target is active, updating the PID set live so
    /// app restarts keep being captured.
    private func restartPIDPollingIfNeeded() {
        pidTask?.cancel(); pidTask = nil
        let package = state.package
        guard state.isRunning, !package.isEmpty else { return }
        let serial = device.id
        let resolve: @Sendable () async -> Set<Int32>
        switch device.platform {
        case .android:
            guard let url = adbURL else { return }
            resolve = { await AndroidLogSource.resolvePIDs(adbURL: url, serial: serial, package: package) }
        case .iosSimulator:
            resolve = { await SimulatorLogSource.resolvePIDs(udid: serial, bundleID: package) }
        case .iosDevice:
            return   // physical iOS scopes the source itself, no pid polling
        }
        pidTask = Task { [weak self] in
            while !Task.isCancelled {
                let resolved = await resolve()
                guard let self, !Task.isCancelled else { return }

                // Mark death / restart so it's unmissable in the log.
                let isAlive = !resolved.isEmpty
                if isAlive {
                    if !self.appWasAlive && self.sawAppAlive {
                        let pids = resolved.sorted().map(String.init).joined(separator: ", ")
                        self.injectMarker("▶︎ \(package) restarted — pid \(pids)")
                    }
                    self.sawAppAlive = true
                } else if self.appWasAlive {
                    self.injectMarker("■ \(package) terminated")
                }
                self.appWasAlive = isAlive

                // Accumulate; never clear. If the app is dead (resolved empty) we keep
                // the known PIDs so its logs stay visible. New PIDs (relaunch) are added.
                let next = Self.accumulatePIDs(self.accumulatedPids, with: resolved)
                if next != self.accumulatedPids {
                    self.accumulatedPids = next
                    self.state.pids = next.sorted()
                }
                try? await Task.sleep(for: .milliseconds(1500))
            }
        }
    }

    /// Accumulates an app's PIDs across restarts. An empty `resolved` (the app died /
    /// is being reinstalled) keeps the current set, so its logs are never hidden; new
    /// PIDs from a relaunch/reinstall are added.
    nonisolated static func accumulatePIDs(_ current: Set<Int32>, with resolved: Set<Int32>) -> Set<Int32> {
        resolved.isEmpty ? current : current.union(resolved)
    }

    // MARK: - Console capture

    /// Simulator stdout/print capture: when a bundle is targeted, launch it under a
    /// PTY (`simctl launch --console-pty`) and fold its stdout/stderr — the only place
    /// `print()`/`println` output appears — into this session alongside the OSLog
    /// stream. Re-targets when the package changes; (re)launches the app each time, by
    /// design. No-op on platforms without a stdout tap (`makeConsoleSource == nil`).
    private func restartConsoleCaptureIfNeeded() {
        consoleTask?.cancel(); consoleTask = nil
        consoleSource?.stop(); consoleSource = nil
        guard state.isRunning, let make = makeConsoleSource else { return }
        let bundle = state.package
        guard !bundle.isEmpty, let src = make(bundle) else { return }
        guard let stream = try? src.start() else {
            injectMarker("✕ couldn’t launch \(bundle) for stdout/print capture")
            return
        }
        consoleSource = src
        injectMarker("▶︎ capturing stdout/print from \(bundle) (app relaunched)")
        let buffer = pending, counter = seq
        consoleTask = Task.detached(priority: .utility) {
            for await line in stream {
                var l = line; l.seq = counter.next(); buffer.append(l)
            }
        }
    }
}
