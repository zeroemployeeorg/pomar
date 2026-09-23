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
