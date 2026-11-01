// Package hostpolicy reads host-owned admission settings. It never writes them.
package hostpolicy

import (
	"io"
	"os"
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

func ReadDiskLimit(path string) DiskLimit {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fallback(path, "unreadable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fallback(path, "unreadable")
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
