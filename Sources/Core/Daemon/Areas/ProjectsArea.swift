import Foundation

/// The Projects area, served by one `ProjectsEngine`. Its state is published, retained, on
/// `projects.state`; folder watching runs while anyone subscribes.
enum ProjectsArea {
    struct PathParams: Codable, Sendable { var path: String }
    struct IDParams: Codable, Sendable { var id: String }
    struct CheckoutParams: Codable, Sendable { var project: String; var checkout: String }

    static let stateTopic = "projects.state"

    @MainActor
    static func install(on server: DaemonServer, engine: ProjectsEngine? = nil) {
        let engine = engine ?? ProjectsEngine()
        server.keep(engine)
        let bus = server.bus
        engine.onChange = { bus.publish(stateTopic, $0, retain: true) }
        bus.publish(stateTopic, engine.state, retain: true)
        bus.onDemand(prefix: stateTopic,
                     start: { t in if t == stateTopic { Task { @MainActor in engine.startWatching() } } },
                     stop: { t in if t == stateTopic { Task { @MainActor in engine.stopWatching() } } })

        let router = server.router
        router.register("projects.state", "The current projects state (also published on projects.state).") { (_: RPCEmpty, _) in
            await engine.state
        }
        router.register("projects.refresh", "Starts a structural rescan. Progress arrives on projects.state.") { (_: RPCEmpty, _) in
            await engine.refresh()
            return RPCEmpty()
        }
        router.register("projects.computeSizes", "Starts the disk-usage scan of every git checkout (slow; ask the user first).") { (_: RPCEmpty, _) in
            await engine.computeSizes()
            return RPCEmpty()
        }
        router.register("projects.cancelSizes", "Stops a running disk-usage scan.") { (_: RPCEmpty, _) in
            await engine.cancelSizes()
            return RPCEmpty()
        }
        router.register("projects.addFolder", "Adds a folder as a project and rescans. False when already added.",
                        params: PathParams.self) { p, _ in
            var isDir: ObjCBool = false
            guard FileManager.default.fileExists(atPath: p.path, isDirectory: &isDir), isDir.boolValue else {
                throw RPCError.failed("Not a folder: \(p.path)")
            }
            return await engine.addFolder(p.path)
        }
        router.register("projects.removeFolder", "Removes a user-added project (the folder is untouched). Returns its name, or null.",
                        params: IDParams.self) { p, _ in
            await engine.removeUserProject(p.id)
        }
        router.register("projects.clearCache", "Clears a checkout's build caches. Returns {name, freedMB, error}, or null when not applicable.",
                        params: CheckoutParams.self, concurrent: true) { p, _ in
            await engine.clearCache(project: p.project, checkout: p.checkout)
        }
        router.register("projects.deleteWorktree", "Removes a linked worktree. Returns {name, ok, stderr}, or null when not applicable.",
                        params: CheckoutParams.self, concurrent: true) { p, _ in
            await engine.deleteWorktree(project: p.project, checkout: p.checkout)
        }
        router.describeTopic(stateTopic, "The whole Projects state after every change. Data: ProjectsState.", retained: true)
    }
}
