import Foundation

/// How a DerivedData entry relates to a project on disk.
enum DerivedDataKind: String, Codable {
    case live    // its WorkspacePath still exists
    case stale   // its WorkspacePath is gone (e.g. a deleted worktree's build)
    case shared  // a shared cache (ModuleCache.noindex, SymbolCache, …) — not project-bound
}

/// One folder under `~/Library/Developer/Xcode/DerivedData`, with its size and
/// whether the project/workspace it was built for still exists.
struct DerivedDataEntry: Identifiable, Hashable {
    var id: String { path }
    let name: String            // project name (or the cache name for shared entries)
    let path: String            // full path to the DerivedData subfolder
    let workspacePath: String?  // resolved WorkspacePath (nil for shared caches)
    let sizeMB: Int
    let kind: DerivedDataKind
    var removing: Bool = false
}

// MARK: - Wire format (daemon)

extension DerivedDataEntry: Codable {
    // `removing` is view state (a row mid-fade) and never crosses the wire.
    private enum CodingKeys: String, CodingKey { case name, path, workspacePath, sizeMB, kind }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let path = try c.decode(String.self, forKey: .path)
        self.init(
            name: try c.decodeIfPresent(String.self, forKey: .name) ?? URL(fileURLWithPath: path).lastPathComponent,
            path: path,
            workspacePath: try c.decodeIfPresent(String.self, forKey: .workspacePath),
            sizeMB: try c.decodeIfPresent(Int.self, forKey: .sizeMB) ?? 0,
            // An unknown kind from a newer daemon reads as shared: never offered by "clean stale".
            kind: (try? c.decodeIfPresent(DerivedDataKind.self, forKey: .kind)) ?? .shared
        )
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(name, forKey: .name)
        try c.encode(path, forKey: .path)
        try c.encodeIfPresent(workspacePath, forKey: .workspacePath)
        try c.encode(sizeMB, forKey: .sizeMB)
        try c.encode(kind, forKey: .kind)
    }
}
