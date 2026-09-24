import Containerization
import ContainerizationOCI
import Foundation

/// Base root filesystems and per-attempt APFS clones.
///
/// A base is unpacked once per image (keyed by its arm64 manifest digest) and
/// never written again. Each attempt gets a `clonefile` copy of it: creating
/// the copy costs no data blocks, and the attempt pays only for the blocks it
/// writes. The copy lives in the attempt's container directory, so deleting
/// the container deletes it.
public enum Rootfs {
    /// Unpacks `imageRef` from the store at `store` into an ext4 file at `out`.
    public static func buildBase(
        store: String, imageRef: String, imageDigest: String, out: String, sizeInBytes: UInt64
    ) async throws -> (arm64Manifest: String, ms: Int) {
        let t0 = Date()
        let images = try ImageStore(path: URL(fileURLWithPath: store))
        let image = try await images.get(reference: imageRef, pull: true)
        guard image.digest == imageDigest else {
            throw CocoaError(.coderInvalidValue, userInfo: [NSDebugDescriptionErrorKey: "image digest \(image.digest) != pinned \(imageDigest)"])
        }
        let platform = Platform(arch: "arm64", os: "linux", variant: "v8")
        let manifest = try await image.descriptor(for: platform)
        _ = try await EXT4Unpacker(capacityInBytes: sizeInBytes).unpack(image, for: platform, at: URL(fileURLWithPath: out))
        return (manifest.digest, Int(Date().timeIntervalSince(t0) * 1000))
    }

    /// Clones `base` to `dst` with APFS clonefile. `dst` must not exist.
    public static func clone(base: String, to dst: String) throws -> Int {
        let t0 = Date()
        try FileManager.default.createDirectory(
            atPath: (dst as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
        guard clonefile(base, dst, 0) == 0 else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
        // The base is read-only; the clone must be writable by the guest.
        chmod(dst, 0o600)
        return Int(Date().timeIntervalSince(t0) * 1000)
    }

    /// Free bytes on the volume holding `path`, for before/after comparisons.
    public static func freeBytes(_ path: String) -> Int64 {
        var s = statfs()
        guard statfs(path, &s) == 0 else { return -1 }
        return Int64(s.f_bavail) * Int64(s.f_bsize)
    }

    /// The mount for a cloned root filesystem.
    public static func mount(_ path: String) -> Containerization.Mount {
        .block(format: "ext4", source: path, destination: "/", options: [])
    }
}
