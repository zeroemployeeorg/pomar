package hostpolicy

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

func kernelACLSafe(f *os.File) bool {
	name := []byte("system.posix_acl_access\x00")
	_, _, errno := syscall.Syscall6(syscall.SYS_FGETXATTR, f.Fd(), uintptr(unsafe.Pointer(&name[0])), 0, 0, 0, 0)
	runtime.KeepAlive(f)
	// ENODATA proves absence. Unsupported metadata remains unknown.
	return errno == syscall.ENODATA
}
