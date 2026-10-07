import Foundation

/// Registers every area's methods and topics on a server. The one place that lists what the
/// daemon serves; `jacad serve` and the in-process test server both call it.
enum DaemonAreas {
    @MainActor
    static func install(on server: DaemonServer) {
        GradleArea.install(on: server)
        XcodeArea.install(on: server)
        ProjectsArea.install(on: server)
        DevicesArea.install(on: server)
        LogsArea.install(on: server)
        CloudArea.install(on: server)
        DatabaseArea.install(on: server)
        let overrides = OverridesArea.install(on: server)
        NetworkArea.install(on: server, interceptServices: {
            FeatureFlags.responseOverridesEnabled ? overrides.services() : nil
        })
        server.router.register("daemon.diagnostics", "What this daemon can reach: the app bundle, bundled agents, private frameworks.") { (_: RPCEmpty, _) in
            await Diagnostics.current()
        }
    }

    struct Diagnostics: Codable, Sendable, Equatable {
        var appBundle: String
        var androidAgentBundled: Bool
        var iosSimulatorAgentBundled: Bool
        /// Apple's private LoggingSupport + MobileDevice load (physical iOS structured logs).
        var loggingSupportAvailable: Bool
        var adbPath: String?

        @MainActor
        static func current() -> Diagnostics {
            Diagnostics(
                appBundle: JacaBundle.app.bundlePath,
                androidAgentBundled: AgentArtifacts.isAvailable,
                iosSimulatorAgentBundled: AgentArtifacts.iosNetworkAgentAvailable,
                loggingSupportAvailable: JacaOSLogStream.isAvailable(),
                adbPath: AndroidToolchain.adbURL(override: JacaDefaults.shared.string(forKey: DevicesEngine.adbPathKey))?.path)
        }
    }
}
