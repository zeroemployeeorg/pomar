import Containerization
import Foundation
import CryptoKit

/// Development agent VM owner. Its writable filesystem lives outside the
/// container directory and is retained on stop. CI Helper remains unchanged.
public enum AgentEnvironment {
    public struct Options: Codable, Sendable {
        public var environment: String
        public var session: String
        public var incarnation: String
        public var directory: String
        public var store: String
        public var rootfs: String
        public var base: String
        public var kernel: String
        public var initRef: String
        public var initDigest: String
        public var imageRef: String
        public var imageDigest: String
        public var guestBinary: String
        public var shimBinary: String
        public var codexArchive: String
        public var codexArchiveSHA256: String
        public var sourceBundle: String
        public var sourceSHA: String
        public var controlSocket: String
        public var egressSocket: String
        public var goProxySocket: String
        public var cpus: Int
        public var memoryBytes: UInt64
        public var controllerCapabilities: [String]? = nil
        /// The coding agent: nil or "codex" (the default), or "claude". The
        /// pinned, hash-checked archive above is that agent's package.
        public var agent: String? = nil
        /// The pinned agent version, required for "claude".
        public var agentVersion: String? = nil
        /// An owner-supplied synthetic file for a qualification profile only
        /// (agentenv QualificationFixture), placed once before the agent starts.
        public var qualificationFixture: QualificationFixture? = nil
    }

    /// The owner's synthetic qualification file: its source inside the host
    /// root's fixtures directory, its exact digest and size, and the one
    /// compiled guest destination.
    public struct QualificationFixture: Codable, Sendable, Equatable {
        public var source: String
        public var sha256: String
        public var size: Int
        public var destination: String
    }

    /// The only guest path a fixture may take: a Claude Code qualification
    /// environment's credential file (agentenv ClaudeQualificationDestination).
    public static let claudeQualificationDestination = "/pomar/job/.claude/.credentials.json"

    /// A fixture belongs to a Claude Code environment, at the compiled
    /// destination, with a sha256 and an exact size of at most 64 KiB.
    public static func validFixture(_ o: Options) -> Bool {
        guard let f = o.qualificationFixture else { return true }
        let hex = f.sha256.count == 64 && f.sha256.allSatisfy { ("0"..."9").contains($0) || ("a"..."f").contains($0) }
        return o.agent == "claude" && f.destination == claudeQualificationDestination && hex
            && f.size > 0 && f.size <= 64 * 1024 && f.source.hasPrefix("/") && f.source.split(separator: "/").allSatisfy { $0 != ".." && $0 != "." }
    }

    public enum FixtureError: Error, Equatable { case custody(String), digest, replaced }

    /// Reads the fixture's bytes with the owner's custody checked on the opened
    /// file itself: the source and its parent are the owner's, private and not
    /// symlinks; the file is regular, 0600, one link, of the exact size; the
    /// bytes read have the exact digest; and afterwards the path still names
    /// the same file and directory. A replacement is refused, never followed.
    /// afterRead is a test seam between the read and the recheck.
    public static func readFixture(_ f: QualificationFixture, afterRead: () -> Void = {}) throws -> Data {
        let parent = (f.source as NSString).deletingLastPathComponent
        var dirBefore = stat()
        guard lstat(parent, &dirBefore) == 0, (dirBefore.st_mode & S_IFMT) == S_IFDIR, dirBefore.st_uid == getuid(),
              dirBefore.st_mode & 0o077 == 0 else { throw FixtureError.custody("the fixtures directory") }
        let fd = open(f.source, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { throw FixtureError.custody("the source cannot be opened without following a link") }
        defer { close(fd) }
        var opened = stat()
        guard fstat(fd, &opened) == 0, (opened.st_mode & S_IFMT) == S_IFREG, opened.st_uid == getuid(),
              opened.st_mode & 0o777 == 0o600, opened.st_nlink == 1, Int(opened.st_size) == f.size
        else { throw FixtureError.custody("the source must be the owner's one regular 0600 file of the exact size") }
        var data = Data(count: f.size)
        let n = data.withUnsafeMutableBytes { read(fd, $0.baseAddress, f.size) }
        guard n == f.size else { throw FixtureError.custody("the source's size changed") }
        let digest = SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
        guard digest == f.sha256 else { throw FixtureError.digest }
        afterRead()
        var fileAfter = stat(), dirAfter = stat()
        guard lstat(f.source, &fileAfter) == 0, fileAfter.st_dev == opened.st_dev, fileAfter.st_ino == opened.st_ino,
              lstat(parent, &dirAfter) == 0, dirAfter.st_dev == dirBefore.st_dev, dirAfter.st_ino == dirBefore.st_ino
        else { throw FixtureError.replaced }
        return data
    }

    /// The agent an environment runs; anything else is refused.
    public static func validAgent(_ o: Options) -> Bool {
        switch o.agent ?? "codex" {
        case "codex":
            return o.agentVersion == nil
        case "claude":
            // A dotted numeric version only. Controller capabilities pass
            // through: the guest broker enables them only for an
            // owner-selected Claude Code executable pinned as qualified
            // (agentenv qualifiedController), and refuses to start otherwise.
            guard let v = o.agentVersion, !v.isEmpty, v.count <= 32,
                  v.allSatisfy({ $0.isASCII && ($0.isNumber || $0 == ".") }), !v.hasPrefix("."), !v.hasSuffix(".") else { return false }
            return true
        default:
            return false
        }
    }

    public static func valid(_ o: Options) -> Bool {
        let id: (String) -> Bool = { s in
            !s.isEmpty && s.count <= 128 && s.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }
        }
        let sha: (String, Int) -> Bool = { s, n in s.count == n && s.allSatisfy { ("0"..."9").contains($0) || ("a"..."f").contains($0) } }
        return id(o.environment) && id(o.session) && id(o.incarnation)
            && validControllerCapabilities(o.controllerCapabilities ?? []) && validAgent(o) && validFixture(o)
            && sha(o.sourceSHA, 40) && sha(o.codexArchiveSHA256, 64)
            && o.cpus > 0 && o.cpus <= 4 && o.memoryBytes >= 512 * 1024 * 1024 && o.memoryBytes <= 8 * 1024 * 1024 * 1024
            && o.rootfs == o.directory + "/workspace.ext4"
            && o.controlSocket == o.directory + "/agent.sock"
            && o.egressSocket == o.directory + "/egress.sock"
    }

    public static func validControllerCapabilities(_ names: [String]) -> Bool {
        names.count <= 16 && Set(names).count == names.count && names.allSatisfy { name in
            !name.isEmpty && name.utf8.count <= 64 && name.first!.isASCII && ("a"..."z").contains(String(name.first!))
                && name.allSatisfy { $0.isASCII && (("a"..."z").contains(String($0)) || ("0"..."9").contains(String($0)) || $0 == "_") }
        }
    }

    static func status(_ o: Options, _ phase: String, stopped: Bool = false, error: String = "") {
        let values: [String: String] = ["environment": o.environment, "session": o.session, "incarnation": o.incarnation,
            "phase": phase, "vm_stopped": stopped ? "true" : "false", "error": error, "pid": String(getpid())]
        guard let data = try? JSONSerialization.data(withJSONObject: values, options: [.sortedKeys]) else { return }
        // Never replace a predecessor's legacy uncertain receipt.
        try? data.write(to: URL(fileURLWithPath: o.directory + "/vm-status-" + o.incarnation + ".json"), options: [.atomic])
    }

    /// Source is installed once. Restarts retain Git changes, Codex home and
    /// the root-owned broker journal. No host directory is mounted.
    public static func setupCommand(sourceSHA: String, agent: String = "codex", fixtureSHA256: String? = nil) -> [String] {
        // Codex: /opt/pomar-codex and CODEX_HOME. Claude Code: /opt/pomar-claude
        // and CLAUDE_CONFIG_DIR, and a stale bridge socket from a previous
        // boot removed (the broker refuses to replace one).
        let claude = agent == "claude"
        let path = claude ? "/opt/pomar-claude/bin" : "/opt/pomar-codex/codex-path"
        // A qualification fixture is placed once, on the environment's first
        // setup: its copied bytes' digest is checked again in the guest, and
        // it never replaces an existing file at its destination.
        var fixture = ""
        if claude, let sha = fixtureSHA256 {
            let dest = claudeQualificationDestination
            fixture = "; if [ ! -e /var/lib/pomar-agent/fixture-placed ]; then "
                + "test ! -e " + dest + " || { echo 'qualification fixture refused: its destination exists'; exit 1; }; "
                + "echo '" + sha + "  /pomar/qualification-fixture' | sha256sum -c - >/dev/null || { echo 'qualification fixture refused: digest'; exit 1; }; "
                + "install -o 1000 -g 1000 -m 0600 /pomar/qualification-fixture " + dest + "; touch /var/lib/pomar-agent/fixture-placed; fi; "
                + "rm -f /pomar/qualification-fixture"
        }
        let install = claude
            ? "mkdir -p /opt/pomar-claude /pomar/job/.claude; chmod 700 /pomar/job/.claude; rm -f /run/pomar-claude/bridge.sock; "
                + "tar -xzf /pomar/codex-package.tar.gz -C /opt/pomar-claude; "
                + "chmod -R go-w /opt/pomar-claude; chown -R 1000:1000 /pomar/job" + fixture
            : "mkdir -p /opt/pomar-codex /pomar/job/.codex; chmod 700 /pomar/job/.codex; "
                + "tar -xzf /pomar/codex-package.tar.gz -C /opt/pomar-codex; "
                + "chmod -R go-w /opt/pomar-codex; chown -R 1000:1000 /pomar/job"
        let script =
            "set -eu; " + Helper.registerJobUser + "; "
            + "mkdir -p /pomar/job /var/lib/pomar-agent /run/pomar; "
            + "mkdir -p /etc/profile.d; printf '%s\\n' 'export PATH=" + path + ":/pomar/job/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin' > /etc/profile.d/pomar-agent.sh; chmod 644 /etc/profile.d/pomar-agent.sh; "
            + "chmod 700 /var/lib/pomar-agent /run/pomar; rm -f /run/pomar/agent.sock; "
            + "if [ ! -e /var/lib/pomar-agent/workspace-created ]; then "
            + "git init -q /work; git -C /work fetch -q --tags /pomar/source.bundle HEAD; "
            + "git -C /work -c advice.detachedHead=false checkout -q --detach " + sourceSHA + "; "
            + "test \"$(git -C /work rev-parse HEAD)\" = " + sourceSHA + "; "
            + "chown -R 1000:1000 /work /pomar/job; touch /var/lib/pomar-agent/workspace-created; fi; "
            + install
        return ["/bin/sh", "-c", script]
    }

    /// The broker's arguments: the Codex default, or Claude Code's actor.
    public static func brokerArguments(_ o: Options) -> [String] {
        var args = ["/pomar/agent-guest", "-environment", o.environment, "-session", o.session, "-incarnation", o.incarnation, "-source-sha", o.sourceSHA,
                    "-controller-capabilities", (o.controllerCapabilities ?? []).joined(separator: ",")]
        if o.agent == "claude" {
            args += ["-actor", "claude", "-claude", "/opt/pomar-claude/bin/claude", "-claude-version", o.agentVersion ?? ""]
        }
        return args
    }

    public static func run(_ o: Options) async -> Int32 {
        guard valid(o), Entitlement.hasVirtualization() else { return 2 }
        // Unix/vsock relay peers disappear on ordinary controller disconnect.
        // Let writers report EPIPE; the default SIGPIPE action would kill this
        // VM owner and every guest task before a stop receipt could be filed.
        signal(SIGPIPE, SIG_IGN)
        let stop = Helper.StopFlag()
        signal(SIGTERM, SIG_IGN)
        let sigterm = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .global())
        sigterm.setEventHandler { stop.set() }; sigterm.resume()
        status(o, "booting")
        var manager: ContainerManager
        var ownedContainer: LinuxContainer?
        var stage = "verify-artifacts"
        do {
            let archive = try Data(contentsOf: URL(fileURLWithPath: o.codexArchive))
            let digest = SHA256.hash(data: archive).map { String(format: "%02x", $0) }.joined()
            guard digest == o.codexArchiveSHA256 else { throw CocoaError(.fileReadCorruptFile) }
            manager = try await ContainerManager(kernel: HostInfo.kernel(atPath: o.kernel), initfsReference: o.initRef,
                root: URL(fileURLWithPath: o.store), network: nil)
            let initImage = try await manager.imageStore.get(reference: o.initRef)
            let image = try await manager.imageStore.get(reference: o.imageRef)
            guard initImage.digest == o.initDigest, image.digest == o.imageDigest else { throw CocoaError(.fileReadCorruptFile) }
            if !FileManager.default.fileExists(atPath: o.rootfs) { _ = try Rootfs.clone(base: o.base, to: o.rootfs) }
            stage = "record-launch-custody"
            try AgentLaunchCustody.write(directory: o.directory, rootfs: o.rootfs,
                environment: o.environment, session: o.session, incarnation: o.incarnation, sourceSHA: o.sourceSHA)
            // The external rootfs does not create the container's boot-log
            // directory, unlike the CI clone located within that directory.
            stage = "prepare-container"
            try FileManager.default.createDirectory(atPath: o.store + "/containers/agent-" + o.environment + "-" + o.incarnation,
                withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            let container = try await manager.create("agent-" + o.environment + "-" + o.incarnation,
                image: image, rootfs: Rootfs.mount(o.rootfs), networking: false) { config in
                    config.cpus = o.cpus; config.memoryInBytes = o.memoryBytes
                    config.process.arguments = ["/bin/sh", "-c", "while :; do sleep 3600; done"]
                    config.process.stdout = try Helper.FileWriter(path: o.directory + "/vm.log")
                    config.process.stderr = config.process.stdout
                    config.interfaces = []; config.dns = Helper.noResolver
                    config.sockets = [
                        UnixSocketConfiguration(source: URL(fileURLWithPath: "/run/pomar/agent.sock"),
                            destination: URL(fileURLWithPath: o.controlSocket), permissions: .init(rawValue: 0o600), direction: .outOf),
                        UnixSocketConfiguration(source: URL(fileURLWithPath: o.egressSocket),
                            destination: URL(fileURLWithPath: "/run/pomar/egress.sock"), permissions: .init(rawValue: 0o600), direction: .into),
                        UnixSocketConfiguration(source: URL(fileURLWithPath: o.goProxySocket),
                            destination: URL(fileURLWithPath: Helper.proxySocket), permissions: .init(rawValue: 0o600), direction: .into),
                    ]
                }
            ownedContainer = container
            stage = "create-vm"
            try await container.create(); try await container.start()
        } catch {
            let failure = "\(stage): \(error)"
            if let container = ownedContainer {
                do {
                    try await container.stop()
                    status(o, "boot-failed-stopped", stopped: true, error: failure)
                } catch {
                    status(o, "boot-uncertain", error: failure + "; stop: \(error)")
                }
            } else {
                // No create/start was attempted. This is a positive
                // non-execution receipt, not an inferred process absence.
                status(o, "boot-not-created", stopped: true, error: failure)
            }
            return 1
        }
        guard let container = ownedContainer else { return 1 }
        do {
            let out = try Helper.FileWriter(path: o.directory + "/vm.log")
            try await container.copyIn(from: URL(fileURLWithPath: o.guestBinary), to: URL(fileURLWithPath: "/pomar/agent-guest"), mode: 0o755)
            try await container.copyIn(from: URL(fileURLWithPath: o.codexArchive), to: URL(fileURLWithPath: "/pomar/codex-package.tar.gz"), mode: 0o600)
            try await container.copyIn(from: URL(fileURLWithPath: o.sourceBundle), to: URL(fileURLWithPath: "/pomar/source.bundle"), mode: 0o600)
            if let f = o.qualificationFixture {
                // The bytes copied are the bytes checked: read with custody
                // pinned, staged privately, then copied in and checked again.
                let data = try readFixture(f)
                let staged = o.directory + "/qualification-fixture-" + o.incarnation
                let fd = open(staged, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600)
                guard fd >= 0 else { throw FixtureError.custody("the staged copy") }
                let written = data.withUnsafeBytes { write(fd, $0.baseAddress, data.count) }
                close(fd)
                defer { unlink(staged) }
                guard written == data.count else { throw FixtureError.custody("the staged copy") }
                try await container.copyIn(from: URL(fileURLWithPath: staged), to: URL(fileURLWithPath: "/pomar/qualification-fixture"), mode: 0o600)
            }
            let setup = try await container.exec("agent-setup") { config in
                config.arguments = setupCommand(sourceSHA: o.sourceSHA, agent: o.agent ?? "codex", fixtureSHA256: o.qualificationFixture?.sha256); config.stdout = out; config.stderr = out
            }
            try await setup.start(); let result = try await setup.wait(); try await setup.delete()
            guard result.exitCode == 0 else { throw CocoaError(.fileReadCorruptFile) }
            _ = try await Helper.startProxyShim(container, shim: o.shimBinary, output: out, name: "agent-egress-shim",
                listen: "127.0.0.1:7072", socket: "/run/pomar/egress.sock", ready: "/pomar/egress.ready")
            _ = try await Helper.startProxyShim(container, shim: o.shimBinary, output: out, name: "agent-go-shim",
                listen: Helper.proxyListen, socket: Helper.proxySocket, ready: "/pomar/go.ready", copy: false)
            let broker = try await container.exec("agent-broker") { config in
                config.arguments = brokerArguments(o)
                config.stdout = out; config.stderr = out
            }
            try await broker.start()
            status(o, "running")
            while !stop.isSet && !FileManager.default.fileExists(atPath: o.directory + "/stop") {
                try await Task.sleep(for: .milliseconds(250))
            }
            // Stop confirmation, not reporting revocation, is the execution fence.
            status(o, "stopping")
            try await container.stop()
            status(o, "stopped", stopped: true)
            try manager.delete("agent-" + o.environment + "-" + o.incarnation)
            return 0
        } catch {
            let failure = "\(error)"
            do {
                try await container.stop()
                status(o, "failed-stopped", stopped: true, error: failure)
            } catch {
                // Preserve the disk and uncertainty when stop itself fails.
                status(o, "stop-uncertain", error: failure + "; stop: \(error)")
            }
            return 1
        }
    }
}
