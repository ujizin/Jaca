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
        let value = await fetch()
        lock.lock()
        let changed = value != last
        if changed { last = value }
        lock.unlock()
        if changed { bus.publish(topic, value, retain: true) }
    }
}
