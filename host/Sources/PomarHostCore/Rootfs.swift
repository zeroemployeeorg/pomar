import Containerization
import ContainerizationArchive
import ContainerizationEXT4
import ContainerizationOCI
import Foundation
import SystemPackage

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
        store: String, imageRef: String, imageDigest: String, out: String, sizeInBytes: UInt64,
        extraLayers: [String] = []
    ) async throws -> (arm64Manifest: String, ms: Int) {
        let t0 = Date()
        let images = try ImageStore(path: URL(fileURLWithPath: store))
        let image = try await images.get(reference: imageRef, pull: true)
        guard image.digest == imageDigest else {
            throw CocoaError(.coderInvalidValue, userInfo: [NSDebugDescriptionErrorKey: "image digest \(image.digest) != pinned \(imageDigest)"])
        }
        let platform = Platform(arch: "arm64", os: "linux", variant: "v8")
        let descriptor = try await image.descriptor(for: platform)
        if extraLayers.isEmpty {
            _ = try await EXT4Unpacker(capacityInBytes: sizeInBytes).unpack(image, for: platform, at: URL(fileURLWithPath: out))
            return (descriptor.digest, Int(Date().timeIntervalSince(t0) * 1000))
        }
        // With extra layers (a class's pinned packages, as xz-compressed
        // data archives), the image's layers and then the extra ones go onto
        // one filesystem, in order, the way the unpacker lays down a layer.
        let manifest = try await image.manifest(for: platform)
        let fs = try EXT4.Formatter(FilePath(out), minDiskSize: sizeInBytes)
        defer { try? fs.close() }
        for layer in manifest.layers {
            let content = try await image.getContent(digest: layer.digest)
            try await fs.unpack(source: content.path, format: .paxRestricted, compression: try layerFilter(layer.mediaType))
        }
        for path in extraLayers {
            try await fs.unpack(source: URL(fileURLWithPath: path), format: .paxRestricted, compression: .xz)
        }
        return (descriptor.digest, Int(Date().timeIntervalSince(t0) * 1000))
    }

    /// The compression of an image layer, by media type.
    static func layerFilter(_ mediaType: String) throws -> ContainerizationArchive.Filter {
        switch mediaType {
        case MediaTypes.imageLayer, MediaTypes.dockerImageLayer:
            return .none
        case MediaTypes.imageLayerGzip, MediaTypes.dockerImageLayerGzip:
            return .gzip
        case MediaTypes.imageLayerZstd, MediaTypes.dockerImageLayerZstd:
            return .zstd
        default:
            throw CocoaError(.coderInvalidValue, userInfo: [NSDebugDescriptionErrorKey: "unsupported layer media type \(mediaType)"])
        }
    }

    /// Parses --extra-layers: comma-separated absolute paths, or nil.
    public static func extraLayers(_ flag: String?) -> [String]? {
        guard let flag else { return [] }
        let paths = flag.split(separator: ",").map(String.init)
        guard !paths.isEmpty, paths.allSatisfy({ $0.hasPrefix("/") }) else { return nil }
        return paths
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
