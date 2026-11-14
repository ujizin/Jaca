import SwiftUI
import Lemonade

/// The network-inspection mode as two selectable cards. The re-attach setting only means
/// something in Agent mode, so it sits inside the Agent card and folds away in the other mode.
/// Its stored value is kept.
struct NetworkSettingsPane: View {
    /// `AppModel.networkInspectionMode`, and its setter: the model reconfigures capture on a change.
    let mode: NetworkInspectionMode
    let select: (NetworkInspectionMode) -> Void
    /// `AppModel.isSwitchingNetworkMode`: a switch stops captures and waits for their devices, so
    /// the options stay disabled until it is done.
    var switching = false
    /// `AppModel.networkModeError`: why the last switch failed.
    var error: String? = nil

    /// Straight from `UserDefaults`, not `AppModel`: it reconfigures nothing — the supervisor
    /// re-reads it per decision, so it applies to a live tab.
    @AppStorage(FeatureFlags.simulatorAutoReattachKey) private var simulatorAutoReattach = false

    var body: some View {
        SettingsCard(title: "Network inspection") {
            // One choice of how capture works: the in-process agent or a proxy on this Mac.
            // Overrides run in both. No "off": a tab captures either way. The choice stays
            // disabled while a switch stops captures.
            modeOption(.agentHTTPSDebugging, title: "Agent HTTPS debugging", detail:
                "Lets you answer a matched request from a rule instead of the " +
                "real server — right-click any captured request, or use the Overrides button " +
                "next to the search field.\n\n" +
                "It works on apps that pin certificates, because the in-process Agent rewrites " +
                "the request above TLS rather than intercepting the network. While a rule is " +
                "enabled, only the hosts it names are routed through your Mac; everything else " +
                "stays on the device's own network. The tunnel is removed when capture stops, " +
                "and the agent stops diverting on its own if Jaca goes away.") {
                simulatorReattachSetting
            }
            modeOption(.mitmHTTPSDebugging, title: "MITM HTTPS debugging", detail:
                // Approved text, but its first and last sentences are no longer true: the iOS
                // Simulator also uses MITM now. A replacement waits for approval.
                "Captures every app on an Android device through a proxy on your Mac, and decrypts HTTPS with Jaca's certificate. Overrides, delays and groups work as in Agent HTTPS debugging. The device must trust Jaca's certificate: on an emulator Jaca can install it; on a phone you install it once. Apps that pin certificates pass through without decryption. HTTP/2, gRPC and WebSockets over TLS aren't supported. The iOS Simulator keeps using the in-process Agent.") {   // COPY-PENDING
                EmptyView()
            }

            SettingsCaption(text: "Switching stops the running captures.")

            if let error {
                LemonadeUi.Text(
                    "Couldn’t switch the mode: \(error)",   // COPY-PENDING
                    textStyle: LemonadeTypography.shared.bodyXSmallRegular,
                    color: LemonadeTheme.colors.content.contentCritical
                )
                .transition(.opacity)
            }
        }
        .animation(.easeInOut(duration: 0.2), value: error)
    }

    /// One mode as a selectable box: radio and title select it, "Learn more" unfolds the
    /// explainer, and `nested` holds settings that only apply while this mode is selected.
    /// The box itself is not a button, so the controls inside it stay clickable.
    private func modeOption<Nested: View>(_ option: NetworkInspectionMode, title: String, detail: String,
                                          @ViewBuilder nested: @escaping () -> Nested) -> some View {
        let selected = mode == option
        let select = {
            guard !switching else { return }
            withAnimation(.easeInOut(duration: 0.2)) { self.select(option) }
        }
        return LemonadeUi.BoxSelection(variant: .outlined, isSelected: selected,
                                       contentPadding: .spacing400, stretched: true) {
            HStack(alignment: .top, spacing: LemonadeTheme.spaces.spacing300) {
                LemonadeUi.RadioButton(checked: selected, onRadioButtonClicked: select)
                VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing200) {
                    Button(action: select) {
                        SettingsLabel(text: title)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityAddTraits(selected ? [.isButton, .isSelected] : .isButton)

                    SettingsExplainer(text: detail)

                    if selected {
                        nested()
                            .transition(.opacity.combined(with: .move(edge: .top)))
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .clipped()
        }
        .disabled(switching)
        .opacity(switching ? 0.5 : 1)
        .animation(.easeInOut(duration: 0.2), value: switching)
    }

    /// Nested under Agent HTTPS debugging: re-attaching re-injects the agent, so it has no meaning
    /// in the other mode (where `FeatureFlags` also keeps it off).
    private var simulatorReattachSetting: some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing300) {
            LemonadeUi.HorizontalDivider()
            SettingsRow(
                label: "Re-attach to iOS Simulator apps automatically",
                explainer: "Off by default. The agent is injected into the process Jaca launched, so an app " +
                    "you quit and reopen yourself comes back without it — capture and overrides stop. " +
                    "Jaca notices and offers a “Relaunch & re-attach” button on the capture tab.\n\n" +
                    "Turn this on to let it relaunch the app for you instead of asking. It still says so " +
                    "in the status line, and it never opens an app you closed — only one you reopened " +
                    "yourself."
            ) {
                SettingsSwitch(checked: simulatorAutoReattach, onCheckedChange: { simulatorAutoReattach = $0 })
            }
        }
    }
}
