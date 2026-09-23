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

        public init(
            attempt: String, stateDir: String, store: String, kernel: String,
            initRef: String, initDigest: String, imageRef: String, imageDigest: String,
            command: [String]
        ) {
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
        let source = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .global())
        source.setEventHandler { stop.set() }
        source.resume()

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
        do {
            let initImage = try await manager.imageStore.get(reference: o.initRef)
            let image = try await manager.imageStore.get(reference: o.imageRef, pull: true)
            guard initImage.digest == o.initDigest, image.digest == o.imageDigest else {
                writeStatus(o.stateDir, ["phase": "failed", "attempt": o.attempt, "error": "digest mismatch"])
                return 1
            }
            let out = try FileWriter(path: o.stateDir + "/output.log")
            container = try await manager.create(
                o.attempt, image: image, rootfsSizeInBytes: 2 * 1024 * 1024 * 1024, networking: false
            ) { config in
                config.cpus = 2
                config.memoryInBytes = 1024 * 1024 * 1024
                config.process.arguments = o.command
                config.process.stdout = out
                config.process.stderr = out
                config.interfaces = []
            }
            try await container.create()
            try await container.start()
        } catch {
            writeStatus(o.stateDir, ["phase": "failed", "attempt": o.attempt, "error": "\(error)"])
            try? manager.delete(o.attempt)
            return 1
        }
        writeStatus(o.stateDir, ["phase": "running", "attempt": o.attempt])

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
        try? await container.stop()
        try? manager.delete(o.attempt)
        if stopped {
            writeStatus(o.stateDir, ["phase": "stopped", "attempt": o.attempt])
            return 143
        }
        writeStatus(o.stateDir, ["phase": "exited", "attempt": o.attempt, "exit_code": String(exitCode)])
        return exitCode == 0 ? 0 : 1
    }
}
