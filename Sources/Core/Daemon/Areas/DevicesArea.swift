import Foundation

/// Device discovery and per-device app lists. Discovery runs while `devices.list` has
/// subscribers; the last list is retained for the next one.
enum DevicesArea {
    struct DeviceParams: Codable, Sendable { var deviceID: String }

    static let listTopic = "devices.list"

    @MainActor
    static func install(on server: DaemonServer, engine: DevicesEngine? = nil) {
        let engine = engine ?? DevicesEngine()
        server.keep(engine)
        let bus = server.bus
        engine.onChange = { bus.publish(listTopic, $0, retain: true) }
        // Start and stop hops can land out of order: each follows the subscribers when it runs.
        let follow: DaemonEventBus.DemandHook = { t in
            guard t == listTopic else { return }
            Task { @MainActor in
                if bus.hasSubscribers(listTopic) { engine.start() } else { engine.stop() }
            }
        }
        bus.onDemand(prefix: listTopic, start: follow, stop: follow)

        let router = server.router
        // Concurrent: with nobody watching the topic it runs discovery first, for seconds.
        router.register("devices.list", "Discovered devices (adb, simulators, iOS devices), ordered by platform.",
                        takes: .empty, returns: .array(.device), concurrent: true) { (_: RPCEmpty, _) in
            await engine.snapshot()
        }
        router.register("devices.reload", "Re-resolves the toolchain (after the adb path setting changes) and restarts discovery.",
                        takes: .empty, returns: .empty) { (_: RPCEmpty, _) in
            await engine.reload()
            return RPCEmpty()
        }
        router.register("devices.apps", "Installed apps on a device. Empty when the device is unknown or unreachable.",
                        params: DeviceParams.self,
                        takes: .object(["deviceID": .string]),
                        returns: .array(.object(["id": .string, "isUserApp": .boolean], optional: ["name": .string])),
                        concurrent: true) { p, _ in
            guard let device = await engine.device(p.deviceID) else { return [AppEntry]() }
            return await InstalledApps.list(for: device, adbURL: engine.adbURL)
        }
        router.describeTopic(listTopic, "Discovered devices after every change; discovery runs while subscribed. Data: [Device].",
                             retained: true)
    }
}
