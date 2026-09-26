import Containerization
import Foundation

/// The per-attempt VM-owning process. It boots one vsock-only guest, runs the
/// attempt's command, and stops and deletes the guest when the command ends or
/// when asked to stop. Its only outputs are files in the attempt's state
/// directory: status.json (phases) and output.log (guest stdout and stderr).
public enum Helper {
    public struct Options: Sendable {
        public var attempt: String
        public var stateDir: String
        public var store: String
        public var kernel: String
        public var initRef: String
        public var initDigest: String
        public var imageRef: String
        public var imageDigest: String
        public var command: [String]
        /// The guest's caps, from the attempt's job class.
        public var cpus: Int
        public var memoryBytes: UInt64
        /// The attempt's pinned source snapshot (a tar) on the host; nil runs
        /// the command with no source.
        public var source: String?
        /// The host socket of this attempt's module proxy, and the shim
        /// binary that serves it in the guest; both or neither.
        public var proxySocket: String?
        /// With a bundle source, the commit to check out; nil for a tar.
        public var sourceBundleSHA: String?
        /// A directory of input files, copied into the guest at /pomar/inputs.
        public var inputs: String?
        public var shim: String?
        /// Named outputs, copied out of the guest into outputsDir when the
        /// command exits; the shim stages at most outputsMax bytes of them.
        public var outputs: [String]
        public var outputsDir: String?
        public var outputsMax: Int64
        /// Leave /work root-owned and not writable, instead of the job's.
        public var readonlySource: Bool
        /// With no source, run the command as the job user anyway (a class
        /// that never runs a job as root).
        public var jobUser: Bool
        /// A base root filesystem to clone; nil unpacks the image as before.
        public var base: String?

        public init(
            attempt: String, stateDir: String, store: String, kernel: String,
            initRef: String, initDigest: String, imageRef: String, imageDigest: String,
            command: [String], base: String? = nil, source: String? = nil,
            proxySocket: String? = nil, shim: String? = nil, sourceBundleSHA: String? = nil, inputs: String? = nil,
            outputs: [String] = [], outputsDir: String? = nil, outputsMax: Int64 = 0,
            readonlySource: Bool = false, jobUser: Bool = false,
            cpus: Int = Helper.defaultCaps.cpus, memoryBytes: UInt64 = Helper.defaultCaps.memoryBytes
        ) {
            self.base = base
            self.source = source
            self.proxySocket = proxySocket
            self.sourceBundleSHA = sourceBundleSHA
            self.inputs = inputs
            self.shim = shim
            self.outputs = outputs
            self.outputsDir = outputsDir
            self.outputsMax = outputsMax
            self.readonlySource = readonlySource
            self.jobUser = jobUser
            self.cpus = cpus
            self.memoryBytes = memoryBytes
            self.attempt = attempt
            self.stateDir = stateDir
            self.store = store
            self.kernel = kernel
            self.initRef = initRef
            self.initDigest = initDigest
            self.imageRef = imageRef
            self.imageDigest = imageDigest
            self.command = command
        }
    }

    /// The caps a helper gets when the manager passes none.
    public static let defaultCaps = (cpus: 2, memoryBytes: UInt64(1024 * 1024 * 1024))

    /// Parses the --cpus and --memory-bytes flags. An absent flag takes the
    /// default; a present one must be a positive integer, and memory at
    /// least 256 MiB. Returns nil on a bad value.
    public static func caps(cpus: String?, memoryBytes: String?) -> (cpus: Int, memoryBytes: UInt64)? {
        var c = defaultCaps
        if let s = cpus {
            guard let n = Int(s), n > 0 else { return nil }
            c.cpus = n
        }
        if let s = memoryBytes {
            guard let n = UInt64(s), n >= 256 * 1024 * 1024 else { return nil }
            c.memoryBytes = n
        }
        return c
    }

    /// Where a source lands in the guest, and the marker that releases the
    /// command once it has.
    public static let workDir = "/work"
    static let archiveInGuest = "/pomar/source"
    // In the job's home, which the job owns, so the job can remove it.
    static let readyMarker = "/pomar/job/.pomar-ready"

    /// A job with a source runs as this unprivileged uid and gid, with its
    /// own home directory.
    public static let jobUID: UInt32 = 1000
    public static let jobHome = "/pomar/job"

    /// Gives the job's uid a passwd and group entry when the image has none,
    /// so that user lookups in the job work.
    static let registerJobUser =
        "{ getent passwd \(jobUID) >/dev/null || echo 'pomar:x:\(jobUID):\(jobUID):pomar:\(jobHome):/bin/sh' >> /etc/passwd; } "
        + "&& { getent group \(jobUID) >/dev/null || echo 'pomar:x:\(jobUID):' >> /etc/group; }"

    /// The image's environment with HOME pointed at the job's home.
    public static func jobEnvironment(_ env: [String], home: String = jobHome) -> [String] {
        env.filter { !$0.hasPrefix("HOME=") } + ["HOME=\(home)"]
    }

    /// The job's HOME when it runs as the job user with no source.
    public static let jobHomeWithoutSource = "/tmp"

    /// Parses a yes-or-absent flag (--readonly-source, --job-user): nil is
    /// false, "yes" is true, and anything else is refused (nil).
    public static func yesFlag(_ v: String?) -> Bool? {
        switch v {
        case nil: return false
        case "yes": return true
        default: return nil
        }
    }

    /// The guest's main process when the attempt has a source: it waits for
    /// the source to be in place, then runs the command in the work directory
    /// as itself (exec), so its exit is the attempt's.
    public static func shim(_ command: [String]) -> [String] {
        let script =
            "while [ ! -e \(readyMarker) ]; do sleep 0.05; done; rm -f \(readyMarker); "
            + "cd \(workDir) || exit 125; exec \"$@\""
        return ["/bin/sh", "-c", script, "pomar-shim"] + command
    }

    /// Where a job leaves its named outputs, and where the shim stages the
    /// ones that pass its checks for the helper to copy out.
    public static let outputsInGuest = "/pomar/outputs"
    public static let outputsStage = "/pomar/outputs/.pomar-stage"

    /// The guest's main process when the attempt names outputs: as shim, but
    /// the command runs as a child, not in its place. When it exits, the shim
    /// kills every other process of the job's uid, so nothing can change a
    /// file after it is checked, then stages each named output that is a
    /// regular file (not a symbolic link) within what is left of the cap, and
    /// exits with the command's status. Names are the manager's (letters,
    /// digits, dot, dash and underscore; never a path or a leading dot).
    public static func shim(_ command: [String], outputs: [String], maxBytes: Int64) -> [String] {
        if outputs.isEmpty { return shim(command) }
        let names = outputs.joined(separator: " ")
        let script =
            "while [ ! -e \(readyMarker) ]; do sleep 0.05; done; rm -f \(readyMarker); "
            + "cd \(workDir) || exit 125; \"$@\"; rc=$?; kill -9 -1 2>/dev/null; "
            + "S=\(outputsStage); rm -rf \"$S\" && mkdir -m 700 \"$S\" || exit $rc; left=\(maxBytes); "
            + "for n in \(names); do f=\(outputsInGuest)/$n; "
            + "if [ -L \"$f\" ] || [ ! -f \"$f\" ]; then echo \"pomar: output $n: missing or not a regular file\" >&2; continue; fi; "
            + "s=$(wc -c < \"$f\"); s=$((s + 0)); "
            + "if [ \"$s\" -gt \"$left\" ]; then echo \"pomar: output $n: $s bytes, over what is left of the cap\" >&2; continue; fi; "
            + "cp \"$f\" \"$S/$n\" && left=$((left - s)); done; exit $rc"
        return ["/bin/sh", "-c", script, "pomar-shim"] + command
    }

    /// Parses --outputs, --outputs-dir and --outputs-max: all three or none.
    /// The names go into the shim's script, so each is checked here as the
    /// manager checks it: 1 to 128 of letters, digits, dot, dash and
    /// underscore, starting with a letter or digit, and none repeated.
    /// Returns nil on a bad set.
    public static func outputsFlags(_ names: String?, dir: String?, max: String?) -> (
        names: [String], dir: String?, max: Int64
    )? {
        guard let names else {
            return dir == nil && max == nil ? ([], nil, 0) : nil
        }
        guard let dir, let m = max.flatMap({ Int64($0) }), m > 0 else { return nil }
        let list = names.split(separator: ",", omittingEmptySubsequences: false).map(String.init)
        let alnum = { (c: Character) in c.isASCII && (c.isLetter || c.isNumber) }
        for n in list {
            guard let first = n.first, alnum(first), n.count <= 128,
                n.allSatisfy({ alnum($0) || $0 == "." || $0 == "-" || $0 == "_" })
            else { return nil }
        }
        guard Set(list).count == list.count, list.count <= 16 else { return nil }
        return (list, dir, m)
    }

    /// Copies the staged outputs out of the guest, each on its own, into dir.
    /// A name the shim did not stage fails to copy and is left out; the
    /// manager records it as missing. Returns how many were copied.
    static func copyOutputs(_ container: LinuxContainer, names: [String], to dir: String) async -> Int {
        try? FileManager.default.createDirectory(
            atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        var copied = 0
        for n in names {
            let dst = URL(fileURLWithPath: dir).appendingPathComponent(n)
            do {
                try await container.copyOut(
                    from: URL(fileURLWithPath: outputsStage + "/" + n), to: dst, createParents: false)
                copied += 1
            } catch {
                try? FileManager.default.removeItem(at: dst)
            }
        }
        return copied
    }

    /// The guest-side step that unpacks the copied archive and releases the
    /// shim. With the module proxy, it first waits (up to 10 s) for the
    /// proxy shim to be listening, so the command never starts without it.
    public static func extractCommand(
        waitForProxy: Bool = false, bundleSHA: String? = nil, outputs: Bool = false, readonlySource: Bool = false
    ) -> [String] {
        let wait =
            waitForProxy
            ? "n=0; until [ -e \(proxyReady) ]; do n=$((n+1)); [ $n -gt 200 ] && { echo 'pomar: proxy shim not ready' >&2; exit 97; }; sleep 0.05; done; "
            : ""
        // A bundle becomes a repository with no remote: its branches arrive
        // as origin/* tracking refs, and the pinned commit is checked out
        // detached. No credential helper is configured.
        let unpack =
            bundleSHA.map {
                "git init -q \(workDir) && git -C \(workDir) fetch -q \(archiveInGuest) 'refs/heads/*:refs/remotes/origin/*' "
                    + "&& git -C \(workDir) -c advice.detachedHead=false checkout -q --detach \($0) "
                    + "&& test \"$(git -C \(workDir) rev-parse HEAD)\" = \($0)"
            } ?? "mkdir -p \(workDir) && tar -xf \(archiveInGuest) -C \(workDir)"
        return [
            "/bin/sh", "-c",
            wait + unpack + " && rm -f \(archiveInGuest) && \(registerJobUser) && mkdir -p \(jobHome) "
                + (readonlySource
                    ? "&& chown -R 0:0 \(workDir) && chmod -R a-w \(workDir) && chown -R \(jobUID):\(jobUID) \(jobHome) "
                        // Git refuses a repository its user does not own; the
                        // ownership is Pomar's choice, so this one is marked safe.
                        + (bundleSHA != nil ? "&& git config --system --add safe.directory \(workDir) " : "")
                    : "&& chown -R \(jobUID):\(jobUID) \(workDir) \(jobHome) ")
                + "&& { [ ! -d \(inputsInGuest) ] || chown -R \(jobUID):\(jobUID) \(inputsInGuest); } "
                + (outputs ? "&& mkdir -p \(outputsInGuest) && chown \(jobUID):\(jobUID) \(outputsInGuest) " : "")
                + "&& touch \(readyMarker)",
        ]
    }

    /// Parses --source-kind and --source-sha: a tar needs no SHA; a bundle
    /// needs a full lower-case hex SHA. Returns nil on a bad pair.
    public static func sourceKind(_ kind: String?, sha: String?) -> (bundle: Bool, sha: String?)? {
        switch kind ?? "tar" {
        case "tar":
            return (false, nil)
        case "bundle":
            guard let s = sha, s.count == 40, s.allSatisfy({ ("0"..."9").contains($0) || ("a"..."f").contains($0) }) else {
                return nil
            }
            return (true, s)
        default:
            return nil
        }
    }

    /// The guest end of the module proxy: the host socket is relayed to
    /// proxySocket, and the shim serves it on proxyListen.
    public static let proxySocket = "/run/pomar/goproxy.sock"
    public static let proxyListen = "127.0.0.1:7070"
    static let shimInGuest = "/pomar/shim"
    public static let inputsInGuest = "/pomar/inputs"
    static let proxyReady = "/pomar/shim.ready"

    /// The environment a command gets with the module proxy: GOPROXY only.
    /// The checksum database stays at its default, reached through the
    /// proxy; nothing that weakens checking is set.
    public static let proxyEnvironment = ["GOPROXY=http://\(proxyListen)"]

    /// Copies the shim into the guest and starts it. The returned process
    /// runs until the guest stops.
    static func startProxyShim(_ container: LinuxContainer, shim: String, output: Writer) async throws -> LinuxProcess {
        try await container.copyIn(
            from: URL(fileURLWithPath: shim), to: URL(fileURLWithPath: shimInGuest), mode: 0o755)
        let p = try await container.exec("pomar-proxy-shim") { config in
            config.arguments = [shimInGuest, "-listen", proxyListen, "-socket", proxySocket, "-ready", proxyReady]
            config.stdout = output
            config.stderr = output
        }
        try await p.start()
        return p
    }

    /// Copies the source archive into the running guest over vsock, unpacks
    /// it there, and releases the command. Returns the copy and unpack times
    /// in milliseconds.
    static func copyIn(
        _ container: LinuxContainer, source: String, output: Writer, waitForProxy: Bool = false, bundleSHA: String? = nil,
        outputs: Bool = false, readonlySource: Bool = false
    ) async throws -> (copy: Int, extract: Int) {
        let clock = ContinuousClock()
        let t0 = clock.now
        try await container.copyIn(
            from: URL(fileURLWithPath: source), to: URL(fileURLWithPath: archiveInGuest), mode: 0o600)
        let t1 = clock.now
        let p = try await container.exec("pomar-extract") { config in
            config.arguments = extractCommand(
                waitForProxy: waitForProxy, bundleSHA: bundleSHA, outputs: outputs, readonlySource: readonlySource)
            config.stdout = output
            config.stderr = output
        }
        try await p.start()
        let status = try await p.wait()
        try? await p.delete()
        guard status.exitCode == 0 else {
            throw CocoaError(.fileReadCorruptFile, userInfo: [NSLocalizedDescriptionKey: "source unpack in the guest exited \(status.exitCode)"])
        }
        let t2 = clock.now
        let ms = { (d: Duration) in Int(d.components.seconds * 1000 + d.components.attoseconds / 1_000_000_000_000_000) }
        return (ms(t1 - t0), ms(t2 - t1))
    }

    /// Appends to a file; used for the guest's stdout and stderr.
    final class FileWriter: Writer, @unchecked Sendable {
        private let handle: FileHandle
        private let lock = NSLock()
        init(path: String) throws {
            if !FileManager.default.fileExists(atPath: path) {
                FileManager.default.createFile(atPath: path, contents: nil, attributes: [.posixPermissions: 0o600])
            }
            guard let h = FileHandle(forWritingAtPath: path) else {
                throw CocoaError(.fileNoSuchFile)
            }
            h.seekToEndOfFile()
            handle = h
        }
        func write(_ data: Data) throws {
            lock.lock()
            defer { lock.unlock() }
            handle.write(data)
        }
        func close() throws {}
    }

    /// Writes status.json atomically (temp file and rename in the same directory).
    static func writeStatus(_ dir: String, _ fields: [String: String]) {
        var f = fields
        f["time"] = ISO8601DateFormatter().string(from: Date())
        f["pid"] = String(getpid())
        guard let data = try? JSONSerialization.data(withJSONObject: f, options: [.sortedKeys]) else { return }
        let tmp = URL(fileURLWithPath: dir).appendingPathComponent(".status.json.tmp")
        let dst = URL(fileURLWithPath: dir).appendingPathComponent("status.json")
        do {
            try data.write(to: tmp)
            _ = try FileManager.default.replaceItemAt(dst, withItemAt: tmp)
        } catch {
            try? data.write(to: dst)
        }
    }

    /// Set by the SIGTERM handler; read by the run loop.
    final class StopFlag: @unchecked Sendable {
        private let lock = NSLock()
        private var value = false
        func set() { lock.lock(); value = true; lock.unlock() }
        var isSet: Bool { lock.lock(); defer { lock.unlock() }; return value }
    }

    public static func run(_ o: Options) async -> Int32 {
        let stop = StopFlag()
        signal(SIGTERM, SIG_IGN)
        let sigterm = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .global())
        sigterm.setEventHandler { stop.set() }
        sigterm.resume()

        guard Entitlement.hasVirtualization() else {
            writeStatus(o.stateDir, ["phase": "failed", "attempt": o.attempt, "error": Entitlement.missingReason])
            return 1
        }
        writeStatus(o.stateDir, ["phase": "booting", "attempt": o.attempt])
        var manager: ContainerManager
        do {
            manager = try await ContainerManager(
                kernel: HostInfo.kernel(atPath: o.kernel),
                initfsReference: o.initRef,
                root: URL(fileURLWithPath: o.store),
                network: nil
            )
        } catch {
            writeStatus(o.stateDir, ["phase": "failed", "attempt": o.attempt, "error": "\(error)"])
            return 1
        }
        let container: LinuxContainer
        var metrics = ["cpus": String(o.cpus), "memory_bytes": String(o.memoryBytes)]
        var freeBefore: Int64 = -1
        do {
            let initImage = try await manager.imageStore.get(reference: o.initRef)
            let image = try await manager.imageStore.get(reference: o.imageRef, pull: true)
            guard initImage.digest == o.initDigest, image.digest == o.imageDigest else {
                writeStatus(o.stateDir, ["phase": "failed", "attempt": o.attempt, "error": "digest mismatch"])
                return 1
            }
            let out = try FileWriter(path: o.stateDir + "/output.log")
            let configure: (inout LinuxContainer.Configuration) throws -> Void = { config in
                config.cpus = o.cpus
                config.memoryInBytes = o.memoryBytes
                config.process.arguments =
                    o.source == nil ? o.command : Helper.shim(o.command, outputs: o.outputs, maxBytes: o.outputsMax)
                if o.source != nil {
                    // The job runs unprivileged in its guest: its tests may
                    // rely on permissions root would bypass.
                    config.process.user = .init(uid: Helper.jobUID, gid: Helper.jobUID)
                    config.process.environmentVariables = Helper.jobEnvironment(config.process.environmentVariables)
                } else if o.jobUser {
                    // No source step made the job's home: the job runs as its
                    // uid with HOME at /tmp, and never as root.
                    config.process.user = .init(uid: Helper.jobUID, gid: Helper.jobUID)
                    config.process.environmentVariables = Helper.jobEnvironment(
                        config.process.environmentVariables, home: Helper.jobHomeWithoutSource)
                }
                config.process.stdout = out
                config.process.stderr = out
                config.interfaces = []
                if let sock = o.proxySocket {
                    config.sockets = [
                        UnixSocketConfiguration(
                            source: URL(fileURLWithPath: sock), destination: URL(fileURLWithPath: Helper.proxySocket),
                            direction: .into)
                    ]
                    config.process.environmentVariables += Helper.proxyEnvironment
                }
            }
            if let base = o.base {
                // The clone sits in the container's own directory, so
                // deleting the container deletes the clone.
                let clonePath = o.store + "/containers/" + o.attempt + "/rootfs.ext4"
                freeBefore = Rootfs.freeBytes(o.store)
                metrics["clone_ms"] = String(try Rootfs.clone(base: base, to: clonePath))
                container = try await manager.create(
                    o.attempt, image: image, rootfs: Rootfs.mount(clonePath), networking: false,
                    configuration: configure)
            } else {
                container = try await manager.create(
                    o.attempt, image: image, rootfsSizeInBytes: 2 * 1024 * 1024 * 1024, networking: false,
                    configuration: configure)
            }
            try await container.create()
            try await container.start()
        } catch {
            // The free space at the failure, read before deleting the guest
            // frees its clone: the manager names a failure on a full host
            // host-disk-full.
            writeStatus(o.stateDir, [
                "phase": "failed", "attempt": o.attempt, "error": "\(error)",
                "host_free_bytes": String(Rootfs.freeBytes(o.store)),
            ])
            try? manager.delete(o.attempt)
            return 1
        }
        // With a source, the guest is up but its command waits in the shim
        // until the source is in place.
        // The proxy shim, when there is one, lives until the guest stops.
        var proxyShim: LinuxProcess?
        defer { _ = proxyShim }
        if let src = o.source {
            do {
                let log = try FileWriter(path: o.stateDir + "/output.log")
                let withProxy = o.proxySocket != nil && o.shim != nil
                if withProxy, let shim = o.shim {
                    proxyShim = try await startProxyShim(container, shim: shim, output: log)
                    metrics["goproxy"] = "http://" + proxyListen
                }
                if let dir = o.inputs {
                    // The client's files, as a directory; unpacked in the guest,
                    // then handed to the job user by the source step.
                    try await container.copyIn(
                        from: URL(fileURLWithPath: dir, isDirectory: true), to: URL(fileURLWithPath: inputsInGuest))
                    metrics["inputs"] = String((try? FileManager.default.contentsOfDirectory(atPath: dir).count) ?? 0)
                }
                let attrs = try FileManager.default.attributesOfItem(atPath: src)
                metrics["source_bytes"] = String((attrs[.size] as? NSNumber)?.int64Value ?? -1)
                let t = try await copyIn(
                    container, source: src, output: log, waitForProxy: withProxy, bundleSHA: o.sourceBundleSHA,
                    outputs: !o.outputs.isEmpty, readonlySource: o.readonlySource)
                if o.sourceBundleSHA != nil { metrics["source_kind"] = "bundle" }
                metrics["copy_in_ms"] = String(t.copy)
                metrics["extract_ms"] = String(t.extract)
            } catch {
                writeStatus(o.stateDir, [
                    "phase": "failed", "attempt": o.attempt, "error": "source copy-in: \(error)",
                    "host_free_bytes": String(Rootfs.freeBytes(o.store)),
                ].merging(metrics) { a, _ in a })
                try? await container.stop()
                try? manager.delete(o.attempt)
                return 1
            }
        }
        writeStatus(o.stateDir, ["phase": "running", "attempt": o.attempt].merging(metrics) { a, _ in a })

        // Wait for the command, checking the stop flag once a second.
        var exitCode: Int32 = -1
        var stopped = false
        while true {
            if stop.isSet {
                stopped = true
                break
            }
            if let status = try? await container.wait(timeoutInSeconds: 1) {
                exitCode = status.exitCode
                break
            }
        }
        // The command exited, and with it the shim's staging: copy the staged
        // outputs out while the guest is still up. A stopped attempt has none.
        if !stopped, !o.outputs.isEmpty, let dir = o.outputsDir {
            metrics["outputs_copied"] = String(await copyOutputs(container, names: o.outputs, to: dir))
        }
        try? await container.stop()
        // Read before deleting the guest, which frees its clone's blocks.
        let freeAtEnd = Rootfs.freeBytes(o.store)
        metrics["host_free_bytes"] = String(freeAtEnd)
        if freeBefore >= 0 {
            // Approximate: other writers on the volume show up here too.
            metrics["bytes_written_approx"] = String(freeBefore - freeAtEnd)
        }
        try? manager.delete(o.attempt)
        if stopped {
            writeStatus(o.stateDir, ["phase": "stopped", "attempt": o.attempt].merging(metrics) { a, _ in a })
            return 143
        }
        writeStatus(o.stateDir, ["phase": "exited", "attempt": o.attempt, "exit_code": String(exitCode)].merging(metrics) { a, _ in a })
        return exitCode == 0 ? 0 : 1
    }
}
