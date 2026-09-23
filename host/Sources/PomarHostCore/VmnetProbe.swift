import Containerization
import vmnet

/// Creates a vmnet network in the given mode, reports what the framework
/// returned, and releases it when the value goes out of scope. Establishes,
/// under this binary's own signature, which vmnet modes are usable.
public enum VmnetProbe {
    public static func run(hostOnly: Bool) -> (ok: Bool, detail: String) {
        let mode: vmnet.operating_modes_t = hostOnly ? .VMNET_HOST_MODE : .VMNET_SHARED_MODE
        let name = hostOnly ? "host" : "shared"
        do {
            let net = try VmnetNetwork(mode: mode)
            return (true, "mode=\(name) created subnet=\(net.subnet) gateway=\(net.ipv4Gateway)")
        } catch {
            return (false, "mode=\(name) failed: \(error)")
        }
    }
}
