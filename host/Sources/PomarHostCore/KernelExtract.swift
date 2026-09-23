import ContainerizationArchive
import Foundation

/// Extracts one member of a (possibly zstd-compressed) tar archive.
public enum KernelExtract {
    public static func run(archive: String, member: String, out: String) throws -> Int {
        let reader = try ArchiveReader(file: URL(fileURLWithPath: archive))
        // Archives name members with or without a leading "./".
        var lastError: Error?
        for candidate in [member, "./" + member] {
            do {
                let (_, data) = try reader.extractFile(path: candidate)
                try data.write(to: URL(fileURLWithPath: out), options: .withoutOverwriting)
                return data.count
            } catch {
                lastError = error
            }
        }
        throw lastError ?? CocoaError(.fileNoSuchFile)
    }
}
