import Darwin
import Foundation
import Testing
@testable import PomarHostCore

// Real owned metadata/resource reads, with a single failure injected at the
// native boundary. No process is stopped, unlinked executable trusted or
// another account made accessible to manufacture a passing fence.
private func withFenceIdentityFixture(_ body: (AgentFenceProbe.Request, proc_bsdinfo, URL) throws -> Void) throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("pomar-fence-identity-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }
    let disk = dir.appendingPathComponent("workspace.ext4")
    try Data("retained writer fixture".utf8).write(to: disk)
    chmod(disk.path, 0o600)
    let handle = try FileHandle(forReadingFrom: disk)
    defer { try? handle.close() }
    var identity = proc_bsdinfo()
    #expect(proc_pidinfo(getpid(), PROC_PIDTBSDINFO, 0, &identity, Int32(MemoryLayout<proc_bsdinfo>.size)) == MemoryLayout<proc_bsdinfo>.size)
    let request = AgentFenceProbe.Request(environment: "fixture", session: "session",
        incarnation: "actor", operation: "probe", rootfs: disk.path,
        helper: "/unavailable/retained/helper", pid: getpid(), uid: getuid(), birth: identity.pbi_start_tvsec)
    try body(request, identity, dir)
    #expect(try Data(contentsOf: disk) == Data("retained writer fixture".utf8))
}

private func missingOwnExecutable() -> AgentFenceProbe.ProcessAccess {
    var access = AgentFenceProbe.ProcessAccess()
    let native = access.executable
    access.executable = { pid in
        if pid == getpid() { errno = ENOENT; return nil }
        return native(pid)
    }
    return access
}

@Test func missingExecutableStillFindsBoundHelperAndWorkspaceHandle() throws {
    try withFenceIdentityFixture { request, _, _ in
        let evidence = try AgentFenceProbe.run(request, access: missingOwnExecutable())
        #expect(!evidence.confirmed)
        #expect(evidence.issues.contains { $0.contains("pid \(getpid()): executable path unavailable errno 2") })
        #expect(evidence.issues.contains { $0.contains("original helper binding still live at unavailable executable path") })
        #expect(evidence.issues.contains { $0.contains("pid \(getpid()) fd") && $0.contains("workspace inode remains open") })
        #expect(evidence.ownProcessesChecked > 0)
        #expect(evidence.vnodeDescriptorsChecked > 0)
    }
}

@Test func reusedPIDWithAnotherBirthCannotMatchOriginalHelper() throws {
    try withFenceIdentityFixture { request, _, _ in
        let anotherBirth = AgentFenceProbe.Request(environment: request.environment, session: request.session,
            incarnation: request.incarnation, operation: request.operation, rootfs: request.rootfs,
            helper: request.helper, pid: request.pid, uid: request.uid, birth: request.birth + 1)
        let evidence = try AgentFenceProbe.run(anotherBirth, access: missingOwnExecutable())
        #expect(!evidence.confirmed) // missing path is still unresolved
        #expect(!evidence.issues.contains { $0.contains("original helper binding still live") })
        #expect(evidence.issues.contains { $0.contains("workspace inode remains open") })
    }
}

@Test func inaccessibleKernelIdentityRemainsUnknown() throws {
    try withFenceIdentityFixture { request, _, _ in
        var access = missingOwnExecutable()
        let native = access.info
        access.info = { pid, flavor, arg, buffer, size in
            if pid == getpid() && flavor == PROC_PIDTBSDINFO { errno = EACCES; return 0 }
            return native(pid, flavor, arg, buffer, size)
        }
        let evidence = try AgentFenceProbe.run(request, access: access)
        #expect(!evidence.confirmed)
        #expect(evidence.issues.contains { $0.contains("owned process birth/identity unavailable") })
        #expect(!evidence.issues.contains { $0.contains("original helper binding still live") })
    }
}

@Test func unavailableResourceCoverageIsSeparateFromMissingExecutable() throws {
    try withFenceIdentityFixture { request, _, _ in
        for (flavor, expected) in [(PROC_PIDLISTFDS, "descriptor inventory unavailable"),
                                   (PROC_PIDLISTFILEPORTS, "fileport inventory unavailable"),
                                   (PROC_PIDREGIONPATHINFO, "mapped-region lookup unavailable")] {
            var access = missingOwnExecutable()
            let native = access.info
            access.info = { pid, query, arg, buffer, size in
                if pid == getpid() && query == flavor { errno = EACCES; return 0 }
                return native(pid, query, arg, buffer, size)
            }
            let evidence = try AgentFenceProbe.run(request, access: access)
            #expect(!evidence.confirmed)
            #expect(evidence.issues.contains { $0.contains("pid \(getpid()): executable path unavailable") })
            #expect(evidence.issues.contains { $0.contains("pid \(getpid()): " + expected) })
        }
    }
}

@Test func fullDescriptorBufferCannotBecomeCompleteEmptyCoverage() throws {
    try withFenceIdentityFixture { request, _, _ in
        var access = missingOwnExecutable()
        let native = access.info
        access.info = { pid, flavor, arg, buffer, size in
            if pid == getpid() && flavor == PROC_PIDLISTFDS && buffer != nil { return size }
            return native(pid, flavor, arg, buffer, size)
        }
        let evidence = try AgentFenceProbe.run(request, access: access)
        #expect(!evidence.confirmed)
        #expect(evidence.issues.contains { $0.contains("pid \(getpid()): descriptor inventory incomplete") })
    }
}

@Test func identityChangeDuringResourceScanRetainsUnknown() throws {
    try withFenceIdentityFixture { request, _, _ in
        var access = missingOwnExecutable()
        let native = access.info
        var reads = 0
        access.info = { pid, flavor, arg, buffer, size in
            let result = native(pid, flavor, arg, buffer, size)
            if pid == getpid() && flavor == PROC_PIDTBSDINFO && result == MemoryLayout<proc_bsdinfo>.size {
                reads += 1
                if reads % 2 == 0, let info = buffer?.assumingMemoryBound(to: proc_bsdinfo.self) {
                    info.pointee.pbi_start_tvusec += 1
                }
            }
            return result
        }
        let evidence = try AgentFenceProbe.run(request, access: access)
        #expect(!evidence.confirmed)
        #expect(evidence.issues.contains { $0.contains("process identity changed during resource observation") })
    }
}

@Test func missingExecutableCannotHideUnlinkedWorkspaceCustody() throws {
    try withFenceIdentityFixture { original, _, dir in
        // Keep the fixture's original FD open while removing only its own
        // pathname. Restore it afterwards for the fixture's preservation check.
        let disk = URL(fileURLWithPath: original.rootfs)
        let saved = try Data(contentsOf: disk)
        try FileManager.default.removeItem(at: disk)
        defer { try? saved.write(to: disk); chmod(disk.path, 0o600) }
        var request = original
        request.workspaceAbsent = true
        request.container = dir.path + "/containers/agent-fixture-actor"
        let evidence = try AgentFenceProbe.run(request, access: missingOwnExecutable())
        #expect(!evidence.confirmed)
        #expect(evidence.issues.contains { $0.contains("unlinked vnode ownership unresolved") })
        #expect(evidence.issues.contains { $0.contains("original helper binding still live") })
    }
}

@Test func foreignMissingPathNamesEveryDeniedResourceBoundary() throws {
    try withFenceIdentityFixture { request, _, _ in
        var access = missingOwnExecutable()
        let native = access.info
        access.info = { pid, flavor, arg, buffer, size in
            guard pid == getpid() else { return native(pid, flavor, arg, buffer, size) }
            if flavor == PROC_PIDT_SHORTBSDINFO {
                let result = native(pid, flavor, arg, buffer, size)
                buffer?.assumingMemoryBound(to: proc_bsdshortinfo.self).pointee.pbsi_uid = getuid() + 1000
                return result
            }
            if [PROC_PIDTBSDINFO, PROC_PIDLISTFDS, PROC_PIDLISTFILEPORTS, PROC_PIDREGIONPATHINFO].contains(flavor) {
                errno = EPERM; return 0
            }
            return native(pid, flavor, arg, buffer, size)
        }
        let evidence = try AgentFenceProbe.run(request, access: access)
        #expect(!evidence.confirmed)
        for text in ["foreign process birth/identity unavailable errno 1", "descriptor inventory target exclusion unproven, errno 1", "fileport inventory target exclusion unproven, errno 1", "mapped-region target exclusion unproven, errno 1"] {
            #expect(evidence.issues.contains { $0.contains("pid \(getpid())") && $0.contains(text) })
        }
    }
}

@Test func stableForeignResourceCoverageCanExcludeOnlyTheTargetInode() throws {
    try withFenceIdentityFixture { request, _, _ in
        var access = missingOwnExecutable()
        let native = access.info
        access.info = { pid, flavor, arg, buffer, size in
            guard pid == getpid() else { return native(pid, flavor, arg, buffer, size) }
            if flavor == PROC_PIDT_SHORTBSDINFO || flavor == PROC_PIDTBSDINFO {
                let result = native(pid, flavor, arg, buffer, size)
                if flavor == PROC_PIDT_SHORTBSDINFO { buffer?.assumingMemoryBound(to: proc_bsdshortinfo.self).pointee.pbsi_uid = getuid() + 1000 }
                else { buffer?.assumingMemoryBound(to: proc_bsdinfo.self).pointee.pbi_uid = getuid() + 1000 }
                return result
            }
            if flavor == PROC_PIDLISTFDS || flavor == PROC_PIDLISTFILEPORTS { errno = 0; return 0 }
            if flavor == PROC_PIDREGIONPATHINFO { errno = EINVAL; return 0 }
            return native(pid, flavor, arg, buffer, size)
        }
        let evidence = try AgentFenceProbe.run(request, access: access)
        // This deliberately synthetic empty target inventory checks the fallback
        // decision, never the actual account's permissions or global clearance.
        #expect(!evidence.issues.contains { $0.contains("pid \(getpid()): executable path unavailable") })
        #expect(!evidence.issues.contains { $0.contains("pid \(getpid())") && $0.contains("original helper binding") })
    }
}

@Test func launchCustodyIsPrivateExactAndNeverReplacesAnIncarnation() throws {
    try withFenceIdentityFixture { request, identity, directory in
        try AgentLaunchCustody.write(directory: directory.path, rootfs: request.rootfs,
            environment: request.environment, session: request.session, incarnation: request.incarnation,
            sourceSHA: String(repeating: "a", count: 40))
        let receipt = directory.appendingPathComponent("launch-custody-actor.json")
        let bytes = try Data(contentsOf: receipt)
        let object = try #require(JSONSerialization.jsonObject(with: bytes) as? [String: Any])
        #expect(object["stage"] as? String == "before-virtualization-create")
        #expect((object["helperBirthSeconds"] as? NSNumber)?.uint64Value == identity.pbi_start_tvsec)
        #expect((object["helperBirthMicroseconds"] as? NSNumber)?.uint64Value == identity.pbi_start_tvusec)
        var disk = stat(), record = stat()
        #expect(lstat(request.rootfs, &disk) == 0)
        #expect(lstat(receipt.path, &record) == 0)
        #expect(record.st_mode & 0o777 == 0o600)
        #expect((object["rootfsInode"] as? NSNumber)?.uint64Value == disk.st_ino)
        #expect(throws: AgentLaunchCustody.Failure.self) {
            try AgentLaunchCustody.write(directory: directory.path, rootfs: request.rootfs,
                environment: "changed", session: request.session, incarnation: request.incarnation,
                sourceSHA: String(repeating: "b", count: 40))
        }
        #expect(try Data(contentsOf: receipt) == bytes)
    }
}

@Test func launchCustodyRefusesPublicOrLinkedWorkspaceBeforeReceipt() throws {
    try withFenceIdentityFixture { request, _, directory in
        chmod(request.rootfs, 0o644)
        defer { chmod(request.rootfs, 0o600) }
        #expect(throws: AgentLaunchCustody.Failure.self) {
            try AgentLaunchCustody.write(directory: directory.path, rootfs: request.rootfs,
                environment: request.environment, session: request.session, incarnation: "public",
                sourceSHA: String(repeating: "a", count: 40))
        }
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("launch-custody-public.json").path))
        chmod(request.rootfs, 0o600)
        let link = directory.appendingPathComponent("alias.ext4")
        #expect(Darwin.link(request.rootfs, link.path) == 0)
        defer { unlink(link.path) }
        #expect(throws: AgentLaunchCustody.Failure.self) {
            try AgentLaunchCustody.write(directory: directory.path, rootfs: request.rootfs,
                environment: request.environment, session: request.session, incarnation: "linked",
                sourceSHA: String(repeating: "a", count: 40))
        }
        #expect(!FileManager.default.fileExists(atPath: directory.appendingPathComponent("launch-custody-linked.json").path))
    }
}

@Test func foreignFallbackCannotHideAnActualTargetDescriptor() throws {
    try withFenceIdentityFixture { request, _, _ in
        var access = missingOwnExecutable()
        let native = access.info
        access.info = { pid, flavor, arg, buffer, size in
            let result = native(pid, flavor, arg, buffer, size)
            if pid == getpid() {
                if flavor == PROC_PIDT_SHORTBSDINFO { buffer?.assumingMemoryBound(to: proc_bsdshortinfo.self).pointee.pbsi_uid = getuid() + 1000 }
                if flavor == PROC_PIDTBSDINFO { buffer?.assumingMemoryBound(to: proc_bsdinfo.self).pointee.pbi_uid = getuid() + 1000 }
            }
            return result
        }
        let evidence = try AgentFenceProbe.run(request, access: access)
        #expect(!evidence.confirmed)
        #expect(evidence.issues.contains { $0.contains("pid \(getpid()) fd") && $0.contains("workspace inode remains open") })
        #expect(evidence.issues.contains { $0.contains("pid \(getpid()): executable path unavailable") })
    }
}
