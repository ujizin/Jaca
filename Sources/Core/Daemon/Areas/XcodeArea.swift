import Foundation

/// Xcode DerivedData, served by `DerivedDataService`.
enum XcodeArea {
    struct PathParams: Codable, Sendable { var path: String }

    static func install(on server: DaemonServer) {
        let service = DerivedDataService()
        server.router.register("xcode.list", "DerivedData folders with size and live/stale/shared kind, stale first. Slow (du).", concurrent: true) { (_: RPCEmpty, _) in
            await service.list()
        }
        server.router.register("xcode.delete", "Deletes one folder directly inside DerivedData. Returns whether it worked.",
                               params: PathParams.self, concurrent: true) { p, _ in
            // Concurrent: deleting several GB must not hold up the connection's other calls.
            await service.delete(path: p.path)
        }
    }
}
