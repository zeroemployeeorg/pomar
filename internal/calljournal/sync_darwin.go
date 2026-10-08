//go:build darwin

package calljournal

import (
	"os"
	"syscall"
)

func flush(f *os.File) error {
	if e := f.Sync(); e != nil {
		return e
	}
	_, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), 51, 0)
	if e != 0 {
		return e
	}
	return nil
}
