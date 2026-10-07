import Foundation

/// A search over a log session's lines: what `logs.search` runs on the replay buffer in the
/// daemon, and what `jaca logs tail --follow` applies to live batches. Pure.
struct LogSearch {
    var minLevel: LogLevel
    /// Lines stamped before this are left out.
    var since: Date?
    /// A regular expression over the tag and the message, case-insensitive. Empty matches all.
    let pattern: String
    private let regex: NSRegularExpression?

    init(pattern: String = "", minLevel: LogLevel = .verbose, since: Date? = nil) {
        self.pattern = pattern
        self.minLevel = minLevel
        self.since = since
        regex = pattern.isEmpty ? nil : try? NSRegularExpression(pattern: pattern, options: [.caseInsensitive])
    }

    func matches(_ line: LogLine) -> Bool {
        if let since, line.timestamp < since { return false }
        // A crash marker is stamped `.info`; it is what a search for errors is looking for.
        if line.level < minLevel, !line.markerCritical { return false }
        guard !pattern.isEmpty else { return true }
        guard let regex else {
            // Not a valid regular expression: match it as text, like `LogFilter` does.
            return line.message.localizedCaseInsensitiveContains(pattern)
                || line.tag.localizedCaseInsensitiveContains(pattern)
        }
        return Self.found(regex, in: line.message) || Self.found(regex, in: line.tag)
    }

    /// The last `limit` matching lines, oldest first.
    func tail(_ lines: [LogLine], limit: Int) -> [LogLine] {
        guard limit > 0 else { return [] }
        var found: [LogLine] = []
        for line in lines.reversed() where matches(line) {
            found.append(line)
            if found.count == limit { break }
        }
        return found.reversed()
    }

    private static func found(_ regex: NSRegularExpression, in text: String) -> Bool {
        regex.firstMatch(in: text, range: NSRange(text.startIndex..., in: text)) != nil
    }
}
