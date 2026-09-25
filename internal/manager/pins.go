package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// CommandHash is the sha256 of a command's canonical JSON encoding (a JSON
// array of its arguments). A result's check binds to it, so a passing
// `make test` cannot stand for a required `make verify` (DESIGN-01).
func CommandHash(command []string) string {
	b, _ := json.Marshal(command)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// BuildVersion names the Pomar build: its VCS revision, with "+dirty" when
// the tree had changes, or "devel" when the build carries no VCS stamp.
func BuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "devel"
	}
	if dirty {
		return rev + "+dirty"
	}
	return rev
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pinsFor records what an attempt of command runs with.
func (m *Manager) pinsFor(command []string) (*Pins, error) {
	host, err := fileSHA256(m.cfg.HostBin)
	if err != nil {
		return nil, fmt.Errorf("manager: hashing the helper binary: %w", err)
	}
	g := m.cfg.Guest
	return &Pins{
		Kernel: g.KernelSHA256, Init: g.InitDigest, Image: g.ImageDigest, ImageArm64: g.ImageArm64,
		PackageSet: g.PackageSet, Pomar: BuildVersion(), HostBin: host, CommandHash: CommandHash(command),
	}, nil
}
