import Foundation

/// The Jaca app bundle, from any Jaca process. Inside the app it's `Bundle.main`; inside `jacad`
/// (`Jaca.app/Contents/MacOS/jacad`, a bare tool) it's the enclosing `.app`, so bundled resources
/// (the in-process agents, the companion APK) resolve the same way in both.
enum JacaBundle {
    static let app: Bundle = {
        if Bundle.main.bundleURL.pathExtension == "app" { return Bundle.main }
        guard let exe = Bundle.main.executableURL?.resolvingSymlinksInPath() else { return Bundle.main }
        // …/Jaca.app/Contents/MacOS/jacad → …/Jaca.app
        let appURL = exe.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        guard appURL.pathExtension == "app", let bundle = Bundle(url: appURL) else { return Bundle.main }
        return bundle
    }()
}
