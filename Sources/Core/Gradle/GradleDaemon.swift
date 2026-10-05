import Foundation

/// A single running Gradle daemon process, as parsed from `ps`.
struct GradleDaemon: Identifiable, Hashable {
    var id: String { String(pid) }
    let pid: Int32
    var version: String       // e.g. "9.4.1" (or "?" if unparsed)
    var uptime: String        // friendly, e.g. "2h 14m"
    var cpu: Double           // percent
    var memoryMB: Int
    var jdk: String?          // major JDK version, e.g. "21" (nil if unknown)
    var maxHeap: String?      // JVM -Xmx value, e.g. "12g" (nil if unset)
    var removing: Bool = false

    /// Heuristic: a daemon burning CPU is mid-build; near-idle daemons sit ~0%.
    var isBusy: Bool { cpu > 20 }
}

/// A reclaimable directory under `~/.gradle/caches` — a version dir ("9.4.1"), the
/// build cache ("build-cache-1"), the dependency cache ("modules-2"), etc.
struct GradleCacheEntry: Identifiable, Hashable {
    var id: String { name }
    let name: String
    let sizeMB: Int
}

// MARK: - Wire format (daemon)

extension GradleDaemon: Codable {
    // `removing` is view state (a row mid-fade) and never crosses the wire.
    private enum CodingKeys: String, CodingKey { case pid, version, uptime, cpu, memoryMB, jdk, maxHeap }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            pid: try c.decode(Int32.self, forKey: .pid),
            version: try c.decodeIfPresent(String.self, forKey: .version) ?? "?",
            uptime: try c.decodeIfPresent(String.self, forKey: .uptime) ?? "",
            cpu: try c.decodeIfPresent(Double.self, forKey: .cpu) ?? 0,
            memoryMB: try c.decodeIfPresent(Int.self, forKey: .memoryMB) ?? 0,
            jdk: try c.decodeIfPresent(String.self, forKey: .jdk),
            maxHeap: try c.decodeIfPresent(String.self, forKey: .maxHeap)
        )
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(pid, forKey: .pid)
        try c.encode(version, forKey: .version)
        try c.encode(uptime, forKey: .uptime)
        try c.encode(cpu, forKey: .cpu)
        try c.encode(memoryMB, forKey: .memoryMB)
        try c.encodeIfPresent(jdk, forKey: .jdk)
        try c.encodeIfPresent(maxHeap, forKey: .maxHeap)
    }
}

extension GradleCacheEntry: Codable {
    private enum CodingKeys: String, CodingKey { case name, sizeMB }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(name: try c.decode(String.self, forKey: .name),
                  sizeMB: try c.decodeIfPresent(Int.self, forKey: .sizeMB) ?? 0)
    }
}
