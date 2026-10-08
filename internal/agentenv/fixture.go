package agentenv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

// QualificationFixture is an owner-supplied synthetic file placed once into a
// new environment of a qualification profile before its agent starts, for
// adapter qualification (POMAR-CC fork 2; the POMAR Codex's constraints in
// #764 6052252128). It is never caller-supplied: only an owner profile marked
// Qualification names one. It is never a real credential: its content is a
// public synthetic marker.
type QualificationFixture struct {
	// Source is the owner's file, directly inside <host root>/fixtures.
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Destination is the compiled guest path for the profile's agent.
	Destination string `json:"destination"`
}

// ClaudeQualificationDestination is the one guest path a fixture may take,
// for a Claude Code qualification profile: the coding user's credential file,
// so a never-signed-in environment holds a synthetic credential instead.
const ClaudeQualificationDestination = "/pomar/job/.claude/.credentials.json"

const maxFixtureSize = 64 << 10

var fixtureSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fixtureAfterRead runs between the read and the identity recheck; tests use
// it to replace the source at that moment.
var fixtureAfterRead = func() {}

// ValidateFixture checks the fixture's description and the owner's custody of
// its source:
//   - the source is directly inside <root>/fixtures, with no symlink at the
//     fixtures directory or the file;
//   - the directory is the owner's, mode 0700; the file is the owner's,
//     regular, mode 0600, with one link and the exact size;
//   - the bytes read through the opened file have the exact sha256;
//   - the path still names the same file and directory after the read.
//
// The VM owner checks the same again when it copies the bytes in.
func ValidateFixture(root string, f QualificationFixture, agent string, uid int) error {
	if agent != "claude" || f.Destination != ClaudeQualificationDestination {
		return errors.New("qualification fixture: no such destination for this agent")
	}
	if !fixtureSHA.MatchString(f.SHA256) || f.Size < 1 || f.Size > maxFixtureSize {
		return errors.New("qualification fixture: needs its sha256 and an exact size of at most 64 KiB")
	}
	dir := filepath.Join(root, "fixtures")
	if !filepath.IsAbs(f.Source) || filepath.Clean(f.Source) != f.Source || filepath.Dir(f.Source) != dir {
		return errors.New("qualification fixture: the source must be directly inside the host root's fixtures directory")
	}
	dirInfo, err := ownedPrivate(dir, uid, true)
	if err != nil {
		return err
	}
	data, fileInfo, err := readPinned(f.Source, f.Size, uid)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != f.SHA256 {
		return errors.New("qualification fixture: digest mismatch")
	}
	fixtureAfterRead()
	// The path must still name what was read: a replaced file or directory
	// is refused, never followed.
	if after, err := os.Lstat(f.Source); err != nil || !os.SameFile(after, fileInfo) {
		return errors.New("qualification fixture: the source was replaced during validation")
	}
	if after, err := os.Lstat(dir); err != nil || !os.SameFile(after, dirInfo) {
		return errors.New("qualification fixture: the fixtures directory was replaced during validation")
	}
	return nil
}

// ownedPrivate lstats a path and requires the owner's, not a symlink, with no
// group or other access.
func ownedPrivate(path string, uid int, dir bool) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("qualification fixture: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() != dir || (!dir && !fi.Mode().IsRegular()) || int(st.Uid) != uid || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("qualification fixture: %s must be the owner's, private, and not a symlink", path)
	}
	return fi, nil
}

// readPinned opens the file without following a symlink, checks the opened
// file's identity, and reads exactly its size.
func readPinned(path string, size int64, uid int) ([]byte, os.FileInfo, error) {
	before, err := ownedPrivate(path, uid, false)
	if err != nil {
		return nil, nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("qualification fixture: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	st := opened.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || st.Nlink != 1 || opened.Size() != size {
		return nil, nil, errors.New("qualification fixture: the source must be one regular 0600 file, with one link and the exact size")
	}
	data, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil || int64(len(data)) != size {
		return nil, nil, errors.New("qualification fixture: the source's size changed")
	}
	return data, opened, nil
}
