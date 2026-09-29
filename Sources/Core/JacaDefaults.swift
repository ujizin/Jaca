import Foundation

/// The app's `UserDefaults` domain, from any Jaca process. Inside the app it is `.standard`;
/// inside `jacad` (a bare tool with no bundle id of its own) it is the app's domain opened by
/// name, so both processes read and write the same settings through `cfprefsd`.
enum JacaDefaults {
    static let appDomain = "dev.srsouza.Jaca"

    static let shared: UserDefaults = {
        if Bundle.main.bundleIdentifier == appDomain { return .standard }
        return UserDefaults(suiteName: appDomain) ?? .standard
    }()
}
