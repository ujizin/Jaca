import Foundation

/// Runs daemon commands one after another, in the order they were issued. A tab that sends
/// "stop" then "start" must never have them arrive the other way round, which separate tasks
/// can't guarantee.
@MainActor
final class DaemonCommandQueue {
    private var tail: Task<Void, Never>?
    private var pending = 0

    /// Whether any command hasn't finished yet.
    var hasPending: Bool { pending > 0 }

    func enqueue(_ operation: @escaping @MainActor () async -> Void) {
        let previous = tail
        pending += 1
        tail = Task { @MainActor [weak self] in
            await previous?.value
            await operation()
            self?.pending -= 1
        }
    }
}
