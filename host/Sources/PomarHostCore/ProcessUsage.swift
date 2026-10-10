import Darwin
import Foundation

/// Public kernel counters for an explicitly named process owned by this user.
/// No command line, environment, credentials or process memory is read.
public enum ProcessUsage {
    public struct Snapshot: Codable, Sendable {
        public let schema: String
        public let time: String
        public let pid: Int32
        public let uid: UInt32
        public let executable: String
        public let startedSeconds: UInt64
        public let startedMicroseconds: UInt64
        public let residentBytes: UInt64
        public let physicalFootprintBytes: UInt64
        public let userCPUTimeNanoseconds: UInt64
        public let systemCPUTimeNanoseconds: UInt64
    }

    public enum Failure: Error { case unavailable, wrongOwner, identityChanged }

    private static func identity(_ pid: Int32) throws -> (proc_bsdinfo, String) {
        guard pid > 0 else { throw Failure.unavailable }
        var info = proc_bsdinfo()
        let size = Int32(MemoryLayout<proc_bsdinfo>.size)
        guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, size) == size else { throw Failure.unavailable }
        guard info.pbi_uid == getuid(), info.pbi_ruid == getuid() else { throw Failure.wrongOwner }
        // Darwin's PROC_PIDPATHINFO_MAXSIZE is 4 * MAXPATHLEN (4096).
        // Swift cannot import that expression macro from the pinned SDK.
        var path = [CChar](repeating: 0, count: 4096)
        guard proc_pidpath(pid, &path, UInt32(path.count)) > 0 else { throw Failure.unavailable }
        return (info, path.withUnsafeBufferPointer { String(cString: $0.baseAddress!) })
    }

    public static func read(pid: Int32) throws -> Snapshot {
        let (before, executable) = try identity(pid)
        var usage = rusage_info_v4()
        let result = withUnsafeMutablePointer(to: &usage) {
            $0.withMemoryRebound(to: rusage_info_t?.self, capacity: 1) {
                proc_pid_rusage(pid, RUSAGE_INFO_V4, $0)
            }
        }
        guard result == 0 else { throw Failure.unavailable }
        let (after, afterPath) = try identity(pid)
        guard before.pbi_start_tvsec == after.pbi_start_tvsec,
            before.pbi_start_tvusec == after.pbi_start_tvusec, executable == afterPath else {
            throw Failure.identityChanged
        }
        return Snapshot(
            schema: "pomar.host-process-pressure/v1", time: ISO8601DateFormatter().string(from: Date()),
            pid: pid, uid: before.pbi_uid, executable: executable,
            startedSeconds: before.pbi_start_tvsec, startedMicroseconds: before.pbi_start_tvusec,
            residentBytes: usage.ri_resident_size, physicalFootprintBytes: usage.ri_phys_footprint,
            userCPUTimeNanoseconds: usage.ri_user_time, systemCPUTimeNanoseconds: usage.ri_system_time)
    }

    /// Bind every sample to the first process identity; PID reuse is a refusal.
    public static func sameProcess(_ a: Snapshot, _ b: Snapshot) -> Bool {
        a.pid == b.pid && a.uid == b.uid && a.executable == b.executable
            && a.startedSeconds == b.startedSeconds && a.startedMicroseconds == b.startedMicroseconds
    }
}
