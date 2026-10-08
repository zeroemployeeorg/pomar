//go:build darwin

package localclient

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// xucred is macOS's struct xucred (sys/ucred.h): a version, the effective uid,
// and the groups.
type xucred struct {
	Version uint32
	UID     uint32
	NGroups int16
	_       [2]byte
	Groups  [16]uint32
}

const (
	solLocal      = 0 // SOL_LOCAL
	localPeerCred = 1 // LOCAL_PEERCRED
	xucredVersion = 0 // XUCRED_VERSION
)

// peerUID is the uid of the process at the other end of a Unix socket, as the
// kernel recorded it when that process connected: never anything the client
// says about itself.
func peerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("manager: not a Unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred xucred
	var serr error
	err = raw.Control(func(fd uintptr) {
		l := uint32(unsafe.Sizeof(cred))
		_, _, e := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, solLocal, localPeerCred,
			uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&l)), 0)
		if e != 0 {
			serr = e
		}
	})
	if err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, fmt.Errorf("manager: LOCAL_PEERCRED: %w", serr)
	}
	if cred.Version != xucredVersion {
		return 0, fmt.Errorf("manager: LOCAL_PEERCRED: xucred version %d", cred.Version)
	}
	return cred.UID, nil
}
