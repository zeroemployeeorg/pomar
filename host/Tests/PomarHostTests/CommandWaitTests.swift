import Containerization
import Foundation
import Testing
@testable import PomarHostCore

private final class WaitScenario: @unchecked Sendable {
    private let lock = NSLock()
    private var calls = 0
    private var pauses = 0
    private var stop = false

    func next() -> Int { lock.lock(); defer { lock.unlock() }; calls += 1; return calls }
    func pause(stopping: Bool = false) {
        lock.lock(); defer { lock.unlock() }; pauses += 1; stop = stopping
    }
    var stopped: Bool { lock.lock(); defer { lock.unlock() }; return stop }
    var counts: (Int, Int) { lock.lock(); defer { lock.unlock() }; return (calls, pauses) }
}

@Test func waitReturnsTheActualCommandExitCode() async throws {
    let code = try await CommandWait.run(stopped: { false }, wait: { 17 })
    #expect(code == 17)
}

@Test func waitRetriesTimeoutsWithAPause() async throws {
    let scenario = WaitScenario()
    let code = try await CommandWait.run(stopped: { false }, wait: {
        if scenario.next() <= 2 { throw ContainerizationError(.timeout, message: "poll deadline") }
        return 0
    }, pause: { scenario.pause() })
    #expect(code == 0)
    #expect(scenario.counts.0 == 3)
    #expect(scenario.counts.1 == 2)
}

@Test func disconnectedVMFailsAfterOneWait() async {
    let scenario = WaitScenario()
    do {
        _ = try await CommandWait.run(stopped: { false }, wait: {
            _ = scenario.next()
            throw ContainerizationError(.internalError, message: "VM disconnected")
        }, pause: { scenario.pause() })
        Issue.record("a disconnected VM must fail")
    } catch let error as ContainerizationError {
        #expect(error.code == .internalError)
    } catch { Issue.record("unexpected error: \(error)") }
    #expect(scenario.counts.0 == 1)
    #expect(scenario.counts.1 == 0)
}

@Test func stopBeforeWaitDoesNotCallTheAgent() async throws {
    let scenario = WaitScenario()
    let code = try await CommandWait.run(stopped: { true }, wait: { _ = scenario.next(); return 0 })
    #expect(code == nil)
    #expect(scenario.counts.0 == 0)
}

@Test func stopAfterTimeoutEndsPolling() async throws {
    let scenario = WaitScenario()
    let code = try await CommandWait.run(stopped: { scenario.stopped }, wait: {
        _ = scenario.next()
        throw ContainerizationError(.timeout, message: "poll deadline")
    }, pause: { scenario.pause(stopping: true) })
    #expect(code == nil)
    #expect(scenario.counts.0 == 1)
    #expect(scenario.counts.1 == 1)
}

@Test func cancellationDoesNotBecomeAWaitRetry() async {
    let scenario = WaitScenario()
    do {
        _ = try await CommandWait.run(stopped: { false }, wait: {
            _ = scenario.next(); throw CancellationError()
        }, pause: { scenario.pause() })
        Issue.record("cancellation must propagate")
    } catch is CancellationError {} catch { Issue.record("unexpected error: \(error)") }
    #expect(scenario.counts.0 == 1)
    #expect(scenario.counts.1 == 0)
}
