// Like HostInfoTests.swift, this file imports neither Foundation nor Containerization.
import Testing

@testable import PomarHostCore

// The file key (approach A) must round-trip on any Mac, as any user: written at
// mode 0600, reloaded, and verifying against the original public key. The Secure
// Enclave and keychain lines depend on the machine and the account, and are what
// the B2 run measures, so they are only checked for being reported.
@Test func keyProbeFileKeyRoundTrips() {
    let r = KeyProbe.run(dir: ".build/key-probe-test")
    #expect(r.lines.contains("file_key_mode=600"))
    #expect(r.lines.contains("file_key_reload_verify=true"))
    #expect(r.lines.contains { $0.hasPrefix("se_available=") })
    #expect(r.lines.contains { $0.hasPrefix("keychain_add=") })
    #expect(r.lines.last?.hasPrefix("probe_ok=") == true)
}
