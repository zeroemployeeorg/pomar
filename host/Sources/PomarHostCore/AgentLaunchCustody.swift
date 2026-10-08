import CryptoKit
import Darwin
import Foundation

/// Pre-virtualization attribution, never a stop or deletion fence. No provider
/// files, request bodies or authentication state are read into this receipt.
enum AgentLaunchCustody {
    struct Record: Encodable {
        let schema = "pomar.agent-launch-custody/v1"
        let environment: String
        let session: String
        let incarnation: String
        let sourceSHA: String
        let observedAt: String
        let observerUID: UInt32
        let helperPID: Int32
        let helperBirthSeconds: UInt64
        let helperBirthMicroseconds: UInt64
        let helperExecutable: String
        let helperFileSHA256: String
        let helperFileDevice: UInt32
        let helperFileInode: UInt64
        let rootfsDevice: UInt32
        let rootfsInode: UInt64
        let rootfsBytes: Int64
        let stage = "before-virtualization-create"
    }
    enum Failure: Error { case custody, identity, executable, persistence }

    static func write(directory: String, rootfs: String, environment: String,
                      session: String, incarnation: String, sourceSHA: String) throws {
        var parent = stat()
        guard lstat(directory, &parent) == 0, parent.st_mode & S_IFMT == S_IFDIR,
              parent.st_uid == getuid(), parent.st_mode & 0o077 == 0,
              rootfs == directory + "/workspace.ext4",
              !incarnation.isEmpty, incarnation.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") })
        else { throw Failure.custody }
        let fd = open(rootfs, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { throw Failure.custody }
        defer { close(fd) }
        var disk = stat(), current = stat()
        guard fstat(fd, &disk) == 0, disk.st_mode & S_IFMT == S_IFREG,
              disk.st_uid == getuid(), disk.st_mode & 0o777 == 0o600,
              disk.st_nlink == 1, lstat(rootfs, &current) == 0,
              disk.st_dev == current.st_dev, disk.st_ino == current.st_ino
        else { throw Failure.custody }
        var identity = proc_bsdinfo()
        guard proc_pidinfo(getpid(), PROC_PIDTBSDINFO, 0, &identity, Int32(MemoryLayout<proc_bsdinfo>.size)) == MemoryLayout<proc_bsdinfo>.size,
              identity.pbi_pid == UInt32(getpid()), identity.pbi_uid == getuid(), identity.pbi_start_tvsec > 0
        else { throw Failure.identity }
        var path = [CChar](repeating: 0, count: 4096)
        guard proc_pidpath(getpid(), &path, UInt32(path.count)) > 0 else { throw Failure.executable }
        let executable = String(cString: path)
        var exe = stat()
        guard lstat(executable, &exe) == 0, exe.st_mode & S_IFMT == S_IFREG,
              exe.st_size > 0, exe.st_size <= 256 * 1024 * 1024 else { throw Failure.executable }
        let exeFD = open(executable, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard exeFD >= 0 else { throw Failure.executable }
        let exeHandle = FileHandle(fileDescriptor: exeFD, closeOnDealloc: true)
        defer { try? exeHandle.close() }
        var openedExe = stat(), afterExe = stat()
        guard fstat(exeFD, &openedExe) == 0, openedExe.st_dev == exe.st_dev, openedExe.st_ino == exe.st_ino,
              openedExe.st_uid == getuid() || openedExe.st_uid == 0,
              openedExe.st_mode & 0o022 == 0, openedExe.st_nlink == 1 else { throw Failure.executable }
        var hasher = SHA256()
        var executableBytes = 0
        while let chunk = try exeHandle.read(upToCount: 65536), !chunk.isEmpty {
            executableBytes += chunk.count
            guard executableBytes <= openedExe.st_size else { throw Failure.executable }
            hasher.update(data: chunk)
        }
        guard executableBytes == openedExe.st_size, fstat(exeFD, &afterExe) == 0,
              afterExe.st_dev == openedExe.st_dev, afterExe.st_ino == openedExe.st_ino,
              afterExe.st_size == openedExe.st_size,
              afterExe.st_mtimespec.tv_sec == openedExe.st_mtimespec.tv_sec,
              afterExe.st_mtimespec.tv_nsec == openedExe.st_mtimespec.tv_nsec else { throw Failure.executable }
        let digest = hasher.finalize().map { String(format: "%02x", $0) }.joined()
        let record = Record(environment: environment, session: session, incarnation: incarnation,
            sourceSHA: sourceSHA, observedAt: ISO8601DateFormatter().string(from: Date()), observerUID: getuid(),
            helperPID: getpid(), helperBirthSeconds: identity.pbi_start_tvsec,
            helperBirthMicroseconds: identity.pbi_start_tvusec, helperExecutable: executable, helperFileSHA256: digest,
            helperFileDevice: UInt32(bitPattern: openedExe.st_dev), helperFileInode: openedExe.st_ino,
            rootfsDevice: UInt32(bitPattern: disk.st_dev), rootfsInode: disk.st_ino, rootfsBytes: disk.st_size)
        let data = try JSONEncoder().encode(record)
        let receipt = directory + "/launch-custody-" + incarnation + ".json"
        let output = open(receipt, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600)
        guard output >= 0 else { throw Failure.persistence }
        defer { close(output) }
        // An incomplete receipt is retained on failure; no VM create follows it.
        try data.withUnsafeBytes { buffer in
            var offset = 0
            while offset < data.count {
                let n = Darwin.write(output, buffer.baseAddress!.advanced(by: offset), data.count - offset)
                if n < 0 && errno == EINTR { continue }
                guard n > 0 else { throw Failure.persistence }
                offset += n
            }
        }
        guard fsync(output) == 0, lstat(rootfs, &current) == 0,
              current.st_dev == disk.st_dev, current.st_ino == disk.st_ino,
              current.st_uid == disk.st_uid, current.st_mode & 0o777 == 0o600 else { throw Failure.persistence }
        let directoryFD = open(directory, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard directoryFD >= 0 else { throw Failure.persistence }
        defer { close(directoryFD) }
        guard fsync(directoryFD) == 0 else { throw Failure.persistence }
    }
}
