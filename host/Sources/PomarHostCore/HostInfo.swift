import Containerization
import Foundation

/// Static facts about this host build. Kept small until the host owns VMs.
public enum HostInfo {
    public static let version = "0.0.0-dev"
    public static let containerizationVersion = "0.45.0"

    /// Guests are ARM64 Linux only.
    public static let guestPlatform: SystemPlatform = .linuxArm

    /// Describes a kernel at `path` for an ARM64 guest.
    public static func kernel(atPath path: String) -> Kernel {
        Kernel(path: URL(fileURLWithPath: path), platform: guestPlatform)
    }

    /// "os/architecture", for logs and records.
    public static func describe(_ p: SystemPlatform) -> String {
        "\(p.os.rawValue)/\(p.architecture.rawValue)"
    }
}
