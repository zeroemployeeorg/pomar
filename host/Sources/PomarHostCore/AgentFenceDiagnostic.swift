import Darwin
import Foundation

/// A one-shot privileged metadata observation. It has no process-control API,
/// reads no file contents and cannot be imported as a retirement fence.
public enum AgentFenceDiagnostic {
    public struct Request: Codable {
        public let target: AgentFenceProbe.Request
        public let expectedDevice: UInt32
        public let expectedInode: UInt64
    }
    struct Process: Encodable {
        let pid: Int32
        let category: String
    }
    public struct Evidence: Encodable {
        let schema = "pomar.target-holder-diagnostic/v1"
        let authority = "diagnostic-only-not-a-retirement-fence"
        let environment: String
        let session: String
        let incarnation: String
        let operation: String
        let observerUID: UInt32
        let targetUID: UInt32
        let targetDevice: UInt32
        let targetInode: UInt64
        let observedAt: String
        let processCount: Int
        let vnodeDescriptorsChecked: Int
        let fileportsChecked: Int
        let regionsChecked: Int
        let resourceCoverageComplete: Bool
        let observations: [Process]
    }
    public static func readRequest(_ path: String) throws -> Request {
        guard getuid() == 0, geteuid() == 0 else { throw NSError(domain: "AgentFenceDiagnostic.rootObserverRequired", code: 1) }
        let fd = open(path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { throw NSError(domain: "AgentFenceDiagnostic.configCustody", code: 1) }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        defer { try? handle.close() }
        var before = stat(), after = stat()
        guard fstat(fd, &before) == 0, before.st_mode & S_IFMT == S_IFREG,
              before.st_uid == 0, before.st_mode & 0o777 == 0o600,
              before.st_nlink == 1, before.st_size > 0, before.st_size <= 65536 else {
            throw NSError(domain: "AgentFenceDiagnostic.configCustody", code: 1)
        }
        let data = try handle.read(upToCount: 65537) ?? Data()
        guard data.count == before.st_size, fstat(fd, &after) == 0,
              before.st_size == after.st_size, before.st_mtimespec.tv_sec == after.st_mtimespec.tv_sec,
              before.st_mtimespec.tv_nsec == after.st_mtimespec.tv_nsec else {
            throw NSError(domain: "AgentFenceDiagnostic.configChanged", code: 1)
        }
        return try JSONDecoder().decode(Request.self, from: data)
    }
    public static func run(_ request: Request) throws -> Evidence {
        guard getuid() == 0, geteuid() == 0, request.target.workspaceAbsent != true,
              request.target.uid != 0, request.expectedInode != 0 else {
            throw NSError(domain: "AgentFenceDiagnostic.rootObserverRequired", code: 1)
        }
        var before = stat()
        guard lstat(request.target.rootfs, &before) == 0,
              before.st_mode & S_IFMT == S_IFREG, before.st_uid == request.target.uid,
              UInt32(bitPattern: before.st_dev) == request.expectedDevice,
              before.st_ino == request.expectedInode else {
            throw NSError(domain: "AgentFenceDiagnostic.targetBindingChanged", code: 1)
        }
        let raw = try AgentFenceProbe.run(request.target, access: .init(), privilegedObserver: true)
        var observations: [Process] = []
        // Return only bounded identity and target/coverage categories. Paths,
        // argv, environment, vnode filenames and mapped bytes never leave here.
        for issue in raw.issues.prefix(4096) {
            let pieces = issue.split(separator: " ")
            let pid = pieces.count > 1 && pieces[0] == "pid" ? Int32(pieces[1].trimmingCharacters(in: CharacterSet(charactersIn: ":"))) : nil
            let category: String
            if issue.contains("workspace inode") { category = "target-holder" }
            else if issue.contains("original helper binding") || issue.contains("retained helper executable") { category = "related-helper-live" }
            else if issue.contains("Virtualization executor") { category = "virtualization-ownership-unresolved" }
            else { category = "coverage-unknown" }
            // A PID is an observation locator, not authority to signal it.
            // Never attach a later process birth to an earlier resource scan.
            observations.append(Process(pid: pid ?? 0, category: category))
        }
        var after = stat()
        guard lstat(request.target.rootfs, &after) == 0,
              after.st_dev == before.st_dev, after.st_ino == before.st_ino,
              after.st_uid == before.st_uid else {
            throw NSError(domain: "AgentFenceDiagnostic.targetBindingChanged", code: 1)
        }
        return Evidence(environment: request.target.environment, session: request.target.session,
            incarnation: request.target.incarnation, operation: request.target.operation,
            observerUID: getuid(), targetUID: request.target.uid, targetDevice: request.expectedDevice,
            targetInode: request.expectedInode, observedAt: raw.observedAt,
            processCount: raw.processCount, vnodeDescriptorsChecked: raw.vnodeDescriptorsChecked,
            fileportsChecked: raw.fileportsChecked, regionsChecked: raw.regionsChecked,
            resourceCoverageComplete: !observations.contains { $0.category == "coverage-unknown" } && raw.issues.count <= 4096,
            observations: observations)
    }
}
