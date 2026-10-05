import Foundation

/// A retained topic fed by polling. It polls only while the topic has subscribers, publishes
/// only when the value changes, and keeps the last value so a new subscriber renders it at
/// once. Every client watching the topic shares the one poll loop.
final class PolledTopic<T: Codable & Equatable & Sendable>: @unchecked Sendable {
    let topic: String
    private let bus: DaemonEventBus
    private let interval: Duration
    private let fetch: @Sendable () async -> T

    private let lock = NSLock()
    private var task: Task<Void, Never>?
    private var last: T?
    /// One fetch at a time: a slow tick that started before a mutation must not publish after the
    /// post-mutation `refreshNow`. A request arriving mid-fetch re-polls once it finishes.
    private var polling = false
    private var pollAgain = false

    init(bus: DaemonEventBus, topic: String, interval: Duration, fetch: @escaping @Sendable () async -> T) {
        self.bus = bus
        self.topic = topic
        self.interval = interval
        self.fetch = fetch
        bus.onDemand(prefix: topic,
                     start: { [weak self] t in if t == topic { self?.start() } },
                     stop: { [weak self] t in if t == topic { self?.stop() } })
    }

    /// Fetches and publishes now (after a mutation), without waiting for the next tick.
    func refreshNow() {
        Task { [weak self] in await self?.poll() }
    }

    private func start() {
        lock.lock(); defer { lock.unlock() }
        guard task == nil else { return }
        let interval = self.interval
        task = Task { [weak self] in
            while !Task.isCancelled {
                await self?.poll()
                try? await Task.sleep(for: interval)
            }
        }
    }

    private func stop() {
        lock.lock(); defer { lock.unlock() }
        task?.cancel()
        task = nil
    }

    private func poll() async {
        let proceed: Bool = lock.withLock {
            if polling { pollAgain = true; return false }
            polling = true
            return true
        }
        guard proceed else { return }
        while true {
            let value = await fetch()
            let again: Bool = lock.withLock {
                let changed = value != last
                if changed { last = value }
                if changed { bus.publish(topic, value, retain: true) }
                if pollAgain { pollAgain = false; return true }
                polling = false
                return false
            }
            if !again { return }
        }
    }
}
