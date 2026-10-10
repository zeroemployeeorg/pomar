import Darwin
import Foundation
import Testing
@testable import PomarHostCore

@Test func processUsageReadsOnlyTheNamedOwnProcess() throws {
    let sample = try ProcessUsage.read(pid: getpid())
    #expect(sample.pid == getpid())
    #expect(sample.uid == getuid())
    #expect(sample.startedSeconds > 0)
    #expect(sample.residentBytes > 0)
    #expect(sample.physicalFootprintBytes > 0)
    #expect(ProcessUsage.sameProcess(sample, try ProcessUsage.read(pid: getpid())))
    #expect(throws: ProcessUsage.Failure.self) { try ProcessUsage.read(pid: -1) }
}

@Test func processUsageRefusesChangedIdentity() throws {
    let a = try ProcessUsage.read(pid: getpid())
    let b = ProcessUsage.Snapshot(
        schema: a.schema, time: a.time, pid: a.pid, uid: a.uid, executable: a.executable,
        startedSeconds: a.startedSeconds + 1, startedMicroseconds: a.startedMicroseconds,
        residentBytes: a.residentBytes, physicalFootprintBytes: a.physicalFootprintBytes,
        userCPUTimeNanoseconds: a.userCPUTimeNanoseconds, systemCPUTimeNanoseconds: a.systemCPUTimeNanoseconds)
    #expect(!ProcessUsage.sameProcess(a, b))
}
