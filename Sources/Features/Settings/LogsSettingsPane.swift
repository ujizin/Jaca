import SwiftUI
import Lemonade

/// Excluded log messages, the two log display settings that also live in the log tabs, and
/// history retention.
struct LogsSettingsPane: View {
    @AppStorage("retentionDays") private var retentionDays = 7
    @State private var exclusions: [LogExcludeRule] = []
    @State private var showCopyFormat = false

    var body: some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing300) {
            SettingsCard(title: "Excluded log messages") {
                SettingsCaption(text: "Lines matching any rule are hidden in every log tab.")
                VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing200) {
                    ForEach($exclusions) { $rule in
                        exclusionRow($rule)
                            .transition(.opacity.combined(with: .move(edge: .top)))
                    }
                    if exclusions.isEmpty {
                        SettingsCaption(text: "No rules — nothing is excluded.")
                            .transition(.opacity)
                    }
                }
                LemonadeUi.Button(label: "Add rule",
                                  onClick: {
                                      withAnimation(.easeInOut(duration: 0.2)) {
                                          exclusions.append(LogExcludeRule(value: "", mode: .prefix))
                                      }
                                  },
                                  variant: .neutral, type: .subtle, size: .small)
                    .fixedSize()
            }

            // The same store as the log tab's "Prettify JSON" chip, so the two stay in step.
            SettingsCard {
                SettingsRow(label: "Prettify JSON",
                            explainer: "Auto-prettify endpoint response bodies (double-click a body to collapse it)") {
                    SettingsSwitch(checked: LogBodyPrettifyStore.shared.enabled,
                                      onCheckedChange: { LogBodyPrettifyStore.shared.enabled = $0 })
                }
                LemonadeUi.HorizontalDivider()
                SettingsNavigationRow(label: "Copy format") { showCopyFormat = true }
            }

            SettingsCard(title: "History") {
                // Read once at launch (`AppModel.init`), as before.
                Stepper(value: $retentionDays, in: 1...365) {
                    LemonadeUi.Text(
                        "Keep logs for \(retentionDays) day\(retentionDays == 1 ? "" : "s")",
                        textStyle: LemonadeTypography.shared.bodySmallSemiBold,
                        color: LemonadeTheme.colors.content.contentPrimary
                    )
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
        .onAppear { exclusions = LogExclusionStore.shared.rules }
        // Every edit applies at once, keystrokes included, as before.
        .onChange(of: exclusions) { _, new in LogExclusionStore.shared.update(new) }
        // Nested in Settings rather than through `RootView`, so closing it returns here.
        .sheet(isPresented: $showCopyFormat) { LogCopyFormatSheet() }
    }

    private func exclusionRow(_ rule: Binding<LogExcludeRule>) -> some View {
        let modes = LogExcludeRule.Mode.allCases
        return HStack(spacing: LemonadeTheme.spaces.spacing200) {
            LemonadeUi.SegmentedControl(
                properties: modes.map { .label($0.label) },
                selectedTab: modes.firstIndex(of: rule.wrappedValue.mode) ?? 0,
                size: .small,
                onTabSelected: { index in
                    guard modes.indices.contains(index) else { return }
                    rule.wrappedValue.mode = modes[index]
                }
            )
            .frame(width: 180)
            SettingsTextField(placeholder: "value", text: rule.value)
            LemonadeUi.IconButton(
                icon: .trash, contentDescription: "Remove rule",
                onClick: {
                    let id = rule.wrappedValue.id
                    withAnimation(.easeInOut(duration: 0.2)) { exclusions.removeAll { $0.id == id } }
                },
                variant: .critical, type: .ghost, size: .small
            )
            .help("Remove rule")
        }
    }
}
