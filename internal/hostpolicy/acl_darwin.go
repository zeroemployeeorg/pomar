package hostpolicy

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// fgetattrlist reads security metadata from the already-open object. Accept
// only the exact empty extended-security result. Any ACL (including a deny
// or empty ACL), unsupported metadata, error or malformed result falls back.
// ABI: Apple sys/attr.h ATTR_CMN_EXTENDED_SECURITY, FSOPT_REPORT_FULLSIZE.
func kernelACLSafe(f *os.File) bool {
	attrs := struct {
		Count, Reserved                       uint16
		Common, Volume, Directory, File, Fork uint32
	}{Count: 5, Common: 0x00400000}
	var out [12]byte
	_, _, errno := syscall.Syscall6(syscall.SYS_FGETATTRLIST, f.Fd(), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)), 4, 0)
	runtime.KeepAlive(f)
	return errno == 0 && emptyDarwinACL(out[:])
}

func emptyDarwinACL(out []byte) bool {
	return len(out) == 12 && binary.LittleEndian.Uint32(out[:4]) == 12 && binary.LittleEndian.Uint32(out[4:8]) == 8 && binary.LittleEndian.Uint32(out[8:12]) == 0
}
