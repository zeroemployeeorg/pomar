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
           pomar-host helper --attempt ID --state-dir DIR --store S --kernel K \\
                             --init REF --init-digest D --image REF --image-digest D [--base ROOTFS] [--source FILE [--source-kind tar|bundle --source-sha SHA] [--inputs DIR]] \\
                             [--goproxy-socket SOCK --shim BIN] [--outputs NAME,NAME... --outputs-dir DIR --outputs-max BYTES] [--readonly-source yes] [--job-user yes] \\
                             [--cpus N] [--memory-bytes N] -- CMD...
           pomar-host build-base --store S --image REF --image-digest D --out ROOTFS --size-bytes N \\
                                 [--extra-layers PATH,PATH...]
           pomar-host key-probe --dir DIR
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
case "helper":
    let rest = args.dropFirst()
    guard let sep = rest.firstIndex(of: "--"), sep < rest.endIndex - 1,
        let f = flags(rest[rest.startIndex..<sep]),
        let a = f["attempt"], let dir = f["state-dir"], let s = f["store"], let k = f["kernel"],
        let ir = f["init"], let idg = f["init-digest"], let mr = f["image"], let md = f["image-digest"]
    else {
        fail(usage, code: 2)
    }
    guard let caps = Helper.caps(cpus: f["cpus"], memoryBytes: f["memory-bytes"]) else {
        fail("helper: --cpus must be a positive integer and --memory-bytes at least 268435456", code: 2)
    }
    guard let kind = Helper.sourceKind(f["source-kind"], sha: f["source-sha"]) else {
        fail("helper: --source-kind is tar or bundle, and a bundle needs --source-sha with a full SHA", code: 2)
    }
    guard let readonlySource = Helper.yesFlag(f["readonly-source"]), let jobUser = Helper.yesFlag(f["job-user"]),
        !readonlySource || f["source"] != nil
    else {
        fail("helper: --readonly-source and --job-user take yes; --readonly-source needs --source", code: 2)
    }
    guard let outs = Helper.outputsFlags(f["outputs"], dir: f["outputs-dir"], max: f["outputs-max"]) else {
        fail("helper: --outputs, --outputs-dir and --outputs-max go together; names are letters, digits, dot, dash and underscore", code: 2)
    }
    let command = Array(rest[(sep + 1)...])
    let code = await Helper.run(
        .init(
            attempt: a, stateDir: dir, store: s, kernel: k, initRef: ir, initDigest: idg,
            imageRef: mr, imageDigest: md, command: command, base: f["base"], source: f["source"],
            proxySocket: f["goproxy-socket"], shim: f["shim"], sourceBundleSHA: kind.sha, inputs: f["inputs"],
            outputs: outs.names, outputsDir: outs.dir, outputsMax: outs.max,
            readonlySource: readonlySource, jobUser: jobUser,
            cpus: caps.cpus, memoryBytes: caps.memoryBytes))
    exit(code)
case "build-base":
    guard let f = flags(args.dropFirst()), let s = f["store"], let mr = f["image"], let md = f["image-digest"],
        let o = f["out"], let sz = f["size-bytes"].flatMap({ UInt64($0) }),
        let extra = Rootfs.extraLayers(f["extra-layers"])
    else {
        fail(usage, code: 2)
    }
    do {
        let r = try await Rootfs.buildBase(
            store: s, imageRef: mr, imageDigest: md, out: o, sizeInBytes: sz, extraLayers: extra)
        print("extra_layers=\(extra.count)")
        print("arm64_manifest=\(r.arm64Manifest)")
        print("unpack_ms=\(r.ms)")
    } catch {
        fail("build-base: \(error)")
    }
case "key-probe":
    guard let f = flags(args.dropFirst()), let d = f["dir"] else {
        fail(usage, code: 2)
    }
    let r = KeyProbe.run(dir: d)
    r.lines.forEach { print($0) }
    exit(r.ok ? 0 : 1)
default:
    fail(usage, code: 2)
}
