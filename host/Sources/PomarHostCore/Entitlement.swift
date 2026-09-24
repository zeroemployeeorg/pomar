import Foundation
import Security

/// The running process's own entitlements. VM paths check this first, so an
/// unentitled binary fails with a named reason instead of deep inside VZ.
public enum Entitlement {
    public static let virtualization = "com.apple.security.virtualization"

    /// Whether this process holds com.apple.security.virtualization = true.
    public static func hasVirtualization() -> Bool {
        guard let task = SecTaskCreateFromSelf(nil) else { return false }
        let value = SecTaskCopyValueForEntitlement(task, virtualization as CFString, nil)
        return (value as? Bool) == true
    }

    public static let missingReason = "missing-entitlement: \(virtualization); run `make swift-sign`"
}
