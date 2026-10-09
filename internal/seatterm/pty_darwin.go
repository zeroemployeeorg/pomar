package seatterm

import (
	"bytes"
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
	for _, req := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, 0); e != 0 {
			master.Close()
			return nil, "", e
		}
	}
	var name [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		master.Close()
		return nil, "", e
	}
	return master, string(name[:bytes.IndexByte(name[:], 0)]), nil
}
