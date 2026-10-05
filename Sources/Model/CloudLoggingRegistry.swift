import Foundation
import Observation

/// Transient feedback for the Cloud Logging area (mirrors `ProjectsToast`).
struct CloudLoggingToast: Equatable { var message: String; var systemFallback: String }

/// THE single source of truth for Cloud Logging in the app (per the single-source-of-truth
/// convention, mirrors `CompanionRegistry`): gcloud detection + auth state, the persisted project
/// list, and the **global per-project** state — the selected log name, the cached available log
/// names, and the auto-detected label keys. Every `CloudLogSession` reads this reactively, so
/// changing the log name (or a detected label key appearing) in one tab is reflected in every tab
/// for that project, with no per-view polling.
///
/// The domain work lives in `CloudEngine`: in-process, or in `jacad` (daemon mode), mirrored here
/// from its retained `cloud.state` topic. This type adds the toasts.
@Observable @MainActor
final class CloudLoggingRegistry {
    // MARK: Detection / auth

    private(set) var binaryURL: URL?
    private(set) var isDetecting = false
    private(set) var authState: CloudAuthState = .unknown

    /// True once a `gcloud` binary has been located — gates whether the Cloud Logging UI shows.
    var isAvailable: Bool { binaryURL != nil }
    var cli: GcloudCLI? { binaryURL.map { GcloudCLI(binary: $0) } }
    /// The exact command we tell the user to run in a terminal to sign in.
    let authCommand = "gcloud auth login"

    // MARK: Projects + saved templates (persisted to ~/.jaca by the engine)

    private(set) var projects: [CloudProject] = []
    private(set) var queryTemplates: [CloudQueryTemplate] = []
    private(set) var sqlTemplates: [CloudSqlTemplate] = []

    var toast: CloudLoggingToast?
    private var toastTask: Task<Void, Never>?

    typealias AddResult = CloudAddProjectResult

    /// The engine doing the work in-process (daemon mode off, or the daemon unreachable).
    private(set) var localEngine: CloudEngine?
    private let daemon: DaemonConnector
    let usesDaemon: Bool
    private var daemonWatch: Task<Void, Never>?
    private let makeLocalEngine: () -> CloudEngine

    init(store: CloudProjectStore = CloudProjectStore(), templateStore: CloudTemplateStore = CloudTemplateStore(),
         daemon: DaemonConnector? = nil) {
        let daemon = daemon ?? .shared
        self.daemon = daemon
        self.usesDaemon = daemon.isEnabled(.cloudLogging)
        makeLocalEngine = { CloudEngine(store: store, templateStore: templateStore) }
        if usesDaemon {
            // First frame from disk (read-only); the daemon's retained state follows.
            projects = store.load()
            (queryTemplates, sqlTemplates) = templateStore.load()
            watchDaemon()
        } else {
            startLocalEngine()
        }
    }

    // MARK: - Engine plumbing

    @discardableResult
    private func startLocalEngine() -> CloudEngine {
        if let localEngine { return localEngine }
        let engine = makeLocalEngine()
        engine.onChange = { [weak self] in self?.apply($0) }
        localEngine = engine
        apply(engine.state)
        return engine
    }

    private func watchDaemon() {
        daemonWatch = daemon.watch([CloudArea.stateTopic],
                                   onUnavailable: { [weak self] in self?.startLocalEngine() }) { [weak self] event in
            guard let self, let state = try? event.decode(CloudState.self) else { return }
            if let local = self.localEngine {
                local.onChange = nil
                self.localEngine = nil
            }
            self.apply(state)
        }
    }

    private func apply(_ state: CloudState) {
        binaryURL = state.binaryPath.map { URL(fileURLWithPath: $0) }
        isDetecting = state.isDetecting
        authState = state.authState
        projects = state.projects
        queryTemplates = state.queryTemplates
        sqlTemplates = state.sqlTemplates
    }

    /// Runs `local` on the in-process engine, or `method` on the daemon's. A call that reached
    /// the daemon and failed yields nil rather than running twice.
    private func perform<P: Encodable & Sendable, R: Decodable & Sendable>(
        _ method: String, _ params: P, as type: R.Type, local: (CloudEngine) async -> R
    ) async -> R? {
        if usesDaemon, localEngine == nil {
            do {
                if let value = try await daemon.request(method, params, as: type) { return value }
            } catch {
                return nil
            }
        }
        return await local(startLocalEngine())
    }

    /// Fire-and-forget form for mutations whose only result is the next state.
    /// In-process it runs synchronously, as the registry always did.
    private func send<P: Encodable & Sendable>(_ method: String, _ params: P, local: @escaping (CloudEngine) -> Void) {
        guard usesDaemon, localEngine == nil else {
            local(startLocalEngine())
            return
        }
        Task { _ = await perform(method, params, as: RPCEmpty.self) { local($0); return RPCEmpty() } }
    }

    // MARK: - Templates

    func saveQueryTemplate(name: String, query: CloudLogQuery, rawFilter: String?) {
        Task {
            let saved = await perform("cloud.saveQueryTemplate",
                                      CloudArea.QueryTemplateParams(name: name, query: query, rawFilter: rawFilter),
                                      as: Bool.self) { $0.saveQueryTemplate(name: name, query: query, rawFilter: rawFilter) }
            if saved == true { flash("Saved query template") }
        }
    }

    func deleteQueryTemplate(_ id: UUID) {
        send("cloud.deleteQueryTemplate", CloudArea.TemplateIDParams(id: id)) { $0.deleteQueryTemplate(id) }
    }

    func saveSqlTemplate(name: String, sql: String) {
        Task {
            let saved = await perform("cloud.saveSqlTemplate", CloudArea.SqlTemplateParams(name: name, sql: sql),
                                      as: Bool.self) { $0.saveSqlTemplate(name: name, sql: sql) }
            if saved == true { flash("Saved SQL template") }
        }
    }

    func deleteSqlTemplate(_ id: UUID) {
        send("cloud.deleteSqlTemplate", CloudArea.TemplateIDParams(id: id)) { $0.deleteSqlTemplate(id) }
    }

    // MARK: - Detection & auth

    /// (Re)detects gcloud, then refreshes the auth state. Safe to call repeatedly.
    func detect() {
        send("cloud.detect", RPCEmpty()) { $0.detect() }
    }

    func refreshAuth() async {
        _ = await perform("cloud.refreshAuth", RPCEmpty(), as: RPCEmpty.self) { await $0.refreshAuth(); return RPCEmpty() }
    }

    /// Flips to "not signed in" when a session's gcloud call reports an auth failure.
    func markUnauthenticated() {
        authState = .notAuthenticated
        send("cloud.markUnauthenticated", RPCEmpty()) { $0.markUnauthenticated() }
    }

    // MARK: - Lookup

    func project(_ id: String) -> CloudProject? { projects.first { $0.projectID == id } }

    // MARK: - Mutations

    /// Validates the project id via `gcloud projects describe`, then stores it.
    func addProject(id: String, displayName: String) async -> AddResult {
        let result = await perform("cloud.addProject", CloudArea.AddProjectParams(id: id, displayName: displayName),
                                   as: AddResult.self) { await $0.addProject(id: id, displayName: displayName) }
            ?? .failure("Couldn't validate the project.")
        if result == .added {
            let trimmed = id.trimmingCharacters(in: .whitespacesAndNewlines)
            var shown = CloudProject(projectID: trimmed)
            shown.displayName = displayName.trimmingCharacters(in: .whitespacesAndNewlines)
            flash("Added \(project(trimmed)?.title ?? shown.title)")
        }
        return result
    }

    func setDisplayName(_ name: String, for id: String) {
        send("cloud.setDisplayName", CloudArea.ProjectNameParams(id: id, name: name)) { $0.setDisplayName(name, for: id) }
        flash("Renamed")
    }

    func removeProject(_ id: String) {
        let title = project(id)?.title ?? id
        send("cloud.removeProject", CloudArea.ProjectIDParams(id: id)) { _ = $0.removeProject(id) }
        flash("Removed \(title)", fallback: "eraser")
    }

    /// Sets the GLOBAL selected log name for a project — shared by every open session.
    func setSelectedLogName(_ logName: String?, for id: String) {
        if let index = projects.firstIndex(where: { $0.projectID == id }) {
            projects[index].selectedLogName = logName   // optimistic; the engine's state follows
        }
        send("cloud.setSelectedLogName", CloudArea.LogNameParams(id: id, logName: logName)) {
            $0.setSelectedLogName(logName, for: id)
        }
    }

    func setLogNames(_ names: [String], for id: String) {
        send("cloud.setLogNames", CloudArea.LogNamesParams(id: id, names: names)) { $0.setLogNames(names, for: id) }
    }

    /// Refreshes the available log names from gcloud and caches them.
    func refreshLogNames(for id: String) async {
        let error = await perform("cloud.refreshLogNames", CloudArea.ProjectIDParams(id: id), as: String?.self) {
            await $0.refreshLogNames(for: id)
        }
        if let message = error ?? nil { flash(message, fallback: "warn") }
    }

    /// Toggles a label key as a favorite for a (project, log name).
    func toggleFavoriteLabel(_ key: String, project id: String, logName: String) {
        send("cloud.toggleFavoriteLabel", CloudArea.LabelKeyParams(project: id, logName: logName, key: key)) {
            $0.toggleFavoriteLabel(key, project: id, logName: logName)
        }
    }

    /// The configured example-count rules for a (project, log name), keyed by label key. Missing
    /// keys fall back to `LabelExampleRule.default` at the point of use.
    func labelExampleRules(project id: String, logName: String) -> [String: LabelExampleRule] {
        project(id)?.labelExampleRulesByLogName[logName] ?? [:]
    }

    /// Replaces the whole rule map for a (project, log name).
    func setLabelExampleRules(_ rules: [String: LabelExampleRule], project id: String, logName: String) {
        send("cloud.setLabelExampleRules", CloudArea.LabelRulesParams(project: id, logName: logName, rules: rules)) {
            $0.setLabelExampleRules(rules, project: id, logName: logName)
        }
    }

    // MARK: - Toast

    func flash(_ message: String, fallback: String = "checkmark") {
        toastTask?.cancel()
        toast = CloudLoggingToast(message: message, systemFallback: fallback)
        toastTask = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(2_600))
            if !Task.isCancelled { self?.toast = nil }
        }
    }

    // MARK: - Session plumbing

    /// A feed for a Cloud Logging tab: a daemon session in daemon mode, otherwise an in-process
    /// engine reporting label keys and auth failures to this registry's engine.
    func makeFeed(id: UUID, config: CloudStreamConfig, autoStart: Bool) -> CloudFeed {
        if usesDaemon {
            return RemoteCloudFeed(id: id, config: config, autoStart: autoStart, daemon: daemon)
        }
        let engine = startLocalEngine()
        return CloudStreamEngine(
            id: id,
            cli: { [weak engine] in engine?.cli },
            recordLabels: { [weak engine] keys, project, logName in
                engine?.recordLabelKeys(keys, project: project, logName: logName)
            },
            markUnauthenticated: { [weak engine] in engine?.markUnauthenticated() })
    }
}
