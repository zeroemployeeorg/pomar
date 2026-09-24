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
