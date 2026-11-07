import SwiftUI
import Lemonade

/// Theme, Dock visibility, and the Herdr command of the Projects area.
struct GeneralSettingsPane: View {
    let model: AppModel

    @AppStorage("colorScheme") private var colorScheme = "dark"
    @AppStorage(DockVisibility.defaultsKey) private var showInDock = false
    @State private var showHerdrConfig = false

    /// The `colorScheme` values in segment order, with their labels.
    private static let themes: [(value: String, label: String)] = [
        ("system", "System"), ("dark", "Dark"), ("light", "Light"),
    ]

    var body: some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing300) {
            SettingsCard(title: "Appearance") {
                SettingsRow(label: "Theme") {
                    LemonadeUi.SegmentedControl(
                        properties: Self.themes.map { .label($0.label) },
                        selectedTab: Self.themes.firstIndex { $0.value == colorScheme } ?? 0,
                        size: .small,
                        onTabSelected: { index in
                            guard Self.themes.indices.contains(index) else { return }
                            colorScheme = Self.themes[index].value
                        }
                    )
                    .frame(width: 240)
                }
            }

            SettingsCard(title: "Dock & app switcher") {
                SettingsRow(
                    label: "Show Jaca in the Dock and ⌘-Tab",
                    explainer: "Off by default: Jaca lives in the menu bar, so it has no Dock icon and doesn't " +
                        "appear in the ⌘-Tab switcher. macOS ties the two together — turning this on adds " +
                        "both. The menu-bar icon stays either way."
                ) {
                    SettingsSwitch(checked: showInDock, onCheckedChange: { showInDock = $0 })
                }
            }

            SettingsCard(title: "Projects") {
                SettingsNavigationRow(label: "Open in Herdr") { showHerdrConfig = true }
            }
        }
        // Nested in Settings rather than through the Projects area, so closing it returns here.
        .sheet(isPresented: $showHerdrConfig) {
            HerdrConfigSheet(model: model.projects)
        }
    }
}
