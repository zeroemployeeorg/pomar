// This file imports neither Foundation nor Containerization. The Command Line
// Tools ship Swift Testing without its Foundation cross-import overlay, so a
// test file importing both fails to compile there.
import Testing

@testable import PomarHostCore

@Test func guestPlatformIsLinuxArm64() {
    #expect(HostInfo.describe(HostInfo.guestPlatform) == "linux/arm64")
}

@Test func kernelDescribesArm64Guest() {
    let k = HostInfo.kernel(atPath: "/tmp/vmlinux")
    #expect(HostInfo.describe(k.platform) == "linux/arm64")
    #expect(k.path.lastPathComponent == "vmlinux")
}

@Test func helperCapsDefaultAndParse() {
    let d = Helper.caps(cpus: nil, memoryBytes: nil)
    #expect(d?.cpus == 2)
    #expect(d?.memoryBytes == 1024 * 1024 * 1024)
    let c = Helper.caps(cpus: "4", memoryBytes: "2147483648")
    #expect(c?.cpus == 4)
    #expect(c?.memoryBytes == 2_147_483_648)
}

@Test func helperCapsRejectBadValues() {
    #expect(Helper.caps(cpus: "0", memoryBytes: nil) == nil)
    #expect(Helper.caps(cpus: "two", memoryBytes: nil) == nil)
    #expect(Helper.caps(cpus: nil, memoryBytes: "1048576") == nil)
    #expect(Helper.caps(cpus: nil, memoryBytes: "-1") == nil)
}

@Test func shimWaitsThenExecsTheCommandInTheWorkDirectory() {
    let args = Helper.shim(["make", "verify"])
    #expect(Array(args.prefix(2)) == ["/bin/sh", "-c"])
    #expect(args[2].contains("while [ ! -e /pomar/job/.pomar-ready ]"))
    #expect(args[2].hasSuffix("cd /work || exit 125; exec \"$@\""))
    #expect(Array(args.suffix(3)) == ["pomar-shim", "make", "verify"])
}

@Test func extractReleasesTheShimOnlyAfterUnpacking() {
    let script = Helper.extractCommand()[2]
    #expect(script.contains("tar -xf /pomar/source -C /work && rm -f /pomar/source && { getent passwd 1000 >/dev/null || echo 'pomar:x:1000:1000:pomar:/pomar/job:/bin/sh' >> /etc/passwd; } && { getent group 1000 >/dev/null || echo 'pomar:x:1000:' >> /etc/group; } && mkdir -p /pomar/job && chown -R 1000:1000 /work /pomar/job && { [ ! -d /pomar/inputs ] || chown -R 1000:1000 /pomar/inputs; } && touch /pomar/job/.pomar-ready"))
}

@Test func extractWaitsForTheProxyShimWhenAsked() {
    #expect(!Helper.extractCommand()[2].contains("shim.ready"))
    let script = Helper.extractCommand(waitForProxy: true)[2]
    #expect(script.hasPrefix("n=0; until [ -e /pomar/shim.ready ]"))
    #expect(script.contains("exit 97"))
    #expect(script.hasSuffix("touch /pomar/job/.pomar-ready"))
}

@Test func proxyEnvironmentSetsOnlyGOPROXY() {
    #expect(Helper.proxyEnvironment == ["GOPROXY=http://127.0.0.1:7070"])
    for weakening in ["GOSUMDB", "GONOSUMDB", "GONOSUMCHECK", "GOINSECURE", "GOFLAGS", "GOPRIVATE", "GONOPROXY"] {
        #expect(!Helper.proxyEnvironment.contains { $0.hasPrefix(weakening + "=") })
    }
}

@Test func bundleSourceBecomesARepositoryWithNoRemote() {
    let sha = String(repeating: "a", count: 40)
    let script = Helper.extractCommand(bundleSHA: sha)[2]
    #expect(script.hasPrefix("git init -q /work && git -C /work fetch -q /pomar/source 'refs/heads/*:refs/remotes/origin/*'"))
    #expect(script.contains("checkout -q --detach \(sha)"))
    #expect(script.contains("rev-parse HEAD)\" = \(sha)"))
    #expect(!script.contains("remote add"))
    #expect(script.hasSuffix("rm -f /pomar/source && { getent passwd 1000 >/dev/null || echo 'pomar:x:1000:1000:pomar:/pomar/job:/bin/sh' >> /etc/passwd; } && { getent group 1000 >/dev/null || echo 'pomar:x:1000:' >> /etc/group; } && mkdir -p /pomar/job && chown -R 1000:1000 /work /pomar/job && { [ ! -d /pomar/inputs ] || chown -R 1000:1000 /pomar/inputs; } && touch /pomar/job/.pomar-ready"))
}

@Test func sourceKindParsing() {
    #expect(Helper.sourceKind(nil, sha: nil)?.bundle == false)
    #expect(Helper.sourceKind("tar", sha: nil)?.bundle == false)
    let sha = String(repeating: "0", count: 40)
    #expect(Helper.sourceKind("bundle", sha: sha)?.sha == sha)
    #expect(Helper.sourceKind("bundle", sha: nil) == nil)
    #expect(Helper.sourceKind("bundle", sha: "main") == nil)
    #expect(Helper.sourceKind("bundle", sha: String(repeating: "A", count: 40)) == nil)
    #expect(Helper.sourceKind("zip", sha: nil) == nil)
}

@Test func jobRunsUnprivilegedWithItsOwnHome() {
    #expect(Helper.jobUID == 1000)
    let env = Helper.jobEnvironment(["PATH=/usr/local/go/bin:/usr/bin", "HOME=/root", "GOPROXY=http://127.0.0.1:7070"])
    #expect(env == ["PATH=/usr/local/go/bin:/usr/bin", "GOPROXY=http://127.0.0.1:7070", "HOME=/pomar/job"])
}

@Test func extraLayersParsing() {
    #expect(Rootfs.extraLayers(nil) == [])
    #expect(Rootfs.extraLayers("/a/x.tar.xz,/b/y.tar.xz") == ["/a/x.tar.xz", "/b/y.tar.xz"])
    #expect(Rootfs.extraLayers("relative.tar.xz") == nil)
    #expect(Rootfs.extraLayers("") == nil)
}

@Test func shimWithoutOutputsIsUnchanged() {
    #expect(Helper.shim(["make"], outputs: [], maxBytes: 64) == Helper.shim(["make"]))
}

@Test func shimWithOutputsRunsTheCommandAsAChildThenStagesChecked() {
    let args = Helper.shim(["make", "report"], outputs: ["report.json", "log.txt"], maxBytes: 67_108_864)
    let script = args[2]
    #expect(Array(args.suffix(3)) == ["pomar-shim", "make", "report"])
    #expect(!script.contains("exec \"$@\""))
    // The command's status is kept, other job processes are killed before
    // anything is checked, and the shim exits with the command's status.
    #expect(script.contains("cd /work || exit 125; \"$@\"; rc=$?; kill -9 -1 2>/dev/null; "))
    #expect(script.contains("S=/pomar/outputs/.pomar-stage; rm -rf \"$S\" && mkdir -m 700 \"$S\" || exit $rc; left=67108864; "))
    #expect(script.contains("for n in report.json log.txt; do f=/pomar/outputs/$n; "))
    #expect(script.contains("if [ -L \"$f\" ] || [ ! -f \"$f\" ]; then"))
    #expect(script.contains("if [ \"$s\" -gt \"$left\" ]; then"))
    #expect(script.hasSuffix("cp \"$f\" \"$S/$n\" && left=$((left - s)); done; exit $rc"))
}

@Test func extractMakesTheOutputsDirectoryOnlyWhenAsked() {
    #expect(!Helper.extractCommand()[2].contains("/pomar/outputs"))
    let script = Helper.extractCommand(outputs: true)[2]
    #expect(script.hasSuffix("&& mkdir -p /pomar/outputs && chown 1000:1000 /pomar/outputs && touch /pomar/job/.pomar-ready"))
}

@Test func outputsFlagsParsing() {
    let none = Helper.outputsFlags(nil, dir: nil, max: nil)
    #expect(none?.names == [] && none?.dir == nil)
    let ok = Helper.outputsFlags("report.json,log_1.txt", dir: "/r/outputs", max: "67108864")
    #expect(ok?.names == ["report.json", "log_1.txt"] && ok?.dir == "/r/outputs" && ok?.max == 67_108_864)
    for bad in [
        "a;b", "a b", "../x", "a/b", ".hidden", "-x", "a,a", "a,", "", "$(x)", "é",
        String(repeating: "a", count: 129),
    ] {
        #expect(Helper.outputsFlags(bad, dir: "/r", max: "1") == nil, "accepted \(bad)")
    }
    #expect(Helper.outputsFlags("a", dir: nil, max: "1") == nil)
    #expect(Helper.outputsFlags("a", dir: "/r", max: nil) == nil)
    #expect(Helper.outputsFlags("a", dir: "/r", max: "0") == nil)
    #expect(Helper.outputsFlags(nil, dir: "/r", max: nil) == nil)
    let many = (0..<17).map { "f\($0)" }.joined(separator: ",")
    #expect(Helper.outputsFlags(many, dir: "/r", max: "1") == nil)
}
