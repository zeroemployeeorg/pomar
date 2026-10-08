import CryptoKit
import Foundation
import Testing
@testable import PomarHostCore

/// A private fixtures directory with one synthetic fixture in it.
func fixtureCase() throws -> (dir: String, fixture: AgentEnvironment.QualificationFixture) {
    let root = NSTemporaryDirectory() + "pomar-fixture-" + UUID().uuidString
    let dir = root + "/fixtures"
    try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    chmod(root, 0o700)
    let content = Data(#"{"claudeAiOauth":{"accessToken":"pomar-canary-test"}}"#.utf8)
    let source = dir + "/claude-canary.json"
    #expect(FileManager.default.createFile(atPath: source, contents: content, attributes: [.posixPermissions: 0o600]))
    let digest = SHA256.hash(data: content).map { String(format: "%02x", $0) }.joined()
    return (dir, .init(source: source, sha256: digest, size: content.count, destination: AgentEnvironment.claudeQualificationDestination))
}

@Test func fixtureReadAcceptsOwnerCustody() throws {
    let c = try fixtureCase()
    let data = try AgentEnvironment.readFixture(c.fixture)
    #expect(data.count == c.fixture.size)
}

@Test func fixtureReadRefusesBrokenCustody() throws {
    let cases: [(String, (String, inout AgentEnvironment.QualificationFixture) -> Void)] = [
        ("a wrong digest", { _, f in f.sha256 = String(repeating: "0", count: 64) }),
        ("a wrong size", { _, f in f.size += 1 }),
        ("a readable file", { _, f in chmod(f.source, 0o644) }),
        ("a hard link", { dir, f in link(f.source, dir + "/second-link.json") }),
        ("a symlinked file", { dir, f in
            rename(f.source, dir + "/real.json"); symlink(dir + "/real.json", f.source) }),
        ("an open directory", { dir, _ in chmod(dir, 0o755) }),
        ("a symlinked directory", { dir, f in
            let moved = dir + "-moved"
            rename(dir, moved); symlink(moved, dir) }),
    ]
    for (name, mutate) in cases {
        var c = try fixtureCase()
        mutate(c.dir, &c.fixture)
        #expect(throws: AgentEnvironment.FixtureError.self, "\(name)") { try AgentEnvironment.readFixture(c.fixture) }
    }
}

@Test func fixtureReadRefusesReplacementAfterTheRead() throws {
    let c = try fixtureCase()
    let replace = {
        let data = FileManager.default.contents(atPath: c.fixture.source)!
        FileManager.default.createFile(atPath: c.fixture.source + ".new", contents: data, attributes: [.posixPermissions: 0o600])
        rename(c.fixture.source + ".new", c.fixture.source) // the same bytes, another file
    }
    #expect(throws: AgentEnvironment.FixtureError.replaced) { try AgentEnvironment.readFixture(c.fixture, afterRead: replace) }
}

@Test func fixtureOptionsAreClaudeOnlyAtTheCompiledDestination() throws {
    let c = try fixtureCase()
    var o = agentOptions(); o.agent = "claude"; o.agentVersion = "2.1.280"; o.qualificationFixture = c.fixture
    #expect(AgentEnvironment.valid(o))
    var codex = agentOptions(); codex.qualificationFixture = c.fixture
    #expect(!AgentEnvironment.valid(codex))
    for change in [{ (f: inout AgentEnvironment.QualificationFixture) in f.destination = "/etc/passwd" },
                   { f in f.sha256 = "short" }, { f in f.size = 0 }, { f in f.size = 64 * 1024 + 1 },
                   { f in f.source = "relative.json" }, { f in f.source = "/a/../b.json" }] {
        var bad = o
        change(&bad.qualificationFixture!)
        #expect(!AgentEnvironment.valid(bad))
    }
}

@Test func fixtureSetupPlacesOnceWithoutOverwriting() {
    let sha = String(repeating: "c", count: 64)
    let script = AgentEnvironment.setupCommand(sourceSHA: String(repeating: "b", count: 40), agent: "claude", fixtureSHA256: sha)[2]
    #expect(script.contains("if [ ! -e /var/lib/pomar-agent/fixture-placed ]; then test ! -e /pomar/job/.claude/.credentials.json || "))
    #expect(script.contains("echo '" + sha + "  /pomar/qualification-fixture' | sha256sum -c -"))
    #expect(script.contains("install -o 1000 -g 1000 -m 0600 /pomar/qualification-fixture /pomar/job/.claude/.credentials.json; touch /var/lib/pomar-agent/fixture-placed"))
    #expect(script.hasSuffix("rm -f /pomar/qualification-fixture"))
    // Without a fixture, and for Codex, the setup is unchanged.
    let plain = AgentEnvironment.setupCommand(sourceSHA: String(repeating: "b", count: 40), agent: "claude")[2]
    #expect(!plain.contains("qualification-fixture"))
    let codex = AgentEnvironment.setupCommand(sourceSHA: String(repeating: "b", count: 40), agent: "codex", fixtureSHA256: sha)[2]
    #expect(!codex.contains("qualification-fixture"))
}
