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
        bus.onDemand(prefix: listTopic,
                     start: { t in if t == listTopic { Task { @MainActor in engine.start() } } },
                     stop: { t in if t == listTopic { Task { @MainActor in engine.stop() } } })

        let router = server.router
        router.register("devices.list", "Discovered devices (adb, simulators, iOS devices), ordered by platform.") { (_: RPCEmpty, _) in
            await engine.devices
        }
        router.register("devices.reload", "Re-resolves the toolchain (after the adb path setting changes) and restarts discovery.") { (_: RPCEmpty, _) in
            await engine.reload()
            return RPCEmpty()
        }
        router.register("devices.apps", "Installed apps on a device. Empty when the device is unknown or unreachable.",
                        params: DeviceParams.self, concurrent: true) { p, _ in
            guard let device = await engine.device(p.deviceID) else { return [AppEntry]() }
            return await InstalledApps.list(for: device, adbURL: engine.adbURL)
        }
        router.describeTopic(listTopic, "Discovered devices after every change; discovery runs while subscribed. Data: [Device].",
                             retained: true)
    }
}
