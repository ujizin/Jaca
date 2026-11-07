import SwiftUI
import Lemonade
import AppKit

/// App settings, in five tabbed panes. Every control writes its `UserDefaults` key when it
/// changes; "Done" and Esc only close the sheet. `AppModel.settingsRoute` is the open pane.
struct SettingsView: View {
    let model: AppModel

    private var pane: SettingsPane { model.settingsRoute ?? model.lastSettingsPane }

    var body: some View {
        SettingsSheet(
            pane: pane,
            onSelect: { model.settingsRoute = $0 },
            onDone: { model.closeSettings() }
        ) {
            switch pane {
            case .general: GeneralSettingsPane(model: model)
            case .devices: DevicesSettingsPane(model: model)
            case .network:
                NetworkSettingsPane(mode: model.networkInspectionMode,
                                    select: { model.networkInspectionMode = $0 })
            case .logs: LogsSettingsPane()
            case .diagnostics: DiagnosticsSettingsPane()
            }
        }
    }
}

/// The sheet around the panes: the title, the tabs and the footer stay fixed, and only a pane
/// taller than the sheet scrolls. A tab change cross-fades the pane.
struct SettingsSheet<Pane: View>: View {
    let pane: SettingsPane
    let onSelect: (SettingsPane) -> Void
    let onDone: () -> Void
    @ViewBuilder var content: () -> Pane

    /// Read once from the screen, so the fixed tabs and footer stay on a short screen.
    private let sheetHeight = SettingsLayout.sheetHeight(visibleHeight: NSScreen.main?.visibleFrame.height)

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            LemonadeUi.Text("Settings", textStyle: LemonadeTypography.shared.headingSmall,
                            color: LemonadeTheme.colors.content.contentPrimary)
                .padding(.horizontal, LemonadeTheme.spaces.spacing600)
                .padding(.top, LemonadeTheme.spaces.spacing500)
                .padding(.bottom, LemonadeTheme.spaces.spacing200)

            LemonadeUi.Tabs(
                tabs: SettingsPane.allCases.map { LemonadeTabItem(label: $0.title, icon: $0.icon) },
                selectedIndex: pane.rawValue,
                onTabSelected: { index in
                    withAnimation(.easeInOut(duration: 0.2)) { onSelect(SettingsPane(index: index)) }
                }
            )
            .padding(.horizontal, LemonadeTheme.spaces.spacing600)

            ScrollView(.vertical) {
                content()
                    .id(pane)
                    .transition(.opacity)
                    .frame(maxWidth: SettingsLayout.contentWidth, alignment: .leading)
                    .frame(maxWidth: .infinity)
                    .padding(.horizontal, LemonadeTheme.spaces.spacing600)
                    .padding(.vertical, LemonadeTheme.spaces.spacing400)
            }
            .scrollBounceBehavior(.basedOnSize)
            .frame(maxHeight: .infinity)

            LemonadeUi.HorizontalDivider()
            HStack {
                Spacer()
                LemonadeUi.Button(label: "Done", onClick: onDone,
                                  variant: .neutral, type: .subtle, size: .small)
                    .fixedSize()
                    .keyboardShortcut(.cancelAction)
            }
            .padding(.horizontal, LemonadeTheme.spaces.spacing600)
            .padding(.vertical, LemonadeTheme.spaces.spacing300)
        }
        .frame(width: SettingsLayout.width, height: sheetHeight)
        .background(LemonadeTheme.colors.background.bgDefault)
    }
}

enum SettingsLayout {
    static let width: CGFloat = 680
    static let contentWidth: CGFloat = 600

    /// 540 pt, or less on a short screen so the footer stays on screen. The panes scroll, so
    /// they absorb the lost height; the floor keeps the tabs and footer usable.
    static func sheetHeight(visibleHeight: CGFloat?) -> CGFloat {
        guard let visibleHeight else { return 540 }
        return max(320, min(540, visibleHeight - 80))
    }
}

private extension SettingsPane {
    var icon: LemonadeIcon {
        switch self {
        case .general: return .gear
        case .devices: return .smartphone
        case .network: return .globe
        case .logs: return .list
        case .diagnostics: return .bug
        }
    }
}
