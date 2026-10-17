package seatterm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// checkExpectedSHA256 opens path once, hashes it while copying its bytes
// into a fresh private directory, and execs the copy, never path itself.
// Darwin has no portable fexecve equivalent (no reliable /proc/self/fd-style
// path that names an open descriptor's exact inode for exec), so unlike the
// Linux version this cannot hand the kernel the exact already-hashed inode.
// The copy instead closes the specific gap the council named — path being
// replaced after the check, before the exec that follows — because what
// actually runs is a file nothing but this process ever wrote, not path.
// It is copied from the SAME open descriptor the hash was taken from, never
// a second open of path, so the copy's bytes are exactly what was hashed.
// This narrows, not eliminates, the window: a sufficiently privileged,
// precisely timed replacement of path between this function's first open
// and its first read could still substitute what gets hashed and copied.
// That earlier, narrower window is real and is not claimed closed.
//
// One confirmed, named limit: this cannot pin a macOS platform binary (one
// signed by Apple with a platform identifier, as every binary under
// /bin, /usr/bin and the rest of the signed system volume is). The kernel's
// code-signing enforcement kills a copy of one of those on exec, regardless
// of where the copy lives or that its bytes are byte-identical (confirmed:
// copying /bin/sh and exec'ing the copy exits 137, SIGKILL). An ordinarily
// signed or ad-hoc-signed executable — which is what this exists to pin:
// Pomar's own binaries, Claude Code, Codex — is not a platform binary and
// is unaffected; a shebang script is unaffected too, since the kernel execs
// the named interpreter from its own real path, never the script itself.
// ExpectedSHA256 on a platform binary will fail at exec, not at the check.
//
// A second, confirmed, named limit, checked directly rather than assumed:
// a copy's own argv[0]/executable path moves, from the real install
// location to this private directory, and some installs resolve sibling
// resources relative to that path, not from a fixed location. Checked on
// this host (2026-10): Claude Code's install directory (Homebrew Cask)
// holds nothing beside the single ~207 MB binary -- it is fully
// self-contained, no dylib rpath, nothing to lose by moving. Codex's
// install (`~/.codex/packages/standalone/current/`) is NOT: its own
// `codex-package.json` manifest declares `resourcesDir`/`pathDir` as
// siblings of `bin/`, not of the `codex` binary alone, and that
// `codex-resources`/`codex-path` tree holds a bundled ripgrep and other
// helper binaries (voice, zsh) Codex plausibly locates relative to its
// own installed layout. A copy run from this function's private
// directory has none of those siblings. This was confirmed by reading
// the install layout and its manifest, not by running a copied Codex and
// observing the breakage directly -- whether ripgrep-backed search (or
// the other bundled features) actually fails from a moved copy is a named
// gap in this check, not verified end to end.
//
// Decided: rather than leave that gap live for whatever program turns out
// to need it, preservesInstallPath (Command.PreservesInstallPath) refuses
// the pin outright when set alongside expected, before any file is even
// opened. A silently relocated copy that can't find its own resources
// fails in a way that's hard to diagnose; refusing the combination fails
// loudly instead, at the one point that knows both facts at once.
//
// closeAfterStart is always a no-op here: unlike the Linux version, this
// never hands cmd an inherited descriptor (cmd.Path names a plain path,
// the private copy, not a /proc/self/fd entry), so there is nothing left
// open for the caller to close once Start returns. The signature matches
// the Linux version's so host.go's call site is platform-agnostic.
func checkExpectedSHA256(cmd *exec.Cmd, path, expected string, preservesInstallPath bool) (closeAfterStart func(), err error) {
	if expected == "" {
		return nil, nil
	}
	if preservesInstallPath {
		return nil, fmt.Errorf("%s resolves resources relative to its installed location; pinning it would run a copy with none of them, so the launch refuses rather than relocate it", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file", path)
	}
	dir, err := os.MkdirTemp("", "pomar-launch-")
	if err != nil {
		return nil, err
	}
	cp, err := os.OpenFile(dir+"/bin", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o500)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h, cp), f); err != nil {
		cp.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := cp.Close(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != expected {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("%s hashes to %s, not the expected %s; refusing to launch", path, got, expected)
	}
	// The copy is left on disk, named uniquely per launch under the OS
	// temp directory (0700, Go's own MkdirTemp default): forensic evidence
	// of exactly what ran, not cleaned up here. Its parent's cleanup is the
	// OS's own temp-directory reaping, the same as everything else under it.
	cmd.Path = cp.Name()
	return nil, nil
}
