import Foundation

/// Launches Google Chrome on this Mac with its own profile, sending its traffic through Jaca's
/// proxy and accepting certificates signed by Jaca's CA. The user's normal Chrome profile, the
/// system proxy and the Keychain are left alone.
enum ChromeBrowser {
    static let bundleName = "Google Chrome.app"

    /// The Chrome app, from /Applications or ~/Applications; nil when it isn't installed.
    static func appURL(fileManager: FileManager = .default,
                       home: URL = FileManager.default.homeDirectoryForCurrentUser) -> URL? {
        let candidates = [URL(fileURLWithPath: "/Applications").appendingPathComponent(bundleName),
                          home.appendingPathComponent("Applications").appendingPathComponent(bundleName)]
        return candidates.first { fileManager.fileExists(atPath: executable(in: $0).path) }
    }

    static func executable(in app: URL) -> URL {
        app.appendingPathComponent("Contents/MacOS/Google Chrome")
    }

    /// The profile Jaca's Chrome window uses, kept between launches so sign-ins survive.
    static func profileDirectory(home: URL = FileManager.default.homeDirectoryForCurrentUser) -> URL {
        home.appendingPathComponent("Library/Application Support/Jaca/chrome-profile", isDirectory: true)
    }

    /// The command line. Chrome honours the SPKI list only with a non-default profile, which
    /// `--user-data-dir` gives it. QUIC is off so every request takes the proxy, and
    /// `<-loopback>` sends localhost through it too, which Chrome otherwise never proxies.
    static func arguments(proxyPort: Int, spkiHash: String, profile: URL) -> [String] {
        [
            "--user-data-dir=\(profile.path)",
            "--proxy-server=http://127.0.0.1:\(proxyPort)",
            "--ignore-certificate-errors-spki-list=\(spkiHash)",
            "--proxy-bypass-list=<-loopback>",
            "--disable-quic",
            "--no-first-run",
            "--no-default-browser-check",
        ]
    }

    /// The pid a running Chrome holds the profile with. Chrome writes `SingletonLock` as a
    /// symlink to "<host>-<pid>".
    static func lockHolder(profile: URL, fileManager: FileManager = .default) -> pid_t? {
        let lock = profile.appendingPathComponent("SingletonLock").path
        guard let target = try? fileManager.destinationOfSymbolicLink(atPath: lock) else { return nil }
        return pid(fromLockTarget: target)
    }

    static func pid(fromLockTarget target: String) -> pid_t? {
        guard let dash = target.lastIndex(of: "-"), let pid = Int32(target[target.index(after: dash)...]),
              pid > 0 else { return nil }
        return pid
    }

    /// Starts Chrome. A Chrome that still holds the profile (from a capture that didn't end
    /// cleanly) would take the new window with its old proxy port, so it is closed first,
    /// unless it is `running`, this capture's own Chrome: then the new process hands the
    /// profile's Chrome a new window and exits. Closing the last window leaves Chrome running,
    /// so that is how the window comes back. Returns the process, or a message to show.
    static func launch(proxyPort: Int, spkiHash: String, running: pid_t? = nil) -> Result<Process, LaunchError> {
        guard let app = appURL() else { return .failure(.notInstalled) }
        let profile = profileDirectory()
        try? FileManager.default.createDirectory(at: profile, withIntermediateDirectories: true)
        if let stale = lockHolder(profile: profile), stale != running, kill(stale, 0) == 0 {
            kill(stale, SIGTERM)
            let deadline = Date().addingTimeInterval(3)
            while kill(stale, 0) == 0, Date() < deadline { usleep(50_000) }
        }

        let process = Process()
        process.executableURL = executable(in: app)
        process.arguments = arguments(proxyPort: proxyPort, spkiHash: spkiHash, profile: profile)
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
        } catch {
            return .failure(.failed(error.localizedDescription))
        }
        return .success(process)
    }

    enum LaunchError: Error, Equatable {
        case notInstalled
        case failed(String)

        var message: String {
            switch self {
            case .notInstalled: return "Google Chrome isn’t installed."
            case .failed(let reason): return "Couldn’t open Chrome: \(reason)"
            }
        }
    }
}
