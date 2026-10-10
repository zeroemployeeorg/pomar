package serviceupgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Open nonblocking and no-follow, then inspect the descriptor before reading.
// This rejects FIFOs, hardlinks, mutable ownership and a replaced pathname.
func readFile(path string, maximum int64, uid uint32) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("required regular file unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || (uid != ^uint32(0) && st.Uid != uid) || st.Nlink != 1 || before.Mode().Perm()&0022 != 0 || before.Size() <= 0 || before.Size() > maximum {
		return nil, fmt.Errorf("file custody or size refused")
	}
	b, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || len(b) > int(maximum) || int64(len(b)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, fmt.Errorf("file changed while reading")
	}
	return b, nil
}
func pinnedFile(path string, pin FilePin) ([]byte, error) {
	b, err := readFile(path, pin.Bytes, 0)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != pin.Bytes || digest(b) != pin.SHA256 {
		return nil, fmt.Errorf("file does not match frozen binding")
	}
	return b, nil
}

func directory(path string, uid uint32, mode os.FileMode) error {
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("required directory unavailable")
	}
	meta, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || meta.Uid != uid || (mode != 0 && st.Mode().Perm() != mode) || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("directory custody refused")
	}
	return nil
}

// The sole writable ancestor allowed is macOS's root-owned sticky staging
// directory. The selected leaf must itself be root-owned 0700.
func privateRoot(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("absolute canonical private directory required")
	}
	if err := directory(path, 0, 0700); err != nil {
		return err
	}
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		if p == "/private/var/tmp" {
			st, err := os.Lstat(p)
			if err != nil {
				return err
			}
			m, ok := st.Sys().(*syscall.Stat_t)
			if !ok || !st.IsDir() || m.Uid != 0 || st.Mode()&os.ModeSticky == 0 {
				return fmt.Errorf("staging ancestor refused")
			}
		} else if err := directory(p, 0, 0); err != nil {
			return err
		}
		if p == "/" {
			break
		}
	}
	return nil
}
func durableWrite(path string, b []byte, mode os.FileMode) error {
	if err := directory(filepath.Dir(path), 0, 0); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pomar-upgrade-pending-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
