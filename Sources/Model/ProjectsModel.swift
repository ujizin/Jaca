import Foundation
import Observation
import AppKit

/// A transient toast shown over the Projects area.
struct ProjectsToast: Equatable { var message: String; var systemFallback: String }

/// How the Projects list is laid out: a flat list, or a tree that nests detected
/// sub-projects under their container folder.
enum ProjectsViewMode: String, CaseIterable { case tree, list }

/// State for the unified Projects area — the merge of the old Worktrees and Claude
/// Projects areas. It auto-discovers projects Claude Code has run in (and their
/// worktrees), lets the user add arbitrary folders, and offers per-checkout disk-cache
/// cleanup + worktree removal.
///
/// The domain work (scanning, sizing, watching, cleaning, the on-disk cache) lives in
/// `ProjectsEngine`. This model holds view state and renders the engine's `ProjectsState`,
/// from an in-process engine or, in daemon mode, from `jacad`'s `projects.state` topic.
///
/// Reactivity: the last scan (with sizes) is cached on disk and loaded synchronously at
/// init, so the area renders instantly. The first scan of a cold start blocks behind a
/// loader; afterwards scans run in the background. `~/.claude/projects` and each repo's
/// `.git/worktrees` are watched so new/removed worktrees refresh the list automatically,
/// and on-appear only rescans when the data is stale.
@Observable
@MainActor
final class ProjectsModel {
    private(set) var projects: [Project] = []
    private(set) var isRefreshing = false
    private(set) var hasCompletedScan = false

    /// Disk-usage scanning reads every file under every checkout, so it runs only after
    /// the user asks for it. Held in memory on purpose — never persisted — so each app
    /// launch asks again.
    private(set) var sizeScanApproved = false
    private(set) var sizeScanDeclined = false
    private(set) var isComputingSizes = false
    var expanded: Set<String> = []
    var toast: ProjectsToast?

    /// List vs tree layout; persisted, defaults to tree.
    var viewMode: ProjectsViewMode {
        didSet { UserDefaults.standard.set(viewMode.rawValue, forKey: Self.viewModeKey) }
    }

    private(set) var lastRefresh: Date?

    /// The installed Zed editor, resolved once at init. Nil when Zed isn't installed, which
    /// hides the per-row "Open in Zed" action entirely.
    let zed = ExternalEditor.detect(.zed)

    /// The system Finder icon, for the always-visible "Open in Finder" button. Resolved
    /// by bundle id (Finder is always present), with a path fallback just in case.
    let finderIcon: NSImage = {
        let ws = NSWorkspace.shared
        let url = ws.urlForApplication(withBundleIdentifier: "com.apple.finder")
            ?? URL(fileURLWithPath: "/System/Library/CoreServices/Finder.app")
        return ws.icon(forFile: url.path)
    }()

    /// Whether the `herdr` CLI is installed — gates the "Open in Herdr" action. Seeded
    /// synchronously, then refined via the login-shell PATH (see `resolveHerdr()`), so the
    /// button appears even when `herdr` lives only on the user's shell PATH.
    var herdrInstalled = HerdrService.binaryURL() != nil
    /// The Herdr logo (bundled asset), for the "Open in Herdr" button. nil hides the action.
    let herdrIcon = NSImage(named: "HerdrIcon")
    /// Presents the Herdr config sheet via the Projects-header gear (command settings only).
    var showingHerdrConfig = false
    /// Presents the launch sheet (name this session; + command on first run) on row click.
    var showingHerdrLaunch = false
    private let herdrService = HerdrService()
    private var pendingHerdrTarget: HerdrService.LaunchTarget?

    /// Daemon mode renders `jacad`'s engine; otherwise (and whenever the daemon can't be
    /// reached) the in-process engine does the work.
    private let daemon: DaemonConnector
    private var useDaemon: Bool { daemon.isEnabled(.projects) }
    private var localEngine: ProjectsEngine?
    private var daemonWatch: Task<Void, Never>?
    /// The last scan generation seen, so sizes are recomputed once per completed scan.
    private var seenGeneration = 0
    private var toastTask: Task<Void, Never>?

    private static let autoRefreshTTL: TimeInterval = 30
    private static let viewModeKey = "jaca.projectsViewMode"
    private static let herdrCommandKey = "jaca.herdr.claudeCommand"
    /// The app-wide default Claude command Herdr runs in the new tab.
    static let defaultHerdrCommand = "claude --permission-mode bypassPermissions"

    init(daemon: DaemonConnector? = nil) {
        let daemon = daemon ?? .shared
        self.daemon = daemon
        viewMode = UserDefaults.standard.string(forKey: Self.viewModeKey)
            .flatMap(ProjectsViewMode.init(rawValue:)) ?? .tree
        if daemon.isEnabled(.projects) {
            // Render the on-disk cache at once; the daemon's retained state follows.
            projects = ProjectsCache().load() ?? []
            watchDaemon()
        } else {
            startLocalEngine()
        }
        resolveHerdr()
    }

    // MARK: - Engine plumbing

    @discardableResult
    private func startLocalEngine() -> ProjectsEngine {
        if let localEngine { return localEngine }
        let engine = ProjectsEngine()
        engine.onChange = { [weak self] in self?.apply($0) }
        engine.startWatching()
        localEngine = engine
        apply(engine.state)
        return engine
    }

    private func watchDaemon() {
        daemonWatch = daemon.watch(
            [ProjectsArea.stateTopic],
            onUnavailable: { [weak self] in self?.startLocalEngine() }
        ) { [weak self] event in
            guard let self, let state = try? event.decode(ProjectsState.self) else { return }
            // The daemon is back: its engine owns the work again.
            if let local = self.localEngine {
                local.stopWatching()
                local.onChange = nil
                self.localEngine = nil
            }
            self.apply(state)
        }
    }

    private func apply(_ state: ProjectsState) {
        projects = state.projects
        isRefreshing = state.isRefreshing
        isComputingSizes = state.isComputingSizes
        hasCompletedScan = state.hasCompletedScan
        lastRefresh = state.lastRefresh
        // A different generation means a scan completed (or the daemon restarted with its
        // own count): with sizes approved, size the new checkout set.
        let newScan = state.scanGeneration != seenGeneration
        seenGeneration = state.scanGeneration
        if newScan && sizeScanApproved { requestSizes() }
    }

    /// Runs `remote` against the daemon, or `local` against the in-process engine when
    /// daemon mode is off or the daemon can't be reached. A call that reached the daemon
    /// and failed yields nil rather than running twice.
    private func perform<R: Decodable & Sendable, P: Encodable & Sendable>(
        _ method: String, _ params: P, as type: R.Type,
        local: (ProjectsEngine) async -> R?
    ) async -> R? {
        if useDaemon, localEngine == nil {
            do {
                if let value = try await daemon.request(method, params, as: Optional<R>.self) { return value }
            } catch {
                return nil
            }
        }
        return await local(startLocalEngine())
    }

    private func requestSizes() {
        Task { [weak self] in
            _ = await self?.perform("projects.computeSizes", RPCEmpty(), as: RPCEmpty.self) {
                $0.computeSizes(); return RPCEmpty()
            }
        }
    }

    /// Re-checks `herdr` availability via the login-shell PATH (off-main), flipping
    /// `herdrInstalled` so the action appears even when the binary isn't in a well-known
    /// dir. The synchronous seed above already covers the common locations.
    private func resolveHerdr() {
        Task { [weak self] in
            let url = await HerdrService.resolveBinaryURL()
            self?.herdrInstalled = (url != nil)
        }
    }

    // MARK: - Derived

    var totalProjects: Int { projects.count }
    var totalWorktrees: Int { projects.reduce(0) { $0 + $1.worktreeCount } }
    var hasNoData: Bool { projects.isEmpty }

    /// Checkouts a size scan would walk — what the approval prompt quotes.
    var sizableCheckouts: Int {
        projects.filter(\.isGitRepo).reduce(0) { $0 + $1.checkouts.count }
    }

    /// Shows the approval prompt: sizes were neither asked for nor turned down yet.
    var needsSizeApproval: Bool {
        !sizeScanApproved && !sizeScanDeclined && sizableCheckouts > 0
    }

    /// The projects laid out per the current view mode (tree nests sub-projects).
    var nodes: [ProjectNode] {
        viewMode == .tree ? ProjectsGrouping.tree(projects) : ProjectsGrouping.flat(projects)
    }
    /// Block the UI with a loader only on a cold first scan (no cache to show).
    var isFirstLoad: Bool { projects.isEmpty && isRefreshing && !hasCompletedScan }

    var shouldAutoRefresh: Bool {
        guard let lastRefresh else { return true }
        return Date().timeIntervalSince(lastRefresh) > Self.autoRefreshTTL
    }

    // MARK: - Scanning

    func refresh() {
        guard !isRefreshing else { return }
        isRefreshing = true
        Task { [weak self] in
            let started = await self?.perform("projects.refresh", RPCEmpty(), as: RPCEmpty.self) {
                $0.refresh(); return RPCEmpty()
            }
            // The daemon refused the call: nothing is refreshing, so drop the indicator.
            if started == nil { self?.isRefreshing = false }
        }
    }

    /// Starts the disk-usage scan the user just approved. The approval lasts for this
    /// app launch only, so a later `refresh()` may re-scan without asking again.
    func approveSizeScan() {
        sizeScanDeclined = false
        guard !sizeScanApproved else { return }
        sizeScanApproved = true
        requestSizes()
    }

    /// Keeps the cached sizes on screen and asks no further this launch.
    func declineSizeScan() {
        sizeScanDeclined = true
        cancelSizeScan()
    }

    func cancelSizeScan() {
        isComputingSizes = false
        Task { [weak self] in
            _ = await self?.perform("projects.cancelSizes", RPCEmpty(), as: RPCEmpty.self) {
                $0.cancelSizes(); return RPCEmpty()
            }
        }
    }

    // MARK: - Expansion

    func toggle(_ id: String) {
        if expanded.contains(id) { expanded.remove(id) } else { expanded.insert(id) }
    }
    func isExpanded(_ id: String) -> Bool { expanded.contains(id) }

    // MARK: - User folders

    func addFolder() {
        guard let url = ProjectsOpen.pickFolder() else { return }
        let path = url.path
        Task { [weak self] in
            guard let self else { return }
            let added = await self.perform("projects.addFolder", ProjectsArea.PathParams(path: path), as: Bool.self) {
                $0.addFolder(path)
            }
            guard let added else { return }
            if added { self.flash("Added \(url.lastPathComponent)") } else { self.flash("Already added") }
        }
    }

    /// Removes a manually-added project from the list (does not touch the folder on disk).
    func removeUserProject(_ id: String) {
        guard let project = projects.first(where: { $0.id == id }), project.source == .user else { return }
        Task { [weak self] in
            guard let self else { return }
            let name = await self.perform("projects.removeFolder", ProjectsArea.IDParams(id: id), as: String.self) {
                $0.removeUserProject(id)
            }
            guard name != nil else { return }
            self.flash("Removed \(project.name)", fallback: "eraser")
        }
    }

    // MARK: - Cache cleaning

    /// Clears build caches for a checkout (the per-worktree `.gradle/` + every `build/`
    /// + matching iOS DerivedData).
    func clearCache(project pid: String, checkout cid: String) {
        guard let c = checkout(pid, cid), !c.cleaning else { return }
        patchCheckout(pid, cid) { $0.cleaning = true }
        Task { [weak self] in
            guard let self else { return }
            let params = ProjectsArea.CheckoutParams(project: pid, checkout: cid)
            let outcome = await self.perform("projects.clearCache", params, as: ProjectsClearCacheOutcome.self) {
                await $0.clearCache(project: pid, checkout: cid)
            }
            guard let outcome else {
                self.patchCheckout(pid, cid) { $0.cleaning = false }
                return
            }
            if let error = outcome.error {
                self.flash("Clean failed · \(error.prefix(50))", fallback: "sparkles")
            } else {
                self.flash("Freed \(formatSize(outcome.freedMB)) · \(outcome.name)", fallback: "sparkles")
            }
        }
    }

    // MARK: - Worktree removal

    /// Removes a linked worktree (`git worktree remove --force`). The main checkout
    /// can't be removed.
    func deleteWorktree(project pid: String, checkout cid: String) {
        guard let c = checkout(pid, cid), !c.isMain else { return }
        Task { [weak self] in
            guard let self else { return }
            let params = ProjectsArea.CheckoutParams(project: pid, checkout: cid)
            guard let outcome = await self.perform("projects.deleteWorktree", params, as: ProjectsDeleteWorktreeOutcome.self, local: {
                await $0.deleteWorktree(project: pid, checkout: cid)
            }) else { return }
            guard outcome.ok else {
                let msg = outcome.stderr.trimmingCharacters(in: .whitespacesAndNewlines)
                self.flash(msg.isEmpty ? "Couldn't remove worktree" : msg, fallback: "eraser")
                return
            }
            self.flash("Deleted \(outcome.name)", fallback: "eraser")
        }
    }

    // MARK: - Reveal & open

    /// Opens the folder itself in Finder (shows its contents), rather than selecting it in
    /// its parent.
    func openInFinder(_ url: URL) {
        guard FileManager.default.fileExists(atPath: url.path) else { return }
        NSWorkspace.shared.open(url)
    }

    /// Opens a project/checkout folder as a workspace in Zed. No-op (with a toast) when the
    /// folder has gone missing; the action is only shown when `zed` is non-nil to begin with.
    func openInZed(_ url: URL) {
        guard let zed else { return }
        guard FileManager.default.fileExists(atPath: url.path) else {
            flash("Folder no longer exists", fallback: "eraser")
            return
        }
        zed.open(url)
        flash("Opening \(url.lastPathComponent) in \(zed.name)")
    }

    // MARK: - Herdr

    /// The app-wide Claude command Herdr launches. Persisted; falls back to the default
    /// until the user configures it the first time.
    var herdrClaudeCommand: String {
        UserDefaults.standard.string(forKey: Self.herdrCommandKey) ?? Self.defaultHerdrCommand
    }
    /// True once the user has confirmed the command at least once (first-run gate).
    var isHerdrConfigured: Bool {
        UserDefaults.standard.string(forKey: Self.herdrCommandKey) != nil
    }

    /// Begins a Herdr launch for a project root or worktree: stashes the target and opens
    /// the launch sheet so the user names the session (and sets the command on first run).
    /// - `isWorktree`: a linked worktree (cd into it) vs the project root (new-worktree flow).
    func openInHerdr(projectRoot: URL, projectName: String, folder: URL, isWorktree: Bool, hasGit: Bool) {
        pendingHerdrTarget = HerdrService.LaunchTarget(
            projectRoot: projectRoot, projectName: projectName,
            folder: folder, isWorktree: isWorktree, hasGit: hasGit, tabName: ""
        )
        showingHerdrLaunch = true
    }

    /// Confirms the launch sheet: persists the command on first run, names the tab, launches.
    func confirmHerdrLaunch(tabName: String, command: String?) {
        if let command { persistHerdrCommand(command) }
        showingHerdrLaunch = false
        guard var target = pendingHerdrTarget else { return }
        pendingHerdrTarget = nil
        target.tabName = tabName.trimmingCharacters(in: .whitespacesAndNewlines)
        launchHerdr(target)
    }

    /// Dismisses the launch sheet without launching.
    func cancelHerdrLaunch() {
        pendingHerdrTarget = nil
        showingHerdrLaunch = false
    }

    /// Opens the command-only config sheet from the Projects header gear.
    func openHerdrSettings() { showingHerdrConfig = true }

    /// Saves the command app-wide (gear sheet).
    func saveHerdrConfig(_ command: String) {
        persistHerdrCommand(command)
        showingHerdrConfig = false
    }

    /// Dismisses the config sheet.
    func cancelHerdrConfig() { showingHerdrConfig = false }

    private func persistHerdrCommand(_ command: String) {
        let trimmed = command.trimmingCharacters(in: .whitespacesAndNewlines)
        UserDefaults.standard.set(trimmed.isEmpty ? Self.defaultHerdrCommand : trimmed, forKey: Self.herdrCommandKey)
    }

    private func launchHerdr(_ target: HerdrService.LaunchTarget) {
        let command = herdrClaudeCommand
        let service = herdrService
        Task { [weak self] in
            do {
                let result = try await service.launch(target, claudeCommand: command) { message in
                    await MainActor.run { self?.flash(message, fallback: "sparkles") }
                }
                self?.flash("Launched in Herdr · \(result.workspaceLabel)/\(result.tabLabel)", fallback: "sparkles")
            } catch {
                let reason = (error as? LocalizedError)?.errorDescription ?? "launch failed"
                self?.flash("Herdr: \(reason)", fallback: "eraser")
            }
        }
    }

    // MARK: - Toast

    func flash(_ message: String, fallback: String = "checkmark") {
        toastTask?.cancel()
        toast = ProjectsToast(message: message, systemFallback: fallback)
        toastTask = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(2600))
            if !Task.isCancelled { self?.toast = nil }
        }
    }

    // MARK: - Mutation helpers

    private func checkout(_ pid: String, _ cid: String) -> ProjectCheckout? {
        projects.first { $0.id == pid }?.checkouts.first { $0.id == cid }
    }

    /// An optimistic local patch; the engine's next state replaces it.
    private func patchCheckout(_ pid: String, _ cid: String, _ mutate: (inout ProjectCheckout) -> Void) {
        guard let pi = projects.firstIndex(where: { $0.id == pid }),
              let ci = projects[pi].checkouts.firstIndex(where: { $0.id == cid }) else { return }
        mutate(&projects[pi].checkouts[ci])
    }
}
