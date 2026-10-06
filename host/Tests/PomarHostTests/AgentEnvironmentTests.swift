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

// The Codex default is byte-for-byte the setup command before the agent
// option existed; Claude Code installs into its own paths.
@Test func setupCommandKeepsCodexAndAddsClaude() {
    let sha = String(repeating: "b", count: 40)
    let codexGolden =
        "set -eu; " + Helper.registerJobUser + "; "
        + "mkdir -p /pomar/job /var/lib/pomar-agent /run/pomar; "
        + "mkdir -p /etc/profile.d; printf '%s\\n' 'export PATH=/opt/pomar-codex/codex-path:/pomar/job/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin' > /etc/profile.d/pomar-agent.sh; chmod 644 /etc/profile.d/pomar-agent.sh; "
        + "chmod 700 /var/lib/pomar-agent /run/pomar; rm -f /run/pomar/agent.sock; "
        + "if [ ! -e /var/lib/pomar-agent/workspace-created ]; then "
        + "git init -q /work; git -C /work fetch -q --tags /pomar/source.bundle HEAD; "
        + "git -C /work -c advice.detachedHead=false checkout -q --detach " + sha + "; "
        + "test \"$(git -C /work rev-parse HEAD)\" = " + sha + "; "
        + "chown -R 1000:1000 /work /pomar/job; touch /var/lib/pomar-agent/workspace-created; fi; "
        + "mkdir -p /opt/pomar-codex /pomar/job/.codex; chmod 700 /pomar/job/.codex; "
        + "tar -xzf /pomar/codex-package.tar.gz -C /opt/pomar-codex; "
        + "chmod -R go-w /opt/pomar-codex; chown -R 1000:1000 /pomar/job"
    #expect(AgentEnvironment.setupCommand(sourceSHA: sha) == ["/bin/sh", "-c", codexGolden])
    #expect(AgentEnvironment.setupCommand(sourceSHA: sha, agent: "codex") == ["/bin/sh", "-c", codexGolden])
    let claude = AgentEnvironment.setupCommand(sourceSHA: sha, agent: "claude")[2]
    #expect(claude.contains("export PATH=/opt/pomar-claude/bin:"))
    #expect(claude.contains("mkdir -p /opt/pomar-claude /pomar/job/.claude; chmod 700 /pomar/job/.claude; rm -f /run/pomar-claude/bridge.sock;"))
    #expect(claude.contains("tar -xzf /pomar/codex-package.tar.gz -C /opt/pomar-claude;"))
    #expect(!claude.contains("pomar-codex"))
}

@Test func agentChoiceIsValidated() {
    var o = agentOptions()
    #expect(AgentEnvironment.valid(o))  // the Codex default
    o.agent = "claude"; o.agentVersion = "2.1.280"
    #expect(AgentEnvironment.valid(o))
    for bad in [nil, "", "latest", "2.1.280; rm", ".2.1", "2.1."] {
        o.agentVersion = bad
        #expect(!AgentEnvironment.valid(o))
    }
    o.agentVersion = "2.1.280"; o.controllerCapabilities = ["inbox"]
    #expect(!AgentEnvironment.valid(o))  // the controller bridge is Codex-only
    var codex = agentOptions(); codex.agentVersion = "1.0.0"
    #expect(!AgentEnvironment.valid(codex))
    var other = agentOptions(); other.agent = "other"
    #expect(!AgentEnvironment.valid(other))
}

@Test func brokerArgumentsNameTheAgent() {
    let codex = AgentEnvironment.brokerArguments(agentOptions())
    #expect(codex == ["/pomar/agent-guest", "-environment", "demo", "-session", "session-one", "-incarnation", "actor-one",
                      "-source-sha", String(repeating: "b", count: 40), "-controller-capabilities", ""])
    var o = agentOptions(); o.agent = "claude"; o.agentVersion = "2.1.280"
    #expect(AgentEnvironment.brokerArguments(o) == codex + ["-actor", "claude", "-claude", "/opt/pomar-claude/bin/claude", "-claude-version", "2.1.280"])
}
