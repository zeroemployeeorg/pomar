import Darwin
import Foundation

/// Keep a bounded tail of the helper's own boot log before container deletion.
/// Neither a symlink nor a special file may become a diagnostic input/output.
enum BootDiagnostics {
    static let limit = 1024 * 1024

    static func preserve(source: String, destination: String) -> [String: String] {
        let input = open(source, O_RDONLY | O_NOFOLLOW | O_NONBLOCK)
        guard input >= 0 else { return ["boot_log_preserved": "false"] }
        defer { close(input) }
        var info = stat()
        guard fstat(input, &info) == 0, info.st_mode & S_IFMT == S_IFREG, info.st_size >= 0 else {
            return ["boot_log_preserved": "false"]
        }
        let offset = max(0, info.st_size - Int64(limit))
        guard lseek(input, offset, SEEK_SET) == offset else { return ["boot_log_preserved": "false"] }
        let output = open(destination, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0o600)
        guard output >= 0 else { return ["boot_log_preserved": "false"] }
        defer { close(output) }
        var buffer = [UInt8](repeating: 0, count: 16 * 1024)
        var total = 0
        while total < limit {
            let count = buffer.withUnsafeMutableBytes { read(input, $0.baseAddress!, min($0.count, limit - total)) }
            if count == 0 { break }
            if count < 0 {
                if errno == EINTR { continue }
                return ["boot_log_preserved": "partial", "boot_log_bytes": String(total)]
            }
            var written = 0
            while written < count {
                let n = buffer.withUnsafeBytes { write(output, $0.baseAddress!.advanced(by: written), count - written) }
                if n < 0 && errno == EINTR { continue }
                guard n > 0 else { return ["boot_log_preserved": "partial", "boot_log_bytes": String(total + written)] }
                written += n
            }
            total += count
        }
        return [
            "boot_log_preserved": "true", "boot_log_bytes": String(total),
            "boot_log_offset": String(offset), "boot_log_truncated": String(offset > 0 || info.st_size > Int64(total)),
        ]
    }
}
