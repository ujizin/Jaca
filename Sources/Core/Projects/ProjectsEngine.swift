import Foundation

/// Everything the Projects area knows, as one value: what a client renders, and what the
/// daemon publishes on `projects.state`.
struct ProjectsState: Codable, Equatable, Sendable {
    var projects: [Project] = []
    var isRefreshing = false
    var isComputingSizes = false
    var hasCompletedScan = false
    var lastRefresh: Date?
    /// Increments each time a structural scan completes. A client that approved size scans
    /// asks for sizes again when it sees a new generation.
    var scanGeneration = 0
}

extension ProjectsState {
    private enum CodingKeys: String, CodingKey {
        case projects, isRefreshing, isComputingSizes, hasCompletedScan, lastRefresh, scanGeneration
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            projects: CloudPersistence.decodeArrayField(Project.self, in: c, forKey: .projects),
            isRefreshing: try c.decodeIfPresent(Bool.self, forKey: .isRefreshing) ?? false,
            isComputingSizes: try c.decodeIfPresent(Bool.self, forKey: .isComputingSizes) ?? false,
            hasCompletedScan: try c.decodeIfPresent(Bool.self, forKey: .hasCompletedScan) ?? false,
            lastRefresh: try c.decodeIfPresent(Date.self, forKey: .lastRefresh),
            scanGeneration: try c.decodeIfPresent(Int.self, forKey: .scanGeneration) ?? 0
        )
    }
}

/// Result of clearing a checkout's build caches.
struct ProjectsClearCacheOutcome: Codable, Equatable, Sendable {
    var name: String
    var freedMB: Int
    var error: String?
}

/// Result of removing a linked worktree.
struct ProjectsDeleteWorktreeOutcome: Codable, Equatable, Sendable {
    var name: String
    var ok: Bool
    var stderr: String
}

/// The Projects area's domain logic: scanning Claude projects and user folders, computing
/// disk usage in batches, watching for worktree changes, cleaning caches and removing
/// worktrees, with the last result cached on disk. No UI state.
///
/// The app runs one in-process when the daemon is off; `jacad` runs one and publishes its
/// state. Either way `onChange` reports every state change.
@MainActor
final class ProjectsEngine {
    private(set) var state = ProjectsState() {
        didSet { if state != oldValue { onChange?(state) } }
    }
    var onChange: ((ProjectsState) -> Void)?

    private let defaults: UserDefaults
    private let scanner: ProjectsScanner
    private let cache: ProjectsCache
    private let git = GitService()
    private let cleaner = CacheCleaner()
    private var watchers: [FolderWatcher] = []
    private var watching = false
    private var sizeTask: Task<Void, Never>?
    /// Checkout paths queued in the running size walk; a repeat request only adds new ones.
    private var sizePass: Set<String> = []
    private var sizesAgain = false
    /// Checkout paths being cleaned. Kept here so a rescan mid-clean can't start a second one.
    private var cleaning: Set<String> = []
    private var scanToken = 0

    /// Checkouts sized at a time. With `DirectorySizer.width` readers each, the whole
    /// area stays well under the core count instead of starting every checkout at once.
    private static let sizeBatch = 4
    static let userFoldersKey = "jaca.projectFolders"
    private static let legacyWorktreeKey = "jaca.worktreesFolder"

    init(defaults: UserDefaults = JacaDefaults.shared,
         scanner: ProjectsScanner = ProjectsScanner(),
         cache: ProjectsCache = ProjectsCache()) {
        self.defaults = defaults
        self.scanner = scanner
        self.cache = cache
        if let cached = cache.load() { state.projects = cached }
        migrateLegacyFolder()
    }

    // MARK: - Watching

    /// Watches `~/.claude/projects` (new Claude projects/worktrees) plus each git repo's
    /// `.git/worktrees` (any `git worktree add/remove`), refreshing in the background on
    /// change. Rebuilt after every scan since the project set changes.
    func startWatching() {
        watching = true
        rebuildWatchers()
    }

    func stopWatching() {
        watching = false
        watchers.forEach { $0.cancel() }
        watchers.removeAll()
    }

    private func rebuildWatchers() {
        watchers.forEach { $0.cancel() }
        watchers.removeAll()
        guard watching else { return }

        let onChange: () -> Void = { [weak self] in
            Task { @MainActor in self?.refresh() }
        }
        let claudeProjects = scanner.claudeHome.appendingPathComponent("projects")
        if let w = FolderWatcher(url: claudeProjects, onChange: onChange) { watchers.append(w) }

        for project in state.projects where project.isGitRepo {
            let wtDir = project.url.appendingPathComponent(".git/worktrees")
            if FileManager.default.fileExists(atPath: wtDir.path),
               let w = FolderWatcher(url: wtDir, onChange: onChange) {
                watchers.append(w)
            }
        }
    }

    // MARK: - Scanning

    func refresh() {
        guard !state.isRefreshing else { return }
        state.isRefreshing = true
        scanToken &+= 1
        let token = scanToken
        let scanner = self.scanner
        let folders = userFolders
        Task { [weak self] in
            let scanned = await scanner.scan(userFolders: folders)
            guard let self, token == self.scanToken else { return }
            // Carry over already-computed sizes so a refresh doesn't flash "—".
            let previous = Self.checkoutIndex(self.state.projects)
            var next = self.state
            next.projects = scanned.map { project in
                var p = project
                p.checkouts = p.checkouts.map { c in
                    var c = c
                    c.cleaning = self.cleaning.contains(c.path)
                    guard let old = previous[c.path], old.sizeComputed else { return c }
                    var merged = c
                    merged.sizeMB = old.sizeMB
                    merged.cacheMB = old.cacheMB
                    merged.sizeComputed = true
                    return merged
                }
                return p
            }
            next.isRefreshing = false
            next.hasCompletedScan = true
            next.lastRefresh = Date()
            next.scanGeneration &+= 1
            self.state = next
            self.rebuildWatchers()
            self.saveCache()
        }
    }

    /// Computes disk usage for every git checkout in the background, `sizeBatch` checkouts
    /// at a time, patching rows as each batch lands, then re-saves the cache. A request while a
    /// walk runs (a rescan, a second client) adds only the checkouts that walk doesn't cover, so
    /// frequent rescans don't restart it from the first checkout.
    func computeSizes() {
        let work: [(pid: String, cid: String, url: URL)] = state.projects
            .filter(\.isGitRepo)
            .flatMap { p in p.checkouts.map { (p.id, $0.id, $0.url) } }
            .filter { !sizePass.contains($0.url.path) }
        guard !work.isEmpty else { return }
        if sizeTask != nil { sizesAgain = true; return }
        sizePass.formUnion(work.map(\.url.path))
        let git = self.git
        state.isComputingSizes = true
        sizeTask = Task { [weak self] in
            for start in stride(from: 0, to: work.count, by: Self.sizeBatch) {
                if Task.isCancelled { break }
                let batch = work[start..<min(start + Self.sizeBatch, work.count)]
                await withTaskGroup(of: (String, String, Int, Int).self) { group in
                    for item in batch {
                        group.addTask {
                            let u = await git.diskUsage(of: item.url)
                            return (item.pid, item.cid, u.sizeMB, u.cacheMB)
                        }
                    }
                    for await (pid, cid, size, cacheMB) in group {
                        guard let self else { continue }
                        self.patchCheckout(pid, cid) { $0.sizeMB = size; $0.cacheMB = cacheMB; $0.sizeComputed = true }
                    }
                }
            }
            guard let self, !Task.isCancelled else { return }
            self.sizeTask = nil
            if self.sizesAgain {
                self.sizesAgain = false
                self.computeSizes()
            }
            if self.sizeTask == nil {
                self.sizePass = []
                self.state.isComputingSizes = false
            }
            self.saveCache()
        }
    }

    func cancelSizes() {
        sizeTask?.cancel()
        sizeTask = nil
        sizePass = []
        sizesAgain = false
        state.isComputingSizes = false
    }

    // MARK: - User folders

    /// Manually added project folders.
    var userFolders: [String] { defaults.stringArray(forKey: Self.userFoldersKey) ?? [] }

    /// Moves the old single Worktrees folder into the user-folder list, once.
    private func migrateLegacyFolder() {
        guard let legacy = defaults.url(forKey: Self.legacyWorktreeKey)?.path else { return }
        var folders = userFolders
        if !folders.contains(legacy) {
            folders.append(legacy)
            defaults.set(folders, forKey: Self.userFoldersKey)
        }
        defaults.removeObject(forKey: Self.legacyWorktreeKey)
    }

    /// Adds a folder and rescans. False when it was already added.
    func addFolder(_ path: String) -> Bool {
        var folders = userFolders
        guard !folders.contains(path) else { return false }
        folders.append(path)
        defaults.set(folders, forKey: Self.userFoldersKey)
        refresh()
        return true
    }

    /// Removes a manually-added project from the list (the folder on disk is untouched).
    /// Returns its name, or nil when `id` isn't a user-added project.
    func removeUserProject(_ id: String) -> String? {
        guard let project = state.projects.first(where: { $0.id == id }), project.source == .user else { return nil }
        defaults.set(userFolders.filter { $0 != id }, forKey: Self.userFoldersKey)
        state.projects.removeAll { $0.id == id }
        saveCache()
        return project.name
    }

    // MARK: - Cache cleaning

    /// Clears build caches for a checkout (the per-worktree `.gradle/` + every `build/`
    /// + matching iOS DerivedData). Nil when the checkout is gone or already cleaning.
    func clearCache(project pid: String, checkout cid: String) async -> ProjectsClearCacheOutcome? {
        guard let c = checkout(pid, cid), !cleaning.contains(c.path) else { return nil }
        let oldSize = c.sizeMB
        let name = c.name
        cleaning.insert(c.path)
        patchCheckout(pid, cid) { $0.cleaning = true }
        let result = await cleaner.clearCache(worktree: c.url)
        cleaning.remove(c.path)
        let freed = max(0, oldSize - result.newSizeMB) + result.derivedFreedMB
        patchCheckout(pid, cid) {
            $0.cleaning = false
            $0.sizeMB = result.newSizeMB
            $0.sizeComputed = true
            $0.dropped = true
        }
        saveCache()
        // The green "freed" flash clears on its own.
        Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(1400))
            self?.patchCheckout(pid, cid) { $0.dropped = false }
        }
        return ProjectsClearCacheOutcome(name: name, freedMB: freed, error: result.error)
    }

    // MARK: - Worktree removal

    /// Removes a linked worktree (`git worktree remove --force`). The main checkout can't be
    /// removed. Nil when the checkout is gone or is the main one.
    func deleteWorktree(project pid: String, checkout cid: String) async -> ProjectsDeleteWorktreeOutcome? {
        guard let project = state.projects.first(where: { $0.id == pid }),
              let c = checkout(pid, cid), !c.isMain else { return nil }
        let result = await git.removeWorktree(at: c.url, repo: project.url)
        guard result.ok else {
            return ProjectsDeleteWorktreeOutcome(name: c.name, ok: false, stderr: result.stderr)
        }
        patchCheckout(pid, cid) { $0.removing = true }
        try? await Task.sleep(for: .milliseconds(280))
        if let pi = state.projects.firstIndex(where: { $0.id == pid }) {
            state.projects[pi].checkouts.removeAll { $0.id == cid }
        }
        saveCache()
        return ProjectsDeleteWorktreeOutcome(name: c.name, ok: true, stderr: "")
    }

    // MARK: - Helpers

    private func saveCache() { cache.save(state.projects) }

    private func checkout(_ pid: String, _ cid: String) -> ProjectCheckout? {
        state.projects.first { $0.id == pid }?.checkouts.first { $0.id == cid }
    }

    private func patchCheckout(_ pid: String, _ cid: String, _ mutate: (inout ProjectCheckout) -> Void) {
        guard let pi = state.projects.firstIndex(where: { $0.id == pid }),
              let ci = state.projects[pi].checkouts.firstIndex(where: { $0.id == cid }) else { return }
        mutate(&state.projects[pi].checkouts[ci])
    }

    private static func checkoutIndex(_ projects: [Project]) -> [String: ProjectCheckout] {
        var map: [String: ProjectCheckout] = [:]
        for p in projects { for c in p.checkouts { map[c.path] = c } }
        return map
    }
}
