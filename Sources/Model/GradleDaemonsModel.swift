import Foundation
import Observation

/// A transient toast shown over the Gradle area (mirrors `WorktreesToast`).
struct GradleToast: Equatable { var message: String; var systemFallback: String }

/// State for the Gradle top-level area: the live list of Gradle daemons, polled
/// every couple of seconds, with kill actions. Mirrors `WorktreesModel`'s toast +
/// polling idioms.
@Observable
@MainActor
final class GradleDaemonsModel {
    var daemons: [GradleDaemon] = []
    var cacheEntries: [GradleCacheEntry] = []
    var cacheLoading = false
    var toast: GradleToast? = nil

    var cacheTotalMB: Int { cacheEntries.reduce(0) { $0 + $1.sizeMB } }

    private let service = GradleDaemonService()
    private var timer: Task<Void, Never>?
    private var toastTask: Task<Void, Never>?

    /// Daemon mode: the list comes from `jacad`'s shared `gradle.daemons` topic and actions go
    /// through it. Falls back to the in-process service whenever the daemon can't be reached.
    private let daemon: DaemonConnector
    private var useDaemon: Bool { daemon.isEnabled(.gradle) }

    init(daemon: DaemonConnector? = nil) {
        let daemon = daemon ?? .shared
        self.daemon = daemon
    }

    // MARK: - Toast

    func flash(_ message: String, fallback: String = "checkmark") {
        toastTask?.cancel()
        toast = GradleToast(message: message, systemFallback: fallback)
        toastTask = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(2600))
            if !Task.isCancelled { self?.toast = nil }
        }
    }

    // MARK: - Refresh

    func refresh() {
        let service = self.service
        let viaDaemon = useDaemon
        Task { [weak self] in
            var list: [GradleDaemon]?
            if viaDaemon { list = await self?.daemon.call("gradle.list", as: [GradleDaemon].self) }
            if list == nil { list = await service.list() }
            self?.apply(list ?? [])
        }
    }

    private func refreshInProcess() {
        let service = self.service
        Task { [weak self] in self?.apply(await service.list()) }
    }

    private func apply(_ list: [GradleDaemon]) {
        // Preserve the in-flight removing flag so a row mid-fade doesn't reappear.
        // A row already gone from the list keeps fading out until `kill` removes it: in daemon
        // mode the list without it arrives before the fade ends.
        let removing = daemons.filter(\.removing)
        let removingPIDs = Set(removing.map(\.pid))
        var next = list.map { d in
            guard removingPIDs.contains(d.pid) else { return d }
            var copy = d
            copy.removing = true
            return copy
        }
        let listed = Set(list.map(\.pid))
        next.append(contentsOf: removing.filter { !listed.contains($0.pid) })
        daemons = next.sorted { $0.pid < $1.pid }
    }

    /// Per-version `~/.gradle/caches` sizes. Computed on demand (du is slow), not polled.
    /// Independent of whether any daemon is running — the cache lives on disk regardless.
    func refreshCache() {
        cacheLoading = true
        let service = self.service
        let viaDaemon = useDaemon
        Task { [weak self] in
            var entries: [GradleCacheEntry]?
            if viaDaemon { entries = await self?.daemon.call("gradle.caches", as: [GradleCacheEntry].self) }
            if entries == nil { entries = await service.cacheSizes() }
            guard let self, let entries else { return }
            self.cacheEntries = entries
            self.cacheLoading = false
        }
    }

    /// Deletes a cache dir (`~/.gradle/caches/<name>`) and drops it from the list.
    func deleteCache(_ name: String) {
        let service = self.service
        let viaDaemon = useDaemon
        Task { [weak self] in
            let ok = await self?.perform(viaDaemon, "gradle.deleteCache", GradleArea.NameParams(name: name)) {
                await service.deleteCache(name: name)
            } ?? false
            guard let self else { return }
            if ok {
                self.cacheEntries.removeAll { $0.name == name }
                self.flash("Deleted cache \(name)", fallback: "eraser")
            } else {
                self.flash("Couldn't delete \(name)", fallback: "eraser")
            }
        }
    }

    // MARK: - Kill

    func kill(_ pid: Int32) {
        guard let i = daemons.firstIndex(where: { $0.pid == pid }) else { return }
        daemons[i].removing = true
        let service = self.service
        let viaDaemon = useDaemon
        Task { [weak self] in
            let ok = await self?.perform(viaDaemon, "gradle.kill", GradleArea.PidParams(pid: pid)) {
                await service.kill(pid: pid)
            } ?? false
            guard let self else { return }
            if ok {
                try? await Task.sleep(for: .milliseconds(280))
                self.daemons.removeAll { $0.pid == pid }
                self.flash("Killed \(pid)", fallback: "eraser")
            } else {
                if let j = self.daemons.firstIndex(where: { $0.pid == pid }) {
                    self.daemons[j].removing = false
                }
                self.flash("Couldn't kill \(pid)", fallback: "eraser")
            }
        }
    }

    // MARK: - Polling

    func startPolling() {
        timer?.cancel()
        if useDaemon {
            // One shared poll loop in the daemon; while it's unreachable, poll in-process.
            timer = daemon.watch([GradleArea.daemonsTopic],
                                 onUnavailable: { [weak self] in self?.refreshInProcess() }) { [weak self] event in
                guard let list = try? event.decode([GradleDaemon].self) else { return }
                self?.apply(list)
            }
            return
        }
        timer = Task { [weak self] in
            while !Task.isCancelled {
                self?.refresh()
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }

    /// Runs a Bool-returning action through the daemon, or in-process when daemon mode is off
    /// or the daemon can't be reached. A daemon-side failure counts as `false`, never a retry.
    private func perform<P: Encodable & Sendable>(_ viaDaemon: Bool, _ method: String, _ params: P,
                                       inProcess: () async -> Bool) async -> Bool {
        if viaDaemon {
            do {
                if let ok = try await daemon.request(method, params, as: Bool.self) { return ok }
            } catch {
                return false
            }
        }
        return await inProcess()
    }

    func stopPolling() {
        timer?.cancel()
        timer = nil
    }
}
