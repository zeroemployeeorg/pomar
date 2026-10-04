import Darwin
import Foundation

/// Read-only machine evidence for the direct Virtualization backend. It neither
/// stops a process nor asserts that a historical launch never executed.
public enum AgentFenceProbe {
    public struct Request: Codable {
        public let environment: String
        public let session: String
        public let incarnation: String
        public let operation: String
        public let rootfs: String
        public let helper: String
        public let pid: Int32
        public let uid: UInt32
        public let birth: UInt64
    }
    public struct Evidence: Codable {
        public var environment: String
        public var session: String
        public var incarnation: String
        public var operation: String
        public var observedAt: String
        public var coverage = "direct-vz-private-disk/v1"
        public var observerUID: UInt32
        public var processCount = 0
        public var ownProcessesChecked = 0
        public var vnodeDescriptorsChecked = 0
        public var fileportsChecked = 0
        public var regionsChecked = 0
        public var kernelZombiesChecked: [Int32] = []
        public var rootfsDevice: UInt32 = 0
        public var rootfsInode: UInt64 = 0
        public var issues: [String] = []
        public var confirmed = false
    }
    private static func pids() throws -> Set<Int32> {
        // Do not silently truncate a kernel inventory.
        var count = max(proc_listallpids(nil, 0), 1024)
        for _ in 0..<4 {
            var ids = [Int32](repeating: 0, count: Int(count) + 1024)
            let n = proc_listallpids(&ids, Int32(ids.count * MemoryLayout<Int32>.stride))
            guard n > 0 else { throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno)) }
            if n < ids.count { return Set(ids.prefix(Int(n)).filter { $0 > 0 }) }
            count *= 2
        }
        throw NSError(domain: "AgentFenceProbe.inventoryIncomplete", code: 1)
    }
    private static func vanished(_ pid: Int32) -> Bool {
        // ESRCH alone is not interpreted as departure.
        guard let live = try? pids() else { return false }
        return !live.contains(pid)
    }
    private static func zombie(_ pid: Int32) -> Bool {
        // proc_listallpids includes zombies while libproc excludes them. A
        // positive kernel zombie record proves no remaining task/file table;
        // ESRCH by itself does not. Zombies cannot return to runnable state.
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.size
        return sysctl(&mib, UInt32(mib.count), &info, &size, nil, 0) == 0
            && size == MemoryLayout<kinfo_proc>.size && info.kp_proc.p_pid == pid
            && info.kp_proc.p_stat == SZOMB
    }
    public static func run(_ r: Request) throws -> Evidence {
        guard r.uid == getuid(), r.pid > 0, r.birth > 0,
              r.rootfs.hasPrefix("/"), r.helper.hasPrefix("/") else {
            throw NSError(domain: "AgentFenceProbe.invalidBinding", code: 1)
        }
        var out = Evidence(environment: r.environment, session: r.session,
                           incarnation: r.incarnation, operation: r.operation,
                           observedAt: ISO8601DateFormatter().string(from: Date()), observerUID: getuid())
        var disk = stat()
        guard lstat(r.rootfs, &disk) == 0, disk.st_mode & S_IFMT == S_IFREG,
              disk.st_uid == r.uid, disk.st_mode & 0o077 == 0 else {
            out.issues.append("workspace inode/owner/privacy lookup incomplete")
            return out
        }
        out.rootfsDevice = UInt32(bitPattern: disk.st_dev)
        out.rootfsInode = disk.st_ino
        func sameDisk(_ v: vinfo_stat) -> Bool {
            v.vst_dev == out.rootfsDevice && v.vst_ino == out.rootfsInode
        }
        // A second pass covers exits/reparenting and newly observed processes.
        // The Go owner holds the root launch lock and environment operation lock
        // throughout both passes AND the subsequent durable receipt commit.
        for _ in 0..<2 {
            let inventory = try pids()
            out.processCount = max(out.processCount, inventory.count)
            for pid in inventory.sorted() {
                var short = proc_bsdshortinfo()
                let n = proc_pidinfo(pid, PROC_PIDT_SHORTBSDINFO, 0, &short, Int32(MemoryLayout<proc_bsdshortinfo>.size))
                if n != MemoryLayout<proc_bsdshortinfo>.size {
                    if zombie(pid) { out.kernelZombiesChecked.append(pid) }
                    else if !vanished(pid) { out.issues.append("pid \(pid): kernel identity unavailable errno \(errno)") }
                    continue
                }
                var path = [CChar](repeating: 0, count: 4096)
                let k = proc_pidpath(pid, &path, UInt32(path.count))
                if k <= 0 {
                    if !vanished(pid) { out.issues.append("pid \(pid): executable path unavailable errno \(errno)") }
                    continue
                }
                let executable = String(cString: path)
                // Any service of the installed Apple Virtualization framework
                // holds reconciliation when its launch ownership cannot be
                // excluded. Names/PIDs are not used as negative evidence.
                if executable.hasPrefix("/System/Library/Frameworks/Virtualization.framework/") {
                    out.issues.append("pid \(pid) uid \(short.pbsi_uid): live Virtualization executor \(executable), ownership unresolved")
                }
                guard short.pbsi_uid == r.uid else { continue }
                var identity = proc_bsdinfo()
                if proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &identity, Int32(MemoryLayout<proc_bsdinfo>.size)) != MemoryLayout<proc_bsdinfo>.size {
                    if !vanished(pid) { out.issues.append("pid \(pid): owned process birth unavailable") }
                    continue
                }
                out.ownProcessesChecked += 1
                if pid == r.pid && identity.pbi_start_tvsec == r.birth {
                    out.issues.append("pid \(pid) uid \(r.uid) birth \(r.birth): original helper incarnation still live at \(executable)")
                }
                // The original helper could have forked before departure; any
                // owned instance of that executable is conservatively held.
                if executable == r.helper {
                    out.issues.append("pid \(pid): retained helper executable live, launch ownership unresolved")
                }
                errno = 0
                let size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, nil, 0)
                if size <= 0 && errno != 0 {
                    if !vanished(pid) { out.issues.append("pid \(pid): descriptor inventory unavailable") }
                    continue
                }
                var descriptors = [proc_fdinfo](repeating: proc_fdinfo(), count: Int(size) / MemoryLayout<proc_fdinfo>.size + 256)
                errno = 0
                let bytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, &descriptors, Int32(descriptors.count * MemoryLayout<proc_fdinfo>.size))
                if (bytes <= 0 && errno != 0) || bytes >= descriptors.count * MemoryLayout<proc_fdinfo>.size {
                    if !vanished(pid) { out.issues.append("pid \(pid): descriptor inventory incomplete") }
                    continue
                }
                for fd in descriptors.prefix(Int(bytes) / MemoryLayout<proc_fdinfo>.size) where fd.proc_fdtype == UInt32(PROX_FDTYPE_VNODE) {
                    var v = vnode_fdinfowithpath()
                    if proc_pidfdinfo(pid, fd.proc_fd, PROC_PIDFDVNODEPATHINFO, &v, Int32(MemoryLayout<vnode_fdinfowithpath>.size)) != MemoryLayout<vnode_fdinfowithpath>.size {
                        // An FD may close between enumeration and lookup. Check
                        // it again; an inaccessible live descriptor is unknown.
                        var check = vnode_fdinfowithpath()
                        if proc_pidfdinfo(pid, fd.proc_fd, PROC_PIDFDVNODEPATHINFO, &check, Int32(MemoryLayout<vnode_fdinfowithpath>.size)) != MemoryLayout<vnode_fdinfowithpath>.size && errno == EBADF { continue }
                        if !vanished(pid) { out.issues.append("pid \(pid) fd \(fd.proc_fd): vnode lookup incomplete") }
                        continue
                    }
                    out.vnodeDescriptorsChecked += 1
                    if sameDisk(v.pvip.vip_vi.vi_stat) { out.issues.append("pid \(pid) fd \(fd.proc_fd): workspace inode remains open") }
                }
                errno = 0
                let portSize = proc_pidinfo(pid, PROC_PIDLISTFILEPORTS, 0, nil, 0)
                if portSize <= 0 && errno != 0 {
                    if !vanished(pid) { out.issues.append("pid \(pid): fileport inventory unavailable") }
                } else if portSize > 0 {
                    var ports = [proc_fileportinfo](repeating: proc_fileportinfo(), count: Int(portSize) / MemoryLayout<proc_fileportinfo>.size + 256)
                    errno = 0
                    let read = proc_pidinfo(pid, PROC_PIDLISTFILEPORTS, 0, &ports, Int32(ports.count * MemoryLayout<proc_fileportinfo>.size))
                    if (read <= 0 && errno != 0) || read >= ports.count * MemoryLayout<proc_fileportinfo>.size {
                        out.issues.append("pid \(pid): fileport inventory incomplete")
                    } else {
                        for port in ports.prefix(Int(read) / MemoryLayout<proc_fileportinfo>.size) where port.proc_fdtype == UInt32(PROX_FDTYPE_VNODE) {
                            var v = vnode_fdinfowithpath()
                            if proc_pidfileportinfo(pid, port.proc_fileport, PROC_PIDFDVNODEPATHINFO, &v, Int32(MemoryLayout<vnode_fdinfowithpath>.size)) != MemoryLayout<vnode_fdinfowithpath>.size {
                                if !vanished(pid) { out.issues.append("pid \(pid) fileport \(port.proc_fileport): vnode lookup incomplete") }
                            } else {
                                out.fileportsChecked += 1
                                if sameDisk(v.pvip.vip_vi.vi_stat) { out.issues.append("pid \(pid): workspace inode retained by fileport") }
                            }
                        }
                    }
                }
                var address: UInt64 = 0
                var finished = false
                for _ in 0..<65536 {
                    var region = proc_regionwithpathinfo()
                    errno = 0
                    let read = proc_pidinfo(pid, PROC_PIDREGIONPATHINFO, address, &region, Int32(MemoryLayout<proc_regionwithpathinfo>.size))
                    if read != MemoryLayout<proc_regionwithpathinfo>.size {
                        // EINVAL indicates no VM region at or above address.
                        if errno == EINVAL || vanished(pid) { finished = true; break }
                        out.issues.append("pid \(pid): mapped-region lookup unavailable at \(address), errno \(errno)")
                        finished = true; break
                    }
                    out.regionsChecked += 1
                    if sameDisk(region.prp_vip.vip_vi.vi_stat) { out.issues.append("pid \(pid): workspace inode remains mapped") }
                    let next = region.prp_prinfo.pri_address.addingReportingOverflow(region.prp_prinfo.pri_size)
                    if next.overflow || next.partialValue <= address { out.issues.append("pid \(pid): non-progressing region inventory"); finished = true; break }
                    address = next.partialValue
                }
                if !finished { out.issues.append("pid \(pid): mapped-region inventory truncated") }
            }
        }
        var finalDisk = stat()
        if lstat(r.rootfs, &finalDisk) != 0 || finalDisk.st_dev != disk.st_dev || finalDisk.st_ino != disk.st_ino {
            out.issues.append("workspace inode changed during observation")
        }
        out.issues = Array(Set(out.issues)).sorted()
        out.kernelZombiesChecked = Array(Set(out.kernelZombiesChecked)).sorted()
        out.confirmed = out.issues.isEmpty
        return out
    }
}
