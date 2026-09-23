import Containerization
import Foundation

/// One end-to-end boot: a guest with no network interface (vsock only), a
/// pinned init filesystem and a pinned base image, one command, teardown.
public enum BootSmoke {
    public struct Options: Sendable {
        public var store: String
        public var kernel: String
        public var initRef: String
        public var initDigest: String
        public var imageRef: String
        public var imageDigest: String
        public var id: String
        public var command: [String]

        public init(
            store: String, kernel: String, initRef: String, initDigest: String,
            imageRef: String, imageDigest: String, id: String, command: [String]
        ) {
            self.store = store
            self.kernel = kernel
            self.initRef = initRef
            self.initDigest = initDigest
            self.imageRef = imageRef
            self.imageDigest = imageDigest
            self.id = id
            self.command = command
        }
    }

    /// Collects guest output and notes when the first byte arrived.
    final class Collector: Writer, @unchecked Sendable {
        private let lock = NSLock()
        private var buffer = Data()
        private(set) var firstByte: Date?

        func write(_ data: Data) throws {
            lock.lock()
            defer { lock.unlock() }
            if firstByte == nil, !data.isEmpty { firstByte = Date() }
            buffer.append(data)
        }

        func close() throws {}

        var text: String {
            lock.lock()
            defer { lock.unlock() }
            return String(decoding: buffer, as: UTF8.self)
        }
    }

    public struct Report: Sendable {
        public var lines: [String] = []
        public var exitCode: Int32 = -1
        public mutating func add(_ key: String, _ value: String) { lines.append("\(key)=\(value)") }
    }

    static func ms(_ from: Date, _ to: Date) -> String {
        String(Int((to.timeIntervalSince(from) * 1000).rounded()))
    }

    public static func run(_ o: Options) async throws -> Report {
        var report = Report()
        let kernel = HostInfo.kernel(atPath: o.kernel)
        let t0 = Date()
        // network: nil, so the manager holds no network object at all.
        var manager = try await ContainerManager(
            kernel: kernel,
            initfsReference: o.initRef,
            root: URL(fileURLWithPath: o.store),
            network: nil
        )
        let initImage = try await manager.imageStore.get(reference: o.initRef)
        report.add("init_digest", initImage.digest)
        guard initImage.digest == o.initDigest else {
            throw CocoaError(.coderInvalidValue, userInfo: [NSDebugDescriptionErrorKey: "init digest \(initImage.digest) != pinned \(o.initDigest)"])
        }
        let image = try await manager.imageStore.get(reference: o.imageRef, pull: true)
        report.add("image_digest", image.digest)
        guard image.digest == o.imageDigest else {
            throw CocoaError(.coderInvalidValue, userInfo: [NSDebugDescriptionErrorKey: "image digest \(image.digest) != pinned \(o.imageDigest)"])
        }
        let arm64 = try await image.descriptor(for: .init(arch: "arm64", os: "linux", variant: "v8"))
        report.add("image_arm64_manifest", arm64.digest)
        let initArm64 = try await initImage.descriptor(for: .init(arch: "arm64", os: "linux"))
        report.add("init_arm64_manifest", initArm64.digest)
        let tPulled = Date()
        report.add("pull_ms", ms(t0, tPulled))

        let out = Collector()
        let err = Collector()
        let container = try await manager.create(
            o.id,
            image: image,
            rootfsSizeInBytes: 2 * 1024 * 1024 * 1024,
            networking: false
        ) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.process.arguments = o.command
            config.process.stdout = out
            config.process.stderr = err
            config.interfaces = []
        }
        defer { try? manager.delete(o.id) }
        let tCreated = Date()
        report.add("rootfs_ms", ms(tPulled, tCreated))
        report.add("interfaces_configured", String(container.interfaces.count))

        try await container.create()
        try await container.start()
        let status = try await container.wait(timeoutInSeconds: 120)
        let tDone = Date()
        try? await container.stop()
        report.exitCode = status.exitCode
        report.add("exit_code", String(status.exitCode))
        if let fb = out.firstByte {
            report.add("boot_to_first_output_ms", ms(tCreated, fb))
        }
        report.add("run_ms", ms(tCreated, tDone))
        report.add("total_ms", ms(t0, tDone))
        for line in out.text.split(separator: "\n", omittingEmptySubsequences: false) {
            report.lines.append("guest_stdout: \(line)")
        }
        for line in err.text.split(separator: "\n") {
            report.lines.append("guest_stderr: \(line)")
        }
        return report
    }
}
