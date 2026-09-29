import Foundation

// MARK: - LogLine on the daemon socket

/// Log lines are the highest-volume payload on the socket, so the wire form uses short keys,
/// a numeric timestamp, and omits flags that are false. Missing fields decode to defaults;
/// only `s` (seq) is required.
///
///     {"s":8,"t":1790511514.123,"l":2,"g":"MyTag","p":123,"i":456,"m":"hello","r":"…raw…"}
extension LogLine: Codable {
    private enum CodingKeys: String, CodingKey {
        case seq = "s", timestamp = "t", level = "l", tag = "g", pid = "p", tid = "i"
        case message = "m", raw = "r", processName = "n"
        case isMarker = "mk", markerCritical = "mc", isConsoleOutput = "co", bodyCompact = "bc"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let message = try c.decodeIfPresent(String.self, forKey: .message) ?? ""
        self.init(
            seq: try c.decode(UInt64.self, forKey: .seq),
            timestamp: Date(timeIntervalSince1970: try c.decodeIfPresent(Double.self, forKey: .timestamp) ?? 0),
            level: (try? c.decodeIfPresent(LogLevel.self, forKey: .level)) ?? .verbose,
            tag: try c.decodeIfPresent(String.self, forKey: .tag) ?? "",
            pid: try c.decodeIfPresent(Int32.self, forKey: .pid) ?? -1,
            tid: try c.decodeIfPresent(Int32.self, forKey: .tid) ?? 0,
            message: message,
            // Omitted when identical to the message (markers, most iOS lines).
            raw: try c.decodeIfPresent(String.self, forKey: .raw) ?? message,
            processName: try c.decodeIfPresent(String.self, forKey: .processName),
            isMarker: try c.decodeIfPresent(Bool.self, forKey: .isMarker) ?? false,
            markerCritical: try c.decodeIfPresent(Bool.self, forKey: .markerCritical) ?? false,
            isConsoleOutput: try c.decodeIfPresent(Bool.self, forKey: .isConsoleOutput) ?? false,
            bodyCompact: try c.decodeIfPresent(String.self, forKey: .bodyCompact)
        )
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(seq, forKey: .seq)
        try c.encode(timestamp.timeIntervalSince1970, forKey: .timestamp)
        try c.encode(level, forKey: .level)
        if !tag.isEmpty { try c.encode(tag, forKey: .tag) }
        try c.encode(pid, forKey: .pid)
        if tid != 0 { try c.encode(tid, forKey: .tid) }
        try c.encode(message, forKey: .message)
        if raw != message { try c.encode(raw, forKey: .raw) }
        try c.encodeIfPresent(processName, forKey: .processName)
        if isMarker { try c.encode(true, forKey: .isMarker) }
        if markerCritical { try c.encode(true, forKey: .markerCritical) }
        if isConsoleOutput { try c.encode(true, forKey: .isConsoleOutput) }
        try c.encodeIfPresent(bodyCompact, forKey: .bodyCompact)
    }
}
