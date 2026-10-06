import Foundation

/// Persists the override rule library to `~/.jaca/network-overrides/`.
///
/// Layout:
/// ```
/// ~/.jaca/network-overrides/
///   rules.json        — the ordered rule list, hand-editable
///   bodies/<uuid>.bin — payloads too large to inline
/// ```
///
/// Payloads sit in sibling files because a body can be megabytes and `rules.json` has to stay
/// readable. Loading goes through `CloudPersistence.decodeArray`, so one corrupt record is skipped
/// rather than wiping the library.
struct OverrideRuleStore: Sendable {

    /// `JACA_OVERRIDES_DIR` moves the library (tests use a temporary one so they never touch
    /// the user's rules).
    static var directory: URL {
        if let override = ProcessInfo.processInfo.environment["JACA_OVERRIDES_DIR"], !override.isEmpty {
            return URL(fileURLWithPath: override, isDirectory: true)
        }
        return FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".jaca/network-overrides", isDirectory: true)
    }

    static var rulesURL: URL { directory.appendingPathComponent("rules.json") }
    static var bodiesDirectory: URL { directory.appendingPathComponent("bodies", isDirectory: true) }

    /// Loads the rule library, or `[]` when nothing is saved. Synchronous by design: the model
    /// calls it from `init`, so the first frame already has the user's rules.
    static func load() -> [OverrideRule] {
        guard let data = try? Data(contentsOf: rulesURL) else { return [] }
        return CloudPersistence.decodeArray(OverrideRule.self, from: data, decoder: makeDecoder())
    }

    /// Like `load`, but nil when `rules.json` exists and isn't a JSON array (a hand edit left it
    /// broken). A caller holding the library in memory keeps that instead of replacing it with
    /// nothing and saving the empty list over the file.
    static func loadIfReadable() -> [OverrideRule]? {
        guard FileManager.default.fileExists(atPath: rulesURL.path) else { return [] }
        guard let data = try? Data(contentsOf: rulesURL) else { return nil }
        if data.isEmpty { return [] }
        guard (try? JSONSerialization.jsonObject(with: data)) is [Any] else { return nil }
        return CloudPersistence.decodeArray(OverrideRule.self, from: data, decoder: makeDecoder())
    }

    /// Must mirror `save`'s encoder exactly. It writes ISO-8601 dates to stay hand-editable, and
    /// decoding those numerically throws on **every** record — the library then loads empty and
    /// the next save wipes the file and every body blob with it.
    static func makeDecoder() -> JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }

    /// Writes the library atomically and garbage-collects orphaned body blobs.
    @discardableResult
    static func save(_ rules: [OverrideRule]) -> Bool {
        do {
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            let encoder = JSONEncoder()
            encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
            encoder.dateEncodingStrategy = .iso8601
            let data = try encoder.encode(rules)
            try data.write(to: rulesURL, options: .atomic)
            collectGarbage(keeping: rules)
            return true
        } catch {
            return false
        }
    }

    /// Stores a payload, inlining small bodies and spilling large ones to `bodies/`.
    static func makeBodyRef(_ data: Data, preferInline: Bool = true) -> OverrideBodyRef {
        if data.isEmpty { return .none }
        if preferInline, data.count <= OverrideBodyRef.inlineLimit,
           let text = String(data: data, encoding: .utf8) {
            return .inline(text)
        }
        let filename = "\(UUID().uuidString).bin"
        do {
            try FileManager.default.createDirectory(at: bodiesDirectory, withIntermediateDirectories: true)
            try data.write(to: bodiesDirectory.appendingPathComponent(filename), options: .atomic)
            return .blob(filename: filename)
        } catch {
            // Couldn't spill — inline what we can rather than losing the body entirely.
            return String(data: data, encoding: .utf8).map { .inline($0) } ?? .none
        }
    }

    /// Deletes blobs no live rule refers to. Called after every save, so deleting a rule
    /// reclaims its payload without a separate cleanup pass.
    ///
    /// Blobs younger than `gracePeriod` are kept even when unreferenced: `makeBodyRef` writes the
    /// blob when a rule is seeded, before the rule is saved, so a save of *another* rule while
    /// the editor is open (or from the other process, app or `jacad`) would otherwise delete it.
    static func collectGarbage(keeping rules: [OverrideRule], now: Date = Date(),
                               gracePeriod: TimeInterval = 24 * 60 * 60) {
        let fm = FileManager.default
        guard let existing = try? fm.contentsOfDirectory(atPath: bodiesDirectory.path) else { return }
        var live: Set<String> = []
        for rule in rules {
            for ref in bodyRefs(of: rule) {
                if case .blob(let filename) = ref { live.insert(filename) }
            }
        }
        for file in existing where !live.contains(file) {
            let url = bodiesDirectory.appendingPathComponent(file)
            let modified = (try? fm.attributesOfItem(atPath: url.path)[.modificationDate] as? Date) ?? .distantPast
            guard now.timeIntervalSince(modified) >= gracePeriod else { continue }
            try? fm.removeItem(at: url)
        }
    }

    private static func bodyRefs(of rule: OverrideRule) -> [OverrideBodyRef] {
        switch rule.action {
        case .respond(let spec):     return [spec.body]
        case .editResponse(let edit): return edit.body.map { [$0] } ?? []
        case .mapRemote:             return []
        }
    }
}
