import Foundation

/// Runs daemon commands one after another, in the order they were issued. A tab that sends
/// "stop" then "start" must never have them arrive the other way round, which separate tasks
/// can't guarantee.
@MainActor
final class DaemonCommandQueue {
    private var tail: Task<Void, Never>?

    func enqueue(_ operation: @escaping @MainActor () async -> Void) {
        let previous = tail
        tail = Task { @MainActor in
            await previous?.value
            await operation()
        }
    }
}
