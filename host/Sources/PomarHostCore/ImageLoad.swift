import Containerization
import ContainerizationOCI
import Foundation

/// Loads an OCI image layout into the image store: the path for an image no
/// registry holds. The layout has already been staged and checked by
/// `pomar image load` (every blob by hash, the platform, the diff IDs, no
/// credentials); this loads that copy and then checks that the image the
/// store now holds has, as its linux/arm64 manifest, the pinned digest.
///
/// The store keys a loaded image by an index it builds around the manifest,
/// so the image's own digest is not the pin: the arm64 manifest is, as it is
/// for the bases (Rootfs.buildBase).
public enum ImageLoad {
    public static func load(store: String, layout: String, manifest: String) async throws -> (
        reference: String, index: String
    ) {
        let images = try ImageStore(path: URL(fileURLWithPath: store))
        let loaded = try await images.load(from: URL(fileURLWithPath: layout))
        guard loaded.count == 1, let image = loaded.first else {
            for i in loaded { try? await images.delete(reference: i.reference, performCleanup: true) }
            throw CocoaError(.coderInvalidValue, userInfo: [NSDebugDescriptionErrorKey: "the layout loaded \(loaded.count) images, not one"])
        }
        let platform = Platform(arch: "arm64", os: "linux", variant: "v8")
        let got = try await image.descriptor(for: platform).digest
        guard got == manifest else {
            try? await images.delete(reference: image.reference, performCleanup: true)
            throw CocoaError(.coderInvalidValue, userInfo: [NSDebugDescriptionErrorKey: "the loaded image's arm64 manifest \(got) != pinned \(manifest); removed from the store"])
        }
        return (image.reference, image.digest)
    }
}
