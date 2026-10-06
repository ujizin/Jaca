import Foundation

/// Topic-based fan-out of events to subscribed connections.
///
/// - **Retained topics** keep their last event; a new subscriber gets it immediately, so a
///   client renders the last known state on connect instead of an empty screen.
/// - **Droppable events** (high-rate streams such as log lines) are skipped for a peer whose
///   socket is backed up. The skip is counted and reported to that peer as an `events.dropped`
///   event before its next delivered event on the topic.
/// - **On-demand producers** start when a topic under their prefix gets its first subscriber
///   and stop when its last one leaves, so the daemon only polls what someone is watching.
final class DaemonEventBus: @unchecked Sendable {
    typealias DemandHook = @Sendable (_ topic: String) -> Void

    private struct Demand {
        let prefix: String
        let start: DemandHook
        let stop: DemandHook
    }

    private let lock = NSLock()
    private var peers: [Int: DaemonPeer] = [:]
    private var subscribers: [String: Set<Int>] = [:]
    private var retained: [String: Data] = [:]
    private var dropped: [Int: [String: Int]] = [:]
    private var demands: [Demand] = []

    // MARK: - Producers

    /// Publishes `data` on `topic`. Encodes once for every subscriber.
    func publish<T: Codable>(_ topic: String, _ data: T, retain: Bool = false, droppable: Bool = false) {
        guard let line = try? DaemonLine.encode(RPCEventEnvelope(topic: topic, data: data)) else {
            DaemonLog.error("event encode failed for \(topic)")
            return
        }
        publishLine(topic, line, retain: retain, droppable: droppable)
    }

    /// Sends happen under the lock. `DaemonPeer.send` only enqueues on the channel's event loop, so
    /// this doesn't block, and it keeps each peer's events in publish order. Sending after
    /// unlocking let a concurrent publish (or a subscribe's replay) overtake a newer line.
    func publishLine(_ topic: String, _ line: Data, retain: Bool, droppable: Bool) {
        lock.lock()
        defer { lock.unlock() }
        if retain { retained[topic] = line }
        for id in subscribers[topic] ?? [] {
            guard let peer = peers[id] else { continue }
            if droppable && !peer.isWritable {
                dropped[id, default: [:]][topic, default: 0] += 1
                continue
            }
            var lines: [Data] = []
            if let count = dropped[id]?[topic], count > 0 {
                dropped[id]?[topic] = nil
                if let note = try? DaemonLine.encode(RPCEventEnvelope(
                    topic: "events.dropped", data: DroppedEvents(topic: topic, count: count))) {
                    lines.append(note)
                }
            }
            lines.append(line)
            lines.forEach(peer.send)
        }
    }

    /// Forgets a retained topic's last value (the thing it described is gone).
    func clearRetained(_ topic: String) {
        lock.lock(); retained[topic] = nil; lock.unlock()
    }

    /// The last retained event line for `topic`, if any.
    func retainedLine(_ topic: String) -> Data? {
        lock.lock(); defer { lock.unlock() }
        return retained[topic]
    }

    /// Runs `start` when a topic with this prefix gains its first subscriber and `stop` when it
    /// loses its last. Both are called outside the bus lock.
    func onDemand(prefix: String, start: @escaping DemandHook, stop: @escaping DemandHook) {
        lock.lock(); demands.append(Demand(prefix: prefix, start: start, stop: stop)); lock.unlock()
    }

    func hasSubscribers(_ topic: String) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return !(subscribers[topic]?.isEmpty ?? true)
    }

    // MARK: - Connections

    func add(_ peer: DaemonPeer) {
        lock.lock(); peers[peer.id] = peer; lock.unlock()
    }

    func remove(peerID id: Int) {
        lock.lock()
        peers[id] = nil
        dropped[id] = nil
        var emptied: [String] = []
        for (topic, var ids) in subscribers where ids.contains(id) {
            ids.remove(id)
            subscribers[topic] = ids.isEmpty ? nil : ids
            if ids.isEmpty { emptied.append(topic) }
        }
        let hooks = demands
        lock.unlock()
        for topic in emptied { hooks.filter { topic.hasPrefix($0.prefix) }.forEach { $0.stop(topic) } }
    }

    /// Every connected peer (the server closes them all when it stops).
    var allPeers: [DaemonPeer] {
        lock.lock(); defer { lock.unlock() }
        return Array(peers.values)
    }

    func subscribe(peerID id: Int, topics: [String]) {
        var started: [String] = []
        lock.lock()
        // The connection may already be gone: its request ran after it closed. Registering it then
        // would leave an id nothing ever removes, keeping the topic's on-demand producer running.
        guard let peer = peers[id] else {
            lock.unlock()
            return
        }
        for topic in topics {
            var ids = subscribers[topic] ?? []
            if ids.isEmpty { started.append(topic) }
            ids.insert(id)
            subscribers[topic] = ids
            // Replayed under the lock, so a publish can't land before the older retained line.
            if let line = retained[topic] { peer.send(line) }
        }
        let hooks = demands
        lock.unlock()
        for topic in started { hooks.filter { topic.hasPrefix($0.prefix) }.forEach { $0.start(topic) } }
    }

    func unsubscribe(peerID id: Int, topics: [String]) {
        var emptied: [String] = []
        lock.lock()
        for topic in topics {
            guard var ids = subscribers[topic], ids.remove(id) != nil else { continue }
            subscribers[topic] = ids.isEmpty ? nil : ids
            if ids.isEmpty { emptied.append(topic) }
        }
        let hooks = demands
        lock.unlock()
        for topic in emptied { hooks.filter { topic.hasPrefix($0.prefix) }.forEach { $0.stop(topic) } }
    }

    var subscriptionCount: Int {
        lock.lock(); defer { lock.unlock() }
        return subscribers.values.reduce(0) { $0 + $1.count }
    }
}

/// Payload of the `events.dropped` event.
struct DroppedEvents: Codable, Sendable, Equatable {
    var topic: String
    var count: Int
}

/// Minimal stderr logging for the daemon (its stderr goes to `~/.jaca/jacad.log`).
enum DaemonLog {
    static func info(_ message: @autoclosure () -> String) { write("info", message()) }
    static func error(_ message: @autoclosure () -> String) { write("error", message()) }

    private static func write(_ level: String, _ message: String) {
        let line = "\(DaemonDates.format(Date())) [\(level)] \(message)\n"
        FileHandle.standardError.write(Data(line.utf8))
    }
}
