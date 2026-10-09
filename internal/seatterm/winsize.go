package seatterm

import (
	"os"
	"syscall"
	"unsafe"
)

func setSize(f *os.File, cols, rows uint16) error {
	ws := struct{ rows, cols, x, y uint16 }{rows, cols, 0, 0}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return e
	}
	return nil
}

// resize sets the window size through a brief reopen of the terminal side.
// The parent never keeps that side open: if it did, the controlling side
// would never see the end of the terminal's last process.
func resize(tty string, cols, rows uint16) {
	f, err := os.OpenFile(tty, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return
	}
	setSize(f, cols, rows)
	f.Close()
}
