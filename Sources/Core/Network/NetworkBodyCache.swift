import Foundation

/// Disk cache for request/response bodies of older network transactions, so a
/// long-running session keeps the heavy payloads on disk (not RAM) while the list
/// keeps only lightweight metadata in memory. Ephemeral — wiped on launch.
actor NetworkBodyCache {
    private let dir: URL
    private let fm = FileManager.default

    /// `name` is the folder under `~/Library/Caches/Jaca`. The app and `jacad` each use their
    /// own: each wipes its folder when it starts, which must not take the other's bodies.
    convenience init?(name: String = "net-bodies") {
        guard let caches = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask).first else { return nil }
        self.init(directory: caches.appendingPathComponent("Jaca/\(name)", isDirectory: true))
    }

    init?(directory: URL) {
        dir = directory
        try? fm.removeItem(at: dir)   // a body cache only needs to outlive the current run
        do { try fm.createDirectory(at: dir, withIntermediateDirectories: true) }
        catch { return nil }
    }

    private struct Blob: Codable { var req: Data?; var resp: Data? }

    /// Persists a transaction's bodies; no-op if both are empty.
    func save(_ id: UUID, req: Data?, resp: Data?) {
        guard req != nil || resp != nil else { return }
        if let data = try? PropertyListEncoder().encode(Blob(req: req, resp: resp)) {
            try? data.write(to: url(id), options: .atomic)
        }
    }

    func load(_ id: UUID) -> (req: Data?, resp: Data?) {
        guard let data = try? Data(contentsOf: url(id)),
              let blob = try? PropertyListDecoder().decode(Blob.self, from: data) else { return (nil, nil) }
        return (blob.req, blob.resp)
    }

    private func url(_ id: UUID) -> URL { dir.appendingPathComponent(id.uuidString) }
}
