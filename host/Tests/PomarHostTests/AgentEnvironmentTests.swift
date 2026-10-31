import Foundation
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
        + "git init -q /work; git -C /work fetch -q --tags /pomar/source.bundle 'refs/heads/*:refs/remotes/origin/*'; "
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
    #expect(AgentEnvironment.valid(o))  // the guest broker gates it by the executable pin
    o.controllerCapabilities = ["not a name"]
    #expect(!AgentEnvironment.valid(o))
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

// A seat profile's environment runs the interactive seat, not the headless
// actor: Claude Code only, a valid seat name, no controller capabilities
// and no qualification fixture.
@Test func aSeatRunsTheInteractiveSeat() {
    var o = agentOptions(); o.agent = "claude"; o.agentVersion = "2.1.280"; o.seat = "zeocreator"
    #expect(AgentEnvironment.valid(o))
    let codex = AgentEnvironment.brokerArguments(agentOptions())
    #expect(AgentEnvironment.brokerArguments(o) == codex + ["-actor", "seat", "-seat", "zeocreator", "-claude", "/opt/pomar-claude/bin/claude", "-claude-version", "2.1.280"])
    var codexSeat = agentOptions(); codexSeat.agent = "codex"; codexSeat.seat = "zeocreator"
    #expect(!AgentEnvironment.valid(codexSeat))
    codexSeat.agentVersion = "0.160.0"
    codexSeat.seatLaunch = .init(sandbox:"workspace-write",approvals:"on-request",network_access:true,model:"fixture-model",effort:"high",cwd:"/work")
    #expect(AgentEnvironment.valid(codexSeat))
    #expect(Array(AgentEnvironment.brokerArguments(codexSeat).prefix(codex.count+8)) == codex + ["-actor", "codex-seat", "-seat", "zeocreator", "-codex", "/opt/pomar-codex/bin/codex", "-codex-version", "0.160.0"])
    #expect(AgentEnvironment.brokerArguments(codexSeat).contains("-codex-launch"))
    codexSeat.controllerCapabilities = ["inbox"]
    #expect(!AgentEnvironment.valid(codexSeat))
    codexSeat.controllerCapabilities = []; codexSeat.agentVersion = "0.170.0"
    #expect(!AgentEnvironment.valid(codexSeat))
    for bad in ["", "Zeocreator", "1seat", "zeo creator", "zeo/../x", String(repeating: "a", count: 64)] {
        var b = o; b.seat = bad
        #expect(!AgentEnvironment.valid(b), "\(bad)")
    }
    var withController = o; withController.controllerCapabilities = ["inbox"]
    #expect(!AgentEnvironment.valid(withController))
}

@Test func codexSeatRefusesMissingAndInconsistentLaunchAtOwnerGate() {
    var o = agentOptions(); o.agent = "codex"; o.agentVersion = "0.160.0"; o.seat = "example"
    #expect(!AgentEnvironment.valid(o))
    o.seatLaunch = .init(sandbox:"read-only",approvals:"never",network_access:true,model:"fixture-model",effort:"high",cwd:"/work")
    #expect(!AgentEnvironment.valid(o))
    o.seatLaunch?.sandbox = "workspace-write"; o.seatLaunch?.effort = "max"
    #expect(!AgentEnvironment.valid(o))
    o.seat = nil; o.seatLaunch = nil
    #expect(!AgentEnvironment.valid(o))
}

@Test func onlyCodexSeatRegistersDedicatedTerminalIdentity() {
    let sha = String(repeating: "b", count: 40)
    let seat = AgentEnvironment.setupCommand(sourceSHA: sha, codexSeat: true)[2]
    #expect(seat.contains("test \"$(getent passwd 1001)\" = 'pomar-terminal:x:1001:1001:Pomar terminal:/run/pomar-codex-terminal:/bin/false'"))
    #expect(seat.contains("! getent passwd pomar-terminal >/dev/null"))
    #expect(seat.contains("test \"$(getent group 1001)\" = 'pomar-terminal:x:1001:'"))
    #expect(!AgentEnvironment.setupCommand(sourceSHA: sha)[2].contains("pomar-terminal"))
    #expect(!AgentEnvironment.setupCommand(sourceSHA: sha, agent: "claude", codexSeat: true)[2].contains("pomar-terminal"))
}

// POMAR-CC-SOW-37: a real, not just string-level, regression for the
// missing-HEAD bundle bug. internal/mirror.Bundle records a bundle's ref
// under its own branch name, never a literal "HEAD" -- the real guest's
// setup script ("git fetch ... HEAD") failed against exactly that shape
// with "couldn't find remote ref HEAD" (confirmed directly against a live
// guest this session, vm.log). This test takes the ACTUAL generated setup
// script from AgentEnvironment.setupCommand -- not a hand-typed stand-in
// -- redirects its hardcoded /work and /pomar/source.bundle paths into a
// real temp sandbox, and runs the real init/fetch/checkout sequence
// against a real branch-only bundle. If the fetch line in the real source
// regresses to the literal "HEAD" form, this test fails for real, because
// it executes whatever setupCommand actually emits, not a copy of it.
@Test func bundleFetchSurvivesABranchOnlyBundleWithoutALiteralHEAD() throws {
    func run(_ args: [String], cwd: String) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        p.arguments = ["git"] + args
        p.currentDirectoryURL = URL(fileURLWithPath: cwd)
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        try p.run()
        p.waitUntilExit()
        return p.terminationStatus
    }
    func shell(_ command: String, cwd: String) throws -> (Int32, String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = ["-c", command]
        p.currentDirectoryURL = URL(fileURLWithPath: cwd)
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        try p.run()
        p.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        return (p.terminationStatus, String(data: data, encoding: .utf8) ?? "")
    }

    let root = NSTemporaryDirectory() + "pcc-bundle-regress-\(UUID().uuidString)"
    try FileManager.default.createDirectory(atPath: root, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(atPath: root) }

    let source = root + "/source"
    try FileManager.default.createDirectory(atPath: source, withIntermediateDirectories: true)
    #expect(try run(["init", "-q", "-b", "feature/pcc-bundle-test"], cwd: source) == 0)
    #expect(try run(["config", "user.email", "t@example.com"], cwd: source) == 0)
    #expect(try run(["config", "user.name", "t"], cwd: source) == 0)
    try "x".write(toFile: source + "/f", atomically: true, encoding: .utf8)
    #expect(try run(["add", "f"], cwd: source) == 0)
    #expect(try run(["commit", "-q", "-m", "c1"], cwd: source) == 0)
    let head = try {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        p.arguments = ["git", "rev-parse", "HEAD"]
        p.currentDirectoryURL = URL(fileURLWithPath: source)
        let pipe = Pipe(); p.standardOutput = pipe
        try p.run(); p.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        return String(data: data, encoding: .utf8)!.trimmingCharacters(in: .whitespacesAndNewlines)
    }()

    // Exactly what internal/mirror.Bundle produces: a bundle whose only ref
    // is the branch name, never a literal HEAD.
    let bundle = root + "/source.bundle"
    #expect(try run(["bundle", "create", "-q", bundle, "refs/heads/feature/pcc-bundle-test"], cwd: source) == 0)

    // The REAL generated script, not a hand-typed copy: if the source
    // regresses, this string -- and therefore this test -- changes with it.
    let fullScript = AgentEnvironment.setupCommand(sourceSHA: head)[2]
    guard let startRange = fullScript.range(of: "git init -q /work;"),
          let endRange = fullScript.range(of: "; chown -R 1000:1000")
    else {
        Issue.record("could not locate the workspace-setup segment in the real generated script")
        return
    }
    var segment = String(fullScript[startRange.lowerBound..<endRange.lowerBound])
    let work = root + "/work"
    try FileManager.default.createDirectory(atPath: work, withIntermediateDirectories: true)
    segment = segment.replacingOccurrences(of: "/work", with: work)
    segment = segment.replacingOccurrences(of: "/pomar/source.bundle", with: bundle)

    let (status, output) = try shell(segment, cwd: root)
    #expect(status == 0, "the real generated setup script must succeed against a real branch-only bundle: \(output)")
    let (rev, _) = try shell("git -C \(work) rev-parse HEAD", cwd: root)
    #expect(rev == 0)
}

@Test func retainedDiskNeverFallsBackToFreshClone() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    var o = agentOptions(); o.rootfs = dir.appendingPathComponent("seat.ext4").path; o.requireRetainedDisk = true
    #expect(throws: (any Error).self) { try AgentEnvironment.ensureRootfs(o) }
    #expect(!FileManager.default.fileExists(atPath: o.rootfs))
    let history = Data("synthetic retained histories".utf8)
    try history.write(to: URL(fileURLWithPath: o.rootfs))
    try AgentEnvironment.ensureRootfs(o)
    #expect(try Data(contentsOf: URL(fileURLWithPath: o.rootfs)) == history)
}

@Test func firstDiskCanCloneAfterConfirmedPrebootRefusal() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    var o = agentOptions()
    o.base = dir.appendingPathComponent("base.ext4").path
    o.rootfs = dir.appendingPathComponent("seat.ext4").path
    o.requireRetainedDisk = false
    let base = Data("synthetic first boot base".utf8)
    try base.write(to: URL(fileURLWithPath: o.base))
    try AgentEnvironment.ensureRootfs(o)
    #expect(try Data(contentsOf: URL(fileURLWithPath: o.rootfs)) == base)
    // Once used, losing the disk must still refuse even though the base exists.
    o.requireRetainedDisk = true
    try FileManager.default.removeItem(atPath: o.rootfs)
    #expect(throws: (any Error).self) { try AgentEnvironment.ensureRootfs(o) }
    #expect(!FileManager.default.fileExists(atPath: o.rootfs))
}
