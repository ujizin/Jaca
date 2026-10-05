import Foundation
import Observation

/// App-wide, persisted toggle for auto-prettifying detected JSON response bodies in the
/// log stream (`LogBodyPrettifier`). On by default. The gate is read once per flush, so
/// flipping it off leaves already-prettified lines as they are and simply stops
/// transforming **new** lines — exactly the "next logs won't be prettified" behaviour.
///
/// One owner, many readers (the toolbar chip and every session's flush), per the
/// single-source-of-truth convention — mirrors `LogExclusionStore`.
@MainActor
@Observable
final class LogBodyPrettifyStore {
    static let shared = LogBodyPrettifyStore()

    private static let key = "logPrettifyJSONBodies"

    var enabled: Bool {
        didSet { JacaDefaults.shared.set(enabled, forKey: Self.key) }
    }

    private init() {
        // Absent key → default ON.
        enabled = Self.isEnabled()
    }

    /// The persisted value, for a process without the observable store (`jacad` reads it on
    /// every flush, so the app's toggle takes effect there too).
    nonisolated static func isEnabled(in defaults: UserDefaults = JacaDefaults.shared) -> Bool {
        defaults.object(forKey: key) as? Bool ?? true
    }
}
