import Darwin
import Foundation
import Testing
@testable import PomarHostCore

private func bootCase() throws -> String {
    let directory = NSTemporaryDirectory() + "pomar-boot-" + UUID().uuidString
    try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    return directory
}

@Test func bootLogSurvivesDeletionWithPrivatePermissions() throws {
    let dir = try bootCase()
    defer { try? FileManager.default.removeItem(atPath: dir) }
    let data = Data("synthetic boot diagnostic".utf8)
    try data.write(to: URL(fileURLWithPath: dir + "/input"))
    let result = BootDiagnostics.preserve(source: dir + "/input", destination: dir + "/output")
    try FileManager.default.removeItem(atPath: dir + "/input")
    #expect(result["boot_log_preserved"] == "true")
    #expect(result["boot_log_truncated"] == "false")
    #expect(try Data(contentsOf: URL(fileURLWithPath: dir + "/output")) == data)
    let permissions = try FileManager.default.attributesOfItem(atPath: dir + "/output")[.posixPermissions] as? NSNumber
    #expect(permissions?.intValue == 0o600)
}

@Test func bootLogIsBoundedAndRetainsTheTail() throws {
    let dir = try bootCase()
    defer { try? FileManager.default.removeItem(atPath: dir) }
    let data = Data(repeating: 65, count: BootDiagnostics.limit) + Data("tail".utf8)
    try data.write(to: URL(fileURLWithPath: dir + "/input"))
    let result = BootDiagnostics.preserve(source: dir + "/input", destination: dir + "/output")
    #expect(result["boot_log_offset"] == "4")
    #expect(result["boot_log_truncated"] == "true")
    #expect(try Data(contentsOf: URL(fileURLWithPath: dir + "/output")) == data.suffix(BootDiagnostics.limit))
}

@Test func bootLogRefusesSymlinksAndExistingOutput() throws {
    let dir = try bootCase()
    defer { try? FileManager.default.removeItem(atPath: dir) }
    try Data("original".utf8).write(to: URL(fileURLWithPath: dir + "/original"))
    #expect(symlink(dir + "/original", dir + "/link") == 0)
    #expect(BootDiagnostics.preserve(source: dir + "/link", destination: dir + "/new")["boot_log_preserved"] == "false")
    #expect(BootDiagnostics.preserve(source: dir + "/original", destination: dir + "/link")["boot_log_preserved"] == "false")
    #expect(BootDiagnostics.preserve(source: dir + "/original", destination: dir + "/original")["boot_log_preserved"] == "false")
    #expect(try String(contentsOfFile: dir + "/original", encoding: .utf8) == "original")
}

@Test func bootLogRefusesSpecialFilesWithoutBlocking() throws {
    let dir = try bootCase()
    defer { try? FileManager.default.removeItem(atPath: dir) }
    #expect(mkfifo(dir + "/fifo", 0o600) == 0)
    #expect(BootDiagnostics.preserve(source: dir + "/fifo", destination: dir + "/output")["boot_log_preserved"] == "false")
    #expect(!FileManager.default.fileExists(atPath: dir + "/output"))
}
