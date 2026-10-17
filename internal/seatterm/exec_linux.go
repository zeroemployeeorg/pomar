package seatterm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// checkExpectedSHA256 opens path once, hashes it, and refuses on a
// mismatch. On a match, it never lets cmd re-open or re-resolve path at
// exec time: the opened file is handed to the child as an inherited
// descriptor (cmd.ExtraFiles), and cmd.Path becomes that descriptor's
// /proc/self/fd entry, which the kernel resolves to the exact inode
// already opened and hashed, never a name. A file REPLACED at path (a
// rename, or unlink+create swapping in a different inode) after this
// check returns can't be what actually runs, since the pinned exec never
// looks the name up again: that is the window this closes, and it is
// genuinely zero for a replacement.
//
// CORRECTED CLAIM (this used to say the window was zero, full stop,
// which overclaimed): a WRITE into the SAME inode -- not a replacement --
// between the hash finishing (io.Copy above) and the kernel's own read of
// that inode's bytes during execve is NOT closed by this. Both this
// function's read and the eventual exec go through the same fd/inode, so
// anyone with write access to the file's own bytes (not merely rename
// rights on its directory) can still alter what actually runs in that
// gap; the file is read-write open here, not sealed against writes. A
// sealed, write-once memfd (memfd_create + fcntl F_ADD_SEALS with
// F_SEAL_WRITE) copied from these exact hashed bytes, exec'd instead of
// this fd, would close that remaining window too -- not implemented here;
// this is a named, not a silently assumed-away, gap.
//
// It returns closeAfterStart: cmd.Start() dup2s f into the child, which
// then holds its own copy, so the parent's f is the caller's to close
// once Start has returned (success or not) -- os/exec never closes an
// ExtraFiles entry for you, and this package's own host never did either,
// leaking one fd per launch for as long as the host itself runs.
//
// preservesInstallPath (Command.PreservesInstallPath) refuses the pin
// outright rather than open path at all, the same as the Darwin version,
// for a program that resolves resources relative to its own installed
// location. Unlike Darwin's private copy, cmd.Path here becomes
// /proc/self/fd/N against the SAME inode as the original, not a new file
// at a new path -- whether that leaves argv[0]/self-path-relative lookups
// resolvable (via /proc/self/exe, say) is plausible but has not been
// verified on an actual Linux host, so this refuses the same way rather
// than assume the narrower Darwin problem doesn't also apply here.
func checkExpectedSHA256(cmd *exec.Cmd, path, expected string, preservesInstallPath bool) (closeAfterStart func(), err error) {
	if expected == "" {
		return nil, nil
	}
	if preservesInstallPath {
		return nil, fmt.Errorf("%s resolves resources relative to its installed location; pinning it would run it by a different path (/proc/self/fd), unverified to preserve that, so the launch refuses rather than risk it", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s isn't a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != expected {
		f.Close()
		return nil, fmt.Errorf("%s hashes to %s, not the expected %s; refusing to launch", path, got, expected)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	// ExtraFiles start at fd 3 in the child (0, 1, 2 are stdin/stdout/stderr).
	cmd.Path = fmt.Sprintf("/proc/self/fd/%d", 3+len(cmd.ExtraFiles)-1)
	return func() { f.Close() }, nil
}
