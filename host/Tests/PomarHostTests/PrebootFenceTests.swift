import Darwin
import Foundation
import Testing
@testable import PomarHostCore

@Test func refusedAgentOptionsReturnBeforeAnyVMOrWorkspaceEffects() async throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("pomar-refusal-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }
    var o = agentOptions()
    o.directory = dir.path
    o.rootfs = dir.path + "/workspace.ext4"
    o.controlSocket = dir.path + "/agent.sock"
    o.egressSocket = dir.path + "/egress.sock"
    o.cpus = 0
    #expect(await AgentEnvironment.run(o) == 2)
    #expect(try FileManager.default.contentsOfDirectory(atPath: dir.path).isEmpty)
}

@Test func prebootProbeRefusesExistingWorkspaceAndContainer() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("pomar-preboot-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }
    let disk = dir.appendingPathComponent("workspace.ext4")
    let container = dir.appendingPathComponent("containers/agent-fixture-actor")
    var request = AgentFenceProbe.Request(environment: "fixture", session: "session",
        incarnation: "actor", operation: "probe", rootfs: disk.path,
        helper: "/retained/helper", pid: Int32.max, uid: getuid(), birth: 1)
    request.workspaceAbsent = true
    request.container = container.path
    try Data("retained".utf8).write(to: disk)
    chmod(disk.path, 0o600)
    var evidence = try AgentFenceProbe.run(request)
    #expect(!evidence.confirmed)
    #expect(evidence.issues.contains { $0.contains("preboot workspace") })
    try FileManager.default.removeItem(at: disk)
    try FileManager.default.createDirectory(at: container, withIntermediateDirectories: true)
    evidence = try AgentFenceProbe.run(request)
    #expect(!evidence.confirmed)
    #expect(evidence.issues.contains { $0.contains("container absence") })
}

@Test func prebootProbeRetainsUnlinkedWriterUncertainty() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("pomar-unlinked-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }
    let disk = dir.appendingPathComponent("workspace.ext4")
    try Data("still open".utf8).write(to: disk)
    chmod(disk.path, 0o600)
    let handle = try FileHandle(forReadingFrom: disk)
    defer { try? handle.close() }
    try FileManager.default.removeItem(at: disk)
    var request = AgentFenceProbe.Request(environment: "fixture", session: "session",
        incarnation: "actor", operation: "probe", rootfs: disk.path,
        helper: "/retained/helper", pid: Int32.max, uid: getuid(), birth: 1)
    request.workspaceAbsent = true
    request.container = dir.path + "/containers/agent-fixture-actor"
    let evidence = try AgentFenceProbe.run(request)
    #expect(!evidence.confirmed)
    #expect(evidence.issues.contains { $0.contains("unlinked vnode ownership unresolved") })
}
