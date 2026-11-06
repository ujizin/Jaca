import SwiftUI
import Lemonade
import AppKit

/// The adb path. It is the one setting that needs an action: "Apply & Rescan Devices"
/// re-resolves adb and restarts discovery. The button is always enabled, so it also serves as
/// a manual rescan.
struct DevicesSettingsPane: View {
    let model: AppModel

    @AppStorage("adbPath") private var adbPath = ""

    var body: some View {
        SettingsCard(title: "Android") {
            VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing200) {
                SettingsLabel(text: "adb path")
                HStack(spacing: LemonadeTheme.spaces.spacing200) {
                    SettingsTextField(placeholder: currentADBPlaceholder, text: $adbPath)
                    LemonadeUi.Button(label: "Browse…", onClick: browseADB,
                                      variant: .neutral, type: .subtle, size: .small)
                        .fixedSize()   // size to the label so the path field gets the width
                    LemonadeUi.Button(label: "Apply & Rescan Devices", onClick: { model.reloadProviders() },
                                      variant: .primary, type: .solid, size: .small)
                        .fixedSize()
                }
                SettingsCaption(text: "Leave blank to auto-detect ($ANDROID_HOME, ~/Library/Android/sdk, PATH).")
            }
        }
    }

    private var currentADBPlaceholder: String {
        AndroidToolchain.adbURL()?.path ?? "adb not found"
    }

    private func browseADB() {
        let panel = NSOpenPanel()
        panel.canChooseFiles = true
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        if panel.runModal() == .OK, let url = panel.url {
            adbPath = url.path
        }
    }
}
