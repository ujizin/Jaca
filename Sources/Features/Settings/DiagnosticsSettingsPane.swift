import SwiftUI
import Lemonade

/// Verbose diagnostic logging, as it was under Network inspection.
struct DiagnosticsSettingsPane: View {
    @AppStorage(JacaLog.verboseKey) private var verboseLogging = false

    var body: some View {
        SettingsCard {
            SettingsRow(
                label: "Verbose diagnostic logging",
                explainer: "Off by default. Writes a line to ~/.jaca/logs/jaca.log for each bounced request " +
                    "and each heartbeat. Useful when a divert isn't behaving, but it is a synchronous " +
                    "file write per request — leave it off during normal capture."
            ) {
                // The setter also updates `JacaLog`'s cached flag, which hot paths read.
                SettingsSwitch(checked: verboseLogging, onCheckedChange: {
                    verboseLogging = $0
                    JacaLog.verboseEnabled = $0
                })
            }
        }
    }
}
