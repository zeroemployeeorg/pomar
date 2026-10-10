import Containerization
import Foundation

/// Polling is for the expected wait deadline, not for a disconnected VM.
enum CommandWait {
    static func run(
        stopped: @Sendable () -> Bool,
        wait: @Sendable () async throws -> Int32,
        pause: @Sendable () async throws -> Void = {
            try await Task.sleep(for: .milliseconds(100))
        }
    ) async throws -> Int32? {
        while !stopped() {
            do {
                return try await wait()
            } catch let error as ContainerizationError where error.code == .timeout {
                // A failed RPC can return immediately. Even timeout retries
                // must yield, rather than occupy an executor in a tight loop.
                if !stopped() { try await pause() }
            }
        }
        return nil
    }
}
