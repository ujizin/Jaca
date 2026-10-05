import Foundation

/// Device discovery: one `DeviceProvider` per platform (adb, simctl, devicectl), merged into a
/// single list ordered by platform. The app runs one in-process when the daemon is off; `jacad`
/// runs one and publishes `devices.list`. Companion devices are merged in by the app on top.
@MainActor
final class DevicesEngine {
    private(set) var devices: [Device] = [] {
        didSet { if devices != oldValue { onChange?(devices) } }
    }
    var onChange: (([Device]) -> Void)?

    /// Resolved adb, or nil when the Android toolchain wasn't found.
    private(set) var adbURL: URL?

    private let defaults: UserDefaults
    private let makeProviders: (URL?) -> [DeviceProvider]
    private var providers: [DeviceProvider] = []
    private var tasks: [Task<Void, Never>] = []
    private var byPlatform: [DevicePlatform: [Device]] = [:]

    static let adbPathKey = "adbPath"

    init(defaults: UserDefaults = JacaDefaults.shared,
         makeProviders: @escaping (URL?) -> [DeviceProvider] = DevicesEngine.defaultProviders) {
        self.defaults = defaults
        self.makeProviders = makeProviders
        resolveProviders()
    }

    nonisolated static func defaultProviders(adbURL: URL?) -> [DeviceProvider] {
        var providers: [DeviceProvider] = []
        if let adbURL { providers.append(AndroidDeviceProvider(adbURL: adbURL)) }
        providers.append(SimulatorDeviceProvider())   // self-guards when no Xcode
        providers.append(IOSDeviceProvider())          // self-guards when no devicectl
        return providers
    }

    var isRunning: Bool { !tasks.isEmpty }

    func start() {
        guard tasks.isEmpty else { return }
        for provider in providers {
            let platform = provider.platform
            tasks.append(Task { [weak self] in
                for await list in provider.deviceStream() {
                    guard let self else { return }
                    self.byPlatform[platform] = list
                    self.merge()
                }
            })
        }
    }

    func stop() {
        tasks.forEach { $0.cancel() }
        tasks.removeAll()
    }

    /// Re-resolves the toolchain (e.g. after the adb path changes in Settings) and restarts
    /// discovery if it was running.
    func reload() {
        let wasRunning = isRunning
        stop()
        byPlatform.removeAll()
        devices = []
        resolveProviders()
        if wasRunning { start() }
    }

    private func resolveProviders() {
        adbURL = AndroidToolchain.adbURL(override: defaults.string(forKey: Self.adbPathKey))
        providers = makeProviders(adbURL)
    }

    private func merge() {
        devices = byPlatform
            .sorted { $0.key.rawValue < $1.key.rawValue }
            .flatMap { $0.value }
    }
}
