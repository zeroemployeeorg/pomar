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
