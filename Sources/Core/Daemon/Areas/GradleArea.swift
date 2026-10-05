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
        server.router.register("gradle.kill", "Kills a daemon (SIGTERM, then SIGKILL). Returns whether it worked.",
                               params: PidParams.self) { p, _ in
            let ok = await service.kill(pid: p.pid)
            daemons.refreshNow()
            return ok
        }
        server.router.register("gradle.caches", "Sizes of the directories under ~/.gradle/caches, largest first. Slow (du).", concurrent: true) { (_: RPCEmpty, _) in
            await service.cacheSizes()
        }
        server.router.register("gradle.deleteCache", "Deletes ~/.gradle/caches/<name>. Returns whether it worked.",
                               params: NameParams.self) { p, _ in
            await service.deleteCache(name: p.name)
        }
        server.router.describeTopic(daemonsTopic, "Running Gradle daemons, polled every 2s while subscribed. Data: [GradleDaemon].",
                                    retained: true)
    }
}
