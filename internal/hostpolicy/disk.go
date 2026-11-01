// Package hostpolicy reads host-owned admission settings. It never writes them.
package hostpolicy

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const DefaultMaxFillPercent = 85
const DiskLimitPath = "/etc/pomar/max-fill-percent"

// DiskLimit is one observed host setting, shared by heavy work and admission.
type DiskLimit struct {
	Percent int    `json:"percent"`
	Source  string `json:"source"`
}

func DefaultDiskLimit() DiskLimit {
	return DiskLimit{Percent: DefaultMaxFillPercent, Source: "built-in default"}
}

// Validated preserves the default for invalid or absent injected settings too.
func (p DiskLimit) Validated() DiskLimit {
	if p.Percent < 1 || p.Percent > 99 {
		return DefaultDiskLimit()
	}
	if p.Source == "" {
		p.Source = "explicit host setting"
	}
	return p
}

// LoadDiskLimit samples the same host-wide setting for every data root and
// account. Missing, unreadable, oversized or invalid input always uses 85.
// No hostname inference or environment variable silently raises this limit.
func LoadDiskLimit() DiskLimit { return ReadDiskLimit(DiskLimitPath) }

// ReadOptions supplies trusted metadata for deterministic tests. Production
// leaves OwnerUID nil and uses the opened object's kernel ownership metadata.
type ReadOptions struct {
	OwnerUID func(os.FileInfo) (uint32, bool)
}

func ReadDiskLimit(path string) DiskLimit { return ReadDiskLimitWithOptions(path, ReadOptions{}) }

func kernelOwner(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func trustedMetadata(info os.FileInfo, owner func(os.FileInfo) (uint32, bool)) bool {
	uid, known := owner(info)
	return known && uid == 0 && info.Mode().Perm()&0022 == 0
}

// ReadDiskLimitWithOptions holds the validated directory while opening the
// setting relative to it. Metadata comes from the opened objects, not a later
// lookup of names that could have been replaced.
func ReadDiskLimitWithOptions(path string, opts ReadOptions) DiskLimit {
	owner := opts.OwnerUID
	if owner == nil {
		owner = kernelOwner
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return fallback(path, "parent unreadable")
	}
	if !info.IsDir() || !trustedMetadata(info, owner) {
		return fallback(path, "parent must be uid 0 and not group/world writable")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return fallback(path, "parent unreadable")
	}
	defer root.Close()
	info, err = root.Stat(".")
	if err != nil || !info.IsDir() || !trustedMetadata(info, owner) {
		return fallback(path, "opened parent must be uid 0 and not group/world writable")
	}
	// Root methods may resolve in-root symlinks even with O_NOFOLLOW. Refuse
	// a named link and bind the opened descriptor to this observed regular file.
	before, err := root.Lstat(filepath.Base(path))
	if err != nil || !before.Mode().IsRegular() {
		return fallback(path, "unreadable")
	}
	if !trustedMetadata(before, owner) {
		return fallback(path, "file must be uid 0 and not group/world writable")
	}
	f, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fallback(path, "unreadable")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fallback(path, "unreadable")
	}
	if !os.SameFile(before, info) {
		return fallback(path, "file changed while opening")
	}
	if !trustedMetadata(info, owner) {
		return fallback(path, "file must be uid 0 and not group/world writable")
	}
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return fallback(path, "unreadable")
	}
	return parseDiskLimit(path, b)
}

func fallback(path, reason string) DiskLimit {
	return DiskLimit{Percent: DefaultMaxFillPercent, Source: "built-in default (" + path + " " + reason + ")"}
}

func parseDiskLimit(path string, b []byte) DiskLimit {
	if len(b) > 32 {
		return fallback(path, "invalid")
	}
	s := strings.TrimSpace(string(b))
	// Only an unsigned ASCII decimal is a setting, not a flag or expression.
	if len(s) == 0 {
		return fallback(path, "invalid")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return fallback(path, "invalid")
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 99 {
		return fallback(path, "invalid")
	}
	return DiskLimit{Percent: n, Source: path}
}
