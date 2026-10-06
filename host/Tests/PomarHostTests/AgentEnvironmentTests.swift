import Testing
@testable import PomarHostCore

func agentOptions() -> AgentEnvironment.Options {
    .init(environment: "demo", session: "session-one", incarnation: "actor-one",
        directory: "/tmp/agent-demo", store: "/tmp/store", rootfs: "/tmp/agent-demo/workspace.ext4",
        base: "/tmp/base", kernel: "/tmp/kernel", initRef: "init", initDigest: "init-digest",
        imageRef: "image", imageDigest: "image-digest", guestBinary: "/tmp/guest", shimBinary: "/tmp/shim",
        codexArchive: "/tmp/codex.tar.gz", codexArchiveSHA256: String(repeating: "a", count: 64),
        sourceBundle: "/tmp/source.bundle", sourceSHA: String(repeating: "b", count: 40),
        controlSocket: "/tmp/agent-demo/agent.sock", egressSocket: "/tmp/agent-demo/egress.sock",
        goProxySocket: "/tmp/agent-demo/go.sock", cpus: 2, memoryBytes: 4 * 1024 * 1024 * 1024)
}

@Test func agentAdmissionRejectsRedirectedWorkspaceAndUnsafeIdentity() {
    let valid = agentOptions()
    #expect(AgentEnvironment.valid(valid))
    var changed = valid
    changed.rootfs = "/tmp/another/workspace.ext4"
    #expect(!AgentEnvironment.valid(changed))
    changed = valid
    changed.environment = "../other"
    #expect(!AgentEnvironment.valid(changed))
    changed = valid
    changed.sourceSHA = "main"
    #expect(!AgentEnvironment.valid(changed))
    changed = valid
    changed.controlSocket = "/tmp/another/control.sock"
    #expect(!AgentEnvironment.valid(changed))
}

@Test func agentAdmissionEnforcesDevelopmentResourceBounds() {
    for cpus in [0, 5] {
        var o = agentOptions(); o.cpus = cpus
        #expect(!AgentEnvironment.valid(o))
    }
    let invalidMemory: [UInt64] = [1, 9 * 1024 * 1024 * 1024]
    for memory in invalidMemory {
        var o = agentOptions(); o.memoryBytes = memory
        #expect(!AgentEnvironment.valid(o))
    }
}

@Test func controllerCapabilitiesRemainOwnerNamedAndBounded() {
    for invalid in [["inbox", "inbox"], ["/bin/sh"], ["Socket"], [String(repeating: "a", count: 65)], Array(0...16).map { "cap\($0)" }] {
        var o = agentOptions(); o.controllerCapabilities = invalid
        #expect(!AgentEnvironment.valid(o))
    }
    var o = agentOptions(); o.controllerCapabilities = ["inbox", "ack", "answer"]
    #expect(AgentEnvironment.valid(o))
}

import Darwin
import Foundation

@Test func fenceProbeFindsWorkspaceHandleWithoutSignalling() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("pomar-fence-test-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }
    let disk = dir.appendingPathComponent("workspace.ext4")
    try Data("test fixture".utf8).write(to: disk)
    chmod(disk.path, 0o600)
    let handle = try FileHandle(forReadingFrom: disk)
    defer { try? handle.close() }
    var executable = [CChar](repeating: 0, count: 4096)
    #expect(proc_pidpath(getpid(), &executable, UInt32(executable.count)) > 0)
    let request = AgentFenceProbe.Request(environment: "fixture", session: "fixture-session",
        incarnation: "fixture-actor", operation: "fixture-probe", rootfs: disk.path,
        helper: String(cString: executable), pid: Int32.max, uid: getuid(), birth: 1)
    let evidence = try AgentFenceProbe.run(request)
    #expect(!evidence.confirmed)
    #expect(evidence.issues.contains { $0.contains("workspace inode remains open") && $0.contains("pid \(getpid()) ") })
    #expect(!evidence.issues.contains { $0.contains("pid \(getpid()): retained helper executable live") })
    #expect(try Data(contentsOf: disk) == Data("test fixture".utf8))
}

@Test func agentStatusPreservesLegacyUncertainReceipt() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("pomar-status-test-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }
    let original = Data("original vm_stopped=false".utf8)
    try original.write(to: dir.appendingPathComponent("vm-status.json"))
    var options = agentOptions(); options.directory = dir.path
    AgentEnvironment.status(options, "stopped", stopped: true)
    AgentEnvironment.status(options, "running")
    AgentEnvironment.status(options, "stopped", stopped: true)
    #expect(try Data(contentsOf: dir.appendingPathComponent("vm-status.json")) == original)
    #expect(FileManager.default.fileExists(atPath: dir.appendingPathComponent("vm-status-actor-one.json").path))
}
