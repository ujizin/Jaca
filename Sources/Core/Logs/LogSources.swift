import Foundation

/// Builds the log sources for a device. Shared by the in-process session and `jacad`, so both
/// stream a device exactly the same way.
enum LogSources {
    /// Whether the device has a usable log source here (Android needs adb).
    static func isSupported(_ device: Device, adbURL: URL?) -> Bool {
        device.platform != .android || adbURL != nil
    }

    /// The primary source factory. A factory (not a fixed instance) so the session can
    /// re-spawn the tool to reconnect after a device/stream drop. The bundle id matters only
    /// for physical iOS, where it narrows the structured stream to that app.
    static func primary(for device: Device, adbURL: URL?) -> @Sendable (String) -> LogSource? {
        { bundleID in
            switch device.platform {
            case .android: return adbURL.map { AndroidLogSource(adbURL: $0, serial: device.id) }
            case .iosSimulator: return SimulatorLogSource(udid: device.id)
            case .iosDevice:
                // Structured logs (level · subsystem · category) via Apple's private
                // LoggingSupport engine (OSActivityStream) — the Xcode/Console-grade
                // stream. `bundleID` here is the selected app's process/display name and
                // narrows the whole-device stream to that app (empty = whole device).
                // Falls back to idevicesyslog internally if the private API is unavailable.
                return IOSDeviceOSLogSource(udid: device.id, processFilter: bundleID)
            }
        }
    }

    /// Simulators can additionally stream the targeted app's stdout (`print()`), which OSLog
    /// can't see, by launching it under a PTY. Other platforms have no stdout tap.
    static func console(for device: Device) -> (@Sendable (String) -> LogSource?)? {
        guard device.platform == .iosSimulator else { return nil }
        return { bundleID in
            guard !bundleID.isEmpty else { return nil }
            return SimulatorConsoleLogSource(udid: device.id, bundleID: bundleID)
        }
    }
}
