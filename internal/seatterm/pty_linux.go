package seatterm

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPTY opens a pseudo-terminal pair: the controlling side, and the
// path of the terminal side.
func openPTY() (*os.File, string, error) {
	fd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	// Non-blocking, so the runtime poller owns it and reads can have deadlines.
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, "", err
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		master.Close()
		return nil, "", e
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		master.Close()
		return nil, "", e
	}
	return master, fmt.Sprintf("/dev/pts/%d", n), nil
}
