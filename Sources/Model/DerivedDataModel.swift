import Foundation
import Observation

/// A transient toast shown over the Xcode area (mirrors `GradleToast`).
struct XcodeToast: Equatable { var message: String; var systemFallback: String }

/// State for the Xcode top-level area: the DerivedData entries with their sizes and
/// live/stale/shared classification. Sizes come from `du`, so the list is refreshed
/// on demand (on appear / via the refresh button), not polled.
@Observable
@MainActor
final class DerivedDataModel {
    var entries: [DerivedDataEntry] = []
    var toast: XcodeToast? = nil
    var isLoading = false

    var totalMB: Int { entries.reduce(0) { $0 + $1.sizeMB } }
    var staleCount: Int { entries.filter { $0.kind == .stale }.count }
    var staleMB: Int { entries.filter { $0.kind == .stale }.reduce(0) { $0 + $1.sizeMB } }

    private let service = DerivedDataService()
    private var toastTask: Task<Void, Never>?

    /// Daemon mode: list and delete through `jacad`, falling back to the in-process service
    /// whenever the daemon can't be reached.
    private let daemon: DaemonConnector
    private var useDaemon: Bool { daemon.isEnabled(.xcode) }

    init(daemon: DaemonConnector? = nil) {
        let daemon = daemon ?? .shared
        self.daemon = daemon
    }

    // MARK: - Toast

    func flash(_ message: String, fallback: String = "checkmark") {
        toastTask?.cancel()
        toast = XcodeToast(message: message, systemFallback: fallback)
        toastTask = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(2600))
            if !Task.isCancelled { self?.toast = nil }
        }
    }

    // MARK: - Refresh

    func refresh() {
        isLoading = true
        let service = self.service
        let viaDaemon = useDaemon
        Task { [weak self] in
            var list: [DerivedDataEntry]?
            if viaDaemon { list = await self?.daemon.call("xcode.list", as: [DerivedDataEntry].self) }
            if list == nil { list = await service.list() }
            guard let self, let list else { return }
            self.entries = list
            self.isLoading = false
        }
    }

    // MARK: - Delete

    /// Deletes a single entry's folder (fades the row, then drops it).
    func delete(_ path: String) {
        guard let i = entries.firstIndex(where: { $0.path == path }) else { return }
        let name = entries[i].name
        entries[i].removing = true
        Task { [weak self] in
            let ok = await self?.delete(path: path) ?? false
            guard let self else { return }
            if ok {
                try? await Task.sleep(for: .milliseconds(280))
                self.entries.removeAll { $0.path == path }
                self.flash("Deleted \(name)", fallback: "eraser")
            } else {
                if let j = self.entries.firstIndex(where: { $0.path == path }) {
                    self.entries[j].removing = false
                }
                self.flash("Couldn't delete \(name)", fallback: "eraser")
            }
        }
    }

    /// Deletes every stale entry in one pass (the header "Clean stale" action).
    func cleanAllStale() {
        let stale = entries.filter { $0.kind == .stale }
        guard !stale.isEmpty else { return }
        for i in entries.indices where entries[i].kind == .stale { entries[i].removing = true }
        Task { [weak self] in
            var deleted = 0
            for entry in stale where await self?.delete(path: entry.path) ?? false { deleted += 1 }
            guard let self else { return }
            try? await Task.sleep(for: .milliseconds(280))
            let paths = Set(stale.map(\.path))
            self.entries.removeAll { paths.contains($0.path) }
            self.flash("Deleted \(deleted) stale", fallback: "eraser")
        }
    }

    /// Deletes through the daemon, or in-process when daemon mode is off or it can't be
    /// reached. A daemon-side failure counts as not deleted, never a retry.
    private func delete(path: String) async -> Bool {
        if useDaemon {
            do {
                if let ok = try await daemon.request("xcode.delete", XcodeArea.PathParams(path: path), as: Bool.self) {
                    return ok
                }
            } catch {
                return false
            }
        }
        return await service.delete(path: path)
    }
}
