import Foundation

/// The panes of the Settings sheet, in tab order. `AppModel.settingsRoute` holds the open
/// one (nil while Settings is closed).
enum SettingsPane: Int, CaseIterable, Identifiable {
    case general, devices, network, logs, diagnostics

    var id: Int { rawValue }

    var title: String {
        switch self {
        case .general: return "General"
        case .devices: return "Devices"
        case .network: return "Network"
        case .logs: return "Logs"
        case .diagnostics: return "Diagnostics"
        }
    }

    /// The pane at a tab index. An index out of range maps to the first pane.
    init(index: Int) {
        self = SettingsPane(rawValue: index) ?? .general
    }

    /// The pane Settings opens on: the requested pane, else the last pane shown.
    static func opening(_ requested: SettingsPane?, last: SettingsPane) -> SettingsPane {
        requested ?? last
    }
}
