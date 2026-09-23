import Foundation
import PomarHostCore

func fail(_ message: String, code: Int32 = 1) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(code)
}

/// Parses "--name value" pairs; returns nil on a malformed list.
func flags(_ args: ArraySlice<String>) -> [String: String]? {
    var out: [String: String] = [:]
    var it = args.makeIterator()
    while let k = it.next() {
        guard k.hasPrefix("--"), let v = it.next() else { return nil }
        out[String(k.dropFirst(2))] = v
    }
    return out
}

let usage = """
    usage: pomar-host version
           pomar-host probe-vmnet host|shared
           pomar-host extract-kernel --archive A --member M --out O
           pomar-host boot-smoke --store S --kernel K --init REF --init-digest D \\
                                 --image REF --image-digest D --id ID
    """

let args = Array(CommandLine.arguments.dropFirst())
switch args.first {
case "version":
    print("pomar-host \(HostInfo.version) containerization \(HostInfo.containerizationVersion)")
case "probe-vmnet" where args.count == 2 && (args[1] == "host" || args[1] == "shared"):
    let r = VmnetProbe.run(hostOnly: args[1] == "host")
    print(r.detail)
    exit(r.ok ? 0 : 1)
case "extract-kernel":
    guard let f = flags(args.dropFirst()), let a = f["archive"], let m = f["member"], let o = f["out"] else {
        fail(usage, code: 2)
    }
    do {
        print("extracted_bytes=\(try KernelExtract.run(archive: a, member: m, out: o))")
    } catch {
        fail("extract-kernel: \(error)")
    }
case "boot-smoke":
    guard let f = flags(args.dropFirst()), let s = f["store"], let k = f["kernel"],
        let ir = f["init"], let id = f["init-digest"], let mr = f["image"], let md = f["image-digest"],
        let cid = f["id"]
    else {
        fail(usage, code: 2)
    }
    let opts = BootSmoke.Options(
        store: s, kernel: k, initRef: ir, initDigest: id, imageRef: mr, imageDigest: md, id: cid,
        command: [
            "/bin/sh", "-c",
            "uname -a; echo '--- /sys/class/net'; ls /sys/class/net; echo '--- /proc/net/dev'; cat /proc/net/dev",
        ])
    // Top-level await. Blocking the main thread on a semaphore here would
    // deadlock: top-level code runs on the main actor, and so would the task.
    do {
        let r = try await BootSmoke.run(opts)
        r.lines.forEach { print($0) }
        exit(r.exitCode == 0 ? 0 : 1)
    } catch {
        fail("boot-smoke: \(error)")
    }
default:
    fail(usage, code: 2)
}
