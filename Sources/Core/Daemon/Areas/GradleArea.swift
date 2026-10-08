import Foundation

/// Gradle daemons and `~/.gradle/caches`, served by `GradleDaemonService`.
enum GradleArea {
    struct PidParams: Codable, Sendable { var pid: Int32 }
    struct NameParams: Codable, Sendable { var name: String }

    static let daemonsTopic = "gradle.daemons"

    static func install(on server: DaemonServer) {
        let service = GradleDaemonService()
        let daemons = PolledTopic(bus: server.bus, topic: daemonsTopic, interval: .seconds(2)) {
            await service.list()
        }
        server.keep(daemons)

        server.router.register("gradle.list", "Running Gradle daemons, sorted by pid.", concurrent: true) { (_: RPCEmpty, _) in
            await service.list()
        }
        server.router.register("gradle.kill", "Kills a listed Gradle daemon (SIGTERM; SIGKILL if that fails). Returns whether it worked.",
                               params: PidParams.self, concurrent: true) { p, _ in
            // Only a pid that is a Gradle daemon right now: the caller's list can be stale (pids
            // get reused), and any client can send any pid.
            guard await service.list().contains(where: { $0.pid == p.pid }) else { return false }
            let ok = await service.kill(pid: p.pid)
            daemons.refreshNow()
            return ok
        }
        server.router.register("gradle.caches", "Sizes of the directories under ~/.gradle/caches, largest first. Slow (du).", concurrent: true) { (_: RPCEmpty, _) in
            await service.cacheSizes()
        }
        // Concurrent: a multi-GB delete must not hold up the connection's other calls.
        server.router.register("gradle.deleteCache", "Deletes ~/.gradle/caches/<name>. Returns whether it worked.",
                               params: NameParams.self, concurrent: true) { p, _ in
            await service.deleteCache(name: p.name)
        }
        server.router.describeTopic(daemonsTopic, "Running Gradle daemons, polled every 2s while subscribed. Data: [GradleDaemon].",
                                    retained: true)
    }
}
