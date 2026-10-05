import Foundation

/// gcloud detection + sign-in status.
enum CloudAuthState: Equatable, Sendable {
    case unknown            // not checked yet
    case notInstalled       // no gcloud binary found
    case notAuthenticated   // gcloud present but no active account
    case authenticated(account: String)
}

extension CloudAuthState: Codable {
    private enum CodingKeys: String, CodingKey { case state, account }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        switch try c.decodeIfPresent(String.self, forKey: .state) {
        case "notInstalled": self = .notInstalled
        case "notAuthenticated": self = .notAuthenticated
        case "authenticated": self = .authenticated(account: try c.decodeIfPresent(String.self, forKey: .account) ?? "")
        default: self = .unknown
        }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .unknown: try c.encode("unknown", forKey: .state)
        case .notInstalled: try c.encode("notInstalled", forKey: .state)
        case .notAuthenticated: try c.encode("notAuthenticated", forKey: .state)
        case .authenticated(let account):
            try c.encode("authenticated", forKey: .state)
            try c.encode(account, forKey: .account)
        }
    }
}

/// The outcome of adding a GCP project.
enum CloudAddProjectResult: Equatable, Sendable {
    case added, alreadyExists, failure(String)
}

extension CloudAddProjectResult: Codable {
    private enum CodingKeys: String, CodingKey { case result, message }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        switch try c.decodeIfPresent(String.self, forKey: .result) {
        case "added": self = .added
        case "alreadyExists": self = .alreadyExists
        default: self = .failure(try c.decodeIfPresent(String.self, forKey: .message) ?? "")
        }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .added: try c.encode("added", forKey: .result)
        case .alreadyExists: try c.encode("alreadyExists", forKey: .result)
        case .failure(let m):
            try c.encode("failure", forKey: .result)
            try c.encode(m, forKey: .message)
        }
    }
}

/// Everything the Cloud Logging area shares across tabs: what the app renders and what the
/// daemon publishes on `cloud.state`.
struct CloudState: Codable, Equatable, Sendable {
    var binaryPath: String?
    var isDetecting = false
    var authState: CloudAuthState = .unknown
    var projects: [CloudProject] = []
    var queryTemplates: [CloudQueryTemplate] = []
    var sqlTemplates: [CloudSqlTemplate] = []
}

extension CloudState {
    private enum CodingKeys: String, CodingKey { case binaryPath, isDetecting, authState, projects, queryTemplates, sqlTemplates }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            binaryPath: try c.decodeIfPresent(String.self, forKey: .binaryPath),
            isDetecting: try c.decodeIfPresent(Bool.self, forKey: .isDetecting) ?? false,
            authState: (try? c.decodeIfPresent(CloudAuthState.self, forKey: .authState)) ?? .unknown,
            projects: CloudPersistence.decodeArrayField(CloudProject.self, in: c, forKey: .projects),
            queryTemplates: CloudPersistence.decodeArrayField(CloudQueryTemplate.self, in: c, forKey: .queryTemplates),
            sqlTemplates: CloudPersistence.decodeArrayField(CloudSqlTemplate.self, in: c, forKey: .sqlTemplates)
        )
    }
}

/// The Cloud Logging area's shared domain state and work: gcloud detection + auth, the
/// persisted project list (with each project's global log name, cached log names and detected
/// label keys) and the saved templates. No UI state; the app's `CloudLoggingRegistry` adds the
/// toasts. The app runs one in-process when the daemon is off; `jacad` runs one otherwise.
@MainActor
final class CloudEngine {
    private(set) var state = CloudState() {
        didSet { if state != oldValue { onChange?(state) } }
    }
    var onChange: ((CloudState) -> Void)?

    private let store: CloudProjectStore
    private let templateStore: CloudTemplateStore

    var cli: GcloudCLI? { state.binaryPath.map { GcloudCLI(binary: URL(fileURLWithPath: $0)) } }

    init(store: CloudProjectStore = CloudProjectStore(), templateStore: CloudTemplateStore = CloudTemplateStore(),
         detectOnInit: Bool = true) {
        self.store = store
        self.templateStore = templateStore
        state.projects = store.load()   // synchronous load → first frame already has the project list
        (state.queryTemplates, state.sqlTemplates) = templateStore.load()
        stamps = currentStamps
        if detectOnInit { detect() }
    }

    /// Re-reads projects and templates from disk: the app edited them in-process while the
    /// daemon was unreachable.
    func reload() {
        stamps = currentStamps
        state.projects = store.load()
        (state.queryTemplates, state.sqlTemplates) = templateStore.load()
    }

    /// Modification dates of the two files when this engine last read or wrote them.
    private var stamps: [Date?] = []

    private var currentStamps: [Date?] {
        [store.fileURL, templateStore.fileURL].map {
            try? FileManager.default.attributesOfItem(atPath: $0.path)[.modificationDate] as? Date
        }
    }

    /// Re-reads when another process (the app or `jacad`) wrote the files since, so a save here
    /// never overwrites newer edits with a stale list.
    private func syncFromDisk() {
        guard currentStamps != stamps else { return }
        reload()
    }

    func project(_ id: String) -> CloudProject? { state.projects.first { $0.projectID == id } }

    // MARK: - Detection & auth

    /// (Re)detects gcloud, then refreshes the auth state. Safe to call repeatedly.
    func detect() {
        state.isDetecting = true
        Task { @MainActor in
            let url = await GcloudToolchain.resolveBinaryURL()
            self.state.binaryPath = url?.path
            self.state.isDetecting = false
            if url == nil { self.state.authState = .notInstalled; return }
            await self.refreshAuth()
        }
    }

    func refreshAuth() async {
        guard let cli else { state.authState = .notInstalled; return }
        if let account = await cli.activeAccount() {
            state.authState = .authenticated(account: account)
        } else {
            state.authState = .notAuthenticated
        }
    }

    /// Flips to "not signed in" when a session's gcloud call reports an auth failure.
    func markUnauthenticated() { state.authState = .notAuthenticated }

    // MARK: - Templates

    /// False when the name is empty (nothing saved).
    func saveQueryTemplate(name: String, query: CloudLogQuery, rawFilter: String?) -> Bool {
        syncFromDisk()
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return false }
        state.queryTemplates.append(CloudQueryTemplate(name: trimmed, query: query, rawFilter: rawFilter))
        saveTemplates()
        return true
    }

    func deleteQueryTemplate(_ id: UUID) {
        syncFromDisk()
        state.queryTemplates.removeAll { $0.id == id }
        saveTemplates()
    }

    func saveSqlTemplate(name: String, sql: String) -> Bool {
        syncFromDisk()
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return false }
        state.sqlTemplates.append(CloudSqlTemplate(name: trimmed, sql: sql))
        saveTemplates()
        return true
    }

    func deleteSqlTemplate(_ id: UUID) {
        syncFromDisk()
        state.sqlTemplates.removeAll { $0.id == id }
        saveTemplates()
    }

    private func saveTemplates() {
        templateStore.save(queries: state.queryTemplates, sql: state.sqlTemplates)
        stamps = currentStamps
    }

    // MARK: - Projects

    /// Validates the project id via `gcloud projects describe`, then stores it.
    func addProject(id: String, displayName: String) async -> CloudAddProjectResult {
        let trimmed = id.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return .failure("Enter a project id.") }
        if state.projects.contains(where: { $0.projectID == trimmed }) { return .alreadyExists }
        guard let cli else { return .failure("gcloud isn't installed.") }
        do {
            try await cli.describeProject(trimmed)
        } catch let error as GcloudCLI.CLIError {
            if case .notAuthenticated = error { state.authState = .notAuthenticated }
            return .failure(error.errorDescription ?? "Couldn't validate the project.")
        } catch {
            return .failure(error.localizedDescription)
        }
        syncFromDisk()
        if state.projects.contains(where: { $0.projectID == trimmed }) { return .alreadyExists }
        var project = CloudProject(projectID: trimmed)
        project.displayName = displayName.trimmingCharacters(in: .whitespacesAndNewlines)
        state.projects.append(project)
        persist()
        return .added
    }

    func setDisplayName(_ name: String, for id: String) {
        update(id) { $0.displayName = name.trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    /// Returns the removed project's title, or nil when it wasn't there.
    func removeProject(_ id: String) -> String? {
        syncFromDisk()
        guard let title = project(id)?.title else { return nil }
        state.projects.removeAll { $0.projectID == id }
        persist()
        return title
    }

    /// Sets the GLOBAL selected log name for a project, shared by every open session.
    func setSelectedLogName(_ logName: String?, for id: String) {
        update(id) { $0.selectedLogName = logName }
    }

    func setLogNames(_ names: [String], for id: String) {
        update(id) { $0.logNames = names }
    }

    /// Refreshes the available log names from gcloud and caches them. Returns an error message
    /// when listing failed, nil on success.
    func refreshLogNames(for id: String) async -> String? {
        guard let cli else { return nil }
        do {
            let names = try await cli.listLogNames(project: id)
            update(id) { $0.logNames = names }
            return nil
        } catch let error as GcloudCLI.CLIError {
            if case .notAuthenticated = error { state.authState = .notAuthenticated }
            return error.errorDescription ?? "Couldn't list logs."
        } catch {
            return "Couldn't list logs."
        }
    }

    /// Merges auto-detected label keys for a (project, log name). Only persists on a real
    /// change, so the hot streaming path doesn't thrash the disk.
    func recordLabelKeys(_ keys: Set<String>, project id: String, logName: String) {
        syncFromDisk()
        guard !keys.isEmpty, let index = state.projects.firstIndex(where: { $0.projectID == id }) else { return }
        let existing = state.projects[index].labelKeysByLogName[logName] ?? []
        let (merged, changed) = LabelDetector.merge(existing, with: keys)
        guard changed else { return }
        state.projects[index].labelKeysByLogName[logName] = merged
        persist()
    }

    /// Toggles a label key as a favorite for a (project, log name).
    func toggleFavoriteLabel(_ key: String, project id: String, logName: String) {
        syncFromDisk()
        guard !key.isEmpty, let index = state.projects.firstIndex(where: { $0.projectID == id }) else { return }
        var favorites = state.projects[index].favoriteLabelKeysByLogName[logName] ?? []
        if let at = favorites.firstIndex(of: key) { favorites.remove(at: at) } else { favorites.append(key) }
        state.projects[index].favoriteLabelKeysByLogName[logName] = favorites
        persist()
    }

    /// Replaces the whole example-count rule map for a (project, log name).
    func setLabelExampleRules(_ rules: [String: LabelExampleRule], project id: String, logName: String) {
        update(id) { $0.labelExampleRulesByLogName[logName] = rules }
    }

    private func update(_ id: String, _ change: (inout CloudProject) -> Void) {
        syncFromDisk()
        guard let index = state.projects.firstIndex(where: { $0.projectID == id }) else { return }
        change(&state.projects[index])
        persist()
    }

    private func persist() {
        store.save(state.projects)
        stamps = currentStamps
    }
}
