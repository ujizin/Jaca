import XCTest
@testable import Jaca

/// This Mac's Chrome capture: the command line Chrome gets, the CA hash it trusts, and which
/// capture options the Mac and the devices are offered.
@MainActor
final class MacBrowserCaptureTests: XCTestCase {
    func test_arguments_pointChromeAtTheProxyWithItsOwnProfile() {
        let profile = URL(fileURLWithPath: "/tmp/jaca profile")
        XCTAssertEqual(ChromeBrowser.arguments(proxyPort: 51234, spkiHash: "abc=", profile: profile), [
            "--user-data-dir=/tmp/jaca profile",
            "--proxy-server=http://127.0.0.1:51234",
            "--ignore-certificate-errors-spki-list=abc=",
            "--disable-quic",
            "--no-first-run",
            "--no-default-browser-check",
        ])
    }

    func test_lockTarget_givesThePid() {
        XCTAssertEqual(ChromeBrowser.pid(fromLockTarget: "Lucass-MacBook-Pro.local-4242"), 4242)
        XCTAssertEqual(ChromeBrowser.pid(fromLockTarget: "host-with-dashes-7"), 7)
        for bad in ["", "host", "host-", "host-x", "host-0"] {
            XCTAssertNil(ChromeBrowser.pid(fromLockTarget: bad), bad)
        }
    }

    func test_lockHolder_readsTheSymlink_andIsNilWithout() throws {
        let profile = FileManager.default.temporaryDirectory.appendingPathComponent("jaca-chrome-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: profile, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: profile) }
        XCTAssertNil(ChromeBrowser.lockHolder(profile: profile))
        try FileManager.default.createSymbolicLink(atPath: profile.appendingPathComponent("SingletonLock").path,
                                                   withDestinationPath: "somehost-31337")
        XCTAssertEqual(ChromeBrowser.lockHolder(profile: profile), 31337)
    }

    func test_appURL_isNilWithoutChrome() {
        let empty = FileManager.default.temporaryDirectory.appendingPathComponent("jaca-home-\(UUID().uuidString)")
        let noChrome = FileManagerWithout(path: "Google Chrome")
        XCTAssertNil(ChromeBrowser.appURL(fileManager: noChrome, home: empty))
    }

    /// The hash Chrome compares is base64(SHA-256(SPKI DER)); openssl computes the same from the PEM.
    func test_rootSPKIHash_matchesOpenSSL() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("jaca-ca-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let ca = try CertificateAuthority(directory: dir)
        let pem = dir.appendingPathComponent("rootCA.pem").path
        let shell = Process()
        shell.executableURL = URL(fileURLWithPath: "/bin/sh")
        shell.arguments = ["-c", "openssl x509 -in '\(pem)' -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | base64"]
        let out = Pipe()
        shell.standardOutput = out
        try shell.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        shell.waitUntilExit()
        let expected = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        try XCTSkipIf(expected.isEmpty, "openssl unavailable")
        XCTAssertEqual(ca.rootSPKIHash, expected)
    }

    func test_registry_offersTheBrowserOnlyForThisMac() {
        let mac = CaptureSourceRegistry.options(for: .thisMac, context: nil).map(\.id)
        XCTAssertEqual(mac, ["browser"])
        XCTAssertEqual(CaptureSourceRegistry.defaultDescriptor(for: .thisMac, context: nil)?.kind, .browser)
        let pixel = Device(id: "emulator-5554", platform: .android, model: "Pixel 8", state: .connected)
        XCTAssertFalse(CaptureSourceRegistry.options(for: pixel, context: nil).contains { $0.kind == .browser })
    }

    /// A tab saved with this Mac decodes back to it; an older build reads the unknown platform
    /// as Android instead of failing.
    func test_thisMac_roundTrips() throws {
        let data = try JSONEncoder().encode(Device.thisMac)
        XCTAssertEqual(try JSONDecoder().decode(Device.self, from: data), Device.thisMac)
        XCTAssertEqual(DevicePlatform(rawValue: "mac"), .mac)
    }
}

/// A file manager that reports no file whose path contains `path`.
private final class FileManagerWithout: FileManager {
    let path: String
    init(path: String) { self.path = path }
    override func fileExists(atPath p: String) -> Bool { p.contains(path) ? false : super.fileExists(atPath: p) }
}
