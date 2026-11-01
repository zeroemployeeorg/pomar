// Package tools holds repository-level tests for the shell tools that
// don't belong to any one internal package. track3_preflight_test.go
// exercises tools/track3-preflight.sh as a real subprocess against
// controlled fixtures, never the live zeocore checkout or a real job.
package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sha256Sum(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

// lineWithPrefix returns the first line of out starting with prefix, tying
// an assertion to THAT specific report line rather than a bare substring
// search over the whole output. Council's review of 6f0f91e: an assertion
// like strings.Contains(out, ": VERIFIED") can match a completely different
// status line (e.g. scratch_ancestry_status) than the one actually under
// test, and would pass or fail for the wrong reason either way.
func lineWithPrefix(out, prefix string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

func wantLine(t *testing.T, out, prefix, wantExact string) {
	t.Helper()
	line, ok := lineWithPrefix(out, prefix)
	if !ok {
		t.Fatalf("no line starting with %q in output:\n%s", prefix, out)
	}
	if line != wantExact {
		t.Fatalf("line starting with %q: got %q, want %q\nfull output:\n%s", prefix, line, wantExact, out)
	}
}

func wantLineSuffix(t *testing.T, out, prefix, wantSuffix string) {
	t.Helper()
	line, ok := lineWithPrefix(out, prefix)
	if !ok {
		t.Fatalf("no line starting with %q in output:\n%s", prefix, out)
	}
	if !strings.HasSuffix(line, wantSuffix) {
		t.Fatalf("line starting with %q: got %q, want suffix %q\nfull output:\n%s", prefix, line, wantSuffix, out)
	}
}

// hermeticPATH is the ONLY source of PATH for every test below -- never
// os.Environ(), and (Council's review of 6f0f91e) never the real
// /opt/homebrew/bin by default either: the previous version always appended
// it as a PATH tail, so any fixture that forgot to shadow uv/pandoc with its
// own fake silently fell through to the REAL tool. Only the one test that
// genuinely needs the real pandoc opts into it explicitly via realToolsPATH.
func hermeticPATH(fakeBin string) string {
	dirs := []string{"/usr/bin", "/bin", "/sbin"}
	if fakeBin != "" {
		dirs = append([]string{fakeBin}, dirs...)
	}
	return "PATH=" + strings.Join(dirs, ":")
}

func realToolsPATH(fakeBin string) string {
	dirs := []string{"/usr/bin", "/bin", "/sbin", "/opt/homebrew/bin"}
	if fakeBin != "" {
		dirs = append([]string{fakeBin}, dirs...)
	}
	return "PATH=" + strings.Join(dirs, ":")
}

// isolatedHome returns a fresh, empty directory to use as HOME, so a real
// ~/.gitconfig, ~/.netrc or any other ambient per-user state on the test
// host is never consulted by the script's own git/uv/pandoc invocations.
func isolatedHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// hermeticEnv builds the complete, explicit environment for one subprocess
// invocation -- PATH, an isolated HOME, a fixed TZ/LC_ALL, and no inherited
// git config or hooks (GIT_CONFIG_GLOBAL/GIT_CONFIG_NOSYSTEM point git away
// from any real global/system config, per Council's hermeticity finding),
// plus whatever test-specific extras are given. Never os.Environ().
func hermeticEnv(t *testing.T, fakeBin string, extra ...string) []string {
	t.Helper()
	env := []string{
		hermeticPATH(fakeBin),
		"HOME=" + isolatedHome(t),
		"TZ=UTC",
		"LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=track3-preflight-test",
		"GIT_AUTHOR_EMAIL=track3-preflight-test@example.invalid",
		"GIT_COMMITTER_NAME=track3-preflight-test",
		"GIT_COMMITTER_EMAIL=track3-preflight-test@example.invalid",
	}
	return append(env, extra...)
}

func hermeticEnvRealTools(t *testing.T, fakeBin string, extra ...string) []string {
	t.Helper()
	env := hermeticEnv(t, fakeBin, extra...)
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = realToolsPATH(fakeBin)
		}
	}
	return env
}

// gitFixture runs a real, hermetic `git` (never the test runner's own
// inherited config) inside dir.
func gitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hermeticEnv(t, "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// zeocoreFixture returns a minimal, self-contained, REAL git repository
// shaped like enough of zeroemployeeorg/zeocore for the preflight script's
// own HEAD-relative reads: its pandoc floor
// (src/zeo_core/integrations/pandoc/_runner.py), its CI pin
// (tools/install-pandoc.sh) and its Python requirement (pyproject.toml). The
// script now reads these via `git show HEAD:<path>`, not the working tree,
// so the fixture must be a real commit, not just files on disk. Returns the
// repo root and its HEAD commit SHA.
func zeocoreFixture(t *testing.T, ciPin string) (root, head string) {
	t.Helper()
	root = t.TempDir()
	runner := filepath.Join(root, "src", "zeo_core", "integrations", "pandoc", "_runner.py")
	if err := os.MkdirAll(filepath.Dir(runner), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runner, []byte("MIN_PANDOC_VERSION: Final = (2, 15)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	toolsDir := filepath.Join(root, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "install-pandoc.sh"), []byte("VERSION=\""+ciPin+"\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("requires-python = \">=3.14\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "init", "-q")
	gitFixture(t, root, "add", "-A")
	gitFixture(t, root, "commit", "-q", "-m", "init")
	head = strings.TrimSpace(gitFixture(t, root, "rev-parse", "HEAD"))
	return root, head
}

// runPreflightEnv invokes the real script as a subprocess with a fully
// explicit, hermetic environment (never os.Environ()), returning its
// combined output and the process's own error (nil on exit 0). Always via
// `bash` -- Council's review of 6f0f91e: the script now requires bash (a
// plain POSIX sh, e.g. the real /bin/sh on most Linux hosts, silently
// disables the job control bounded_run's process-group primitive depends
// on), and the script itself now refuses to run under anything else.
func runPreflightEnv(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	return runPreflightEnvDir(t, env, "", args...)
}

// runPreflightEnvDir is runPreflightEnv with an explicit process CWD --
// used only by the test that needs the script's own $PWD to sit inside a
// fixture directory (pomar's review: a path component containing glob
// metacharacters is pathname-expanded against the SCRIPT'S CWD, not
// against any path-relative context, so reproducing that bug needs
// control over the real subprocess working directory, not just its args).
func runPreflightEnvDir(t *testing.T, env []string, dir string, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("track3-preflight.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runPreflight is the common case: no fixture fakes, just the hermetic
// system PATH and the real repo/scratch/jobTarget/head arguments. head is
// now a required 4th positional argument to the script itself (Council's
// review of 6f0f91e: an omitted expected revision used to report
// same_revision_status: NOT-PROVIDED and that status silently PASSED the
// overall gate), so every caller must supply one -- almost always the
// fixture's own real HEAD, from zeocoreFixture's second return value.
func runPreflight(t *testing.T, repo, scratch, jobTarget, head string) (string, error) {
	t.Helper()
	return runPreflightEnv(t, hermeticEnv(t, ""), repo, scratch, jobTarget, head)
}

func mustRunOrGateFail(t *testing.T, out string, err error) {
	t.Helper()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("script failed to run at all: %v\n%s", err, out)
		}
	}
}

func TestTrack3PreflightReportsThreeDistinctPandocValues(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	out, err := runPreflight(t, repo, t.TempDir(), "3.11", head)
	// This fixture's job target (3.11) is below the real installed pandoc on
	// this host in common cases and above it in others -- the overall gate
	// may legitimately be NOT-VERIFIED (nonzero exit) either way; this test
	// only asserts the three-value report shape, not the gate outcome.
	mustRunOrGateFail(t, out, err)
	wantLine(t, out, "pandoc floor (code minimum, from HEAD:", "pandoc floor (code minimum, from HEAD:src/zeo_core/integrations/pandoc/_runner.py): 2.15")
	wantLine(t, out, "pandoc CI pin (from HEAD:", "pandoc CI pin (from HEAD:tools/install-pandoc.sh): 3.1.3")
	wantLine(t, out, "pandoc job target", "pandoc job target (council/Architect operational policy, not a repo fact): 3.11")
}

// council's settlement (msg-architect-architect-pandoc-settled-2156): the
// installed pandoc is checked against the JOB TARGET, never the repo's own
// CI pin -- a CI pin lower than the job target must not read as satisfied.
// This is the one test that deliberately opts into the real pandoc on PATH.
func TestTrack3PreflightChecksInstalledAgainstJobTargetNotCIPin(t *testing.T) {
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("no pandoc on PATH to exercise the real version check")
	}
	repo, head := zeocoreFixture(t, "1.0.0") // a CI pin an installed pandoc would trivially satisfy
	out, err := runPreflightEnv(t, hermeticEnvRealTools(t, ""), repo, t.TempDir(), "999.0.0", head)
	if err == nil {
		t.Fatalf("an unmeetable job target must fail the overall gate (nonzero exit): %s", out)
	}
	// Pins the STATUS token exactly, not the whole line (the installed
	// pandoc version varies by host): must both start with the expected
	// status and end with the fixed trailer text.
	line := mustLine(t, out, "pandoc_status:")
	if !strings.HasPrefix(line, "pandoc_status: NOT-CONFIGURED") {
		t.Fatalf("an unmeetable job target must not read as satisfied just because the CI pin is low: %s", line)
	}
	if !strings.HasSuffix(line, "does-not-satisfy)") {
		t.Fatalf("expected the does-not-satisfy trailer: %s", line)
	}
}

func mustLine(t *testing.T, out, prefix string) string {
	t.Helper()
	line, ok := lineWithPrefix(out, prefix)
	if !ok {
		t.Fatalf("no line starting with %q in output:\n%s", prefix, out)
	}
	return line
}

// Council P2: a pandoc version must be validated as strictly numeric-dotted
// (^[0-9]+(\.[0-9]+)*$) before it is ever handed to `sort -V` -- a
// non-numeric installed_version (as a real `pandoc --version` could in
// principle emit from an unexpected build, and as this fake deliberately
// does) must read as UNKNOWN, never silently compared.
func TestTrack3PreflightTreatsNonNumericPandocVersionAsUnknown(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "pandoc"), []byte("#!/bin/sh\necho 'pandoc dev-build-not-a-version'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, filepath.Join(root, "scratch"), "3.11", head)
	if err == nil {
		t.Fatalf("an unknown pandoc version must not pass the overall gate: %s", out)
	}
	wantLineSuffix(t, out, "pandoc_status:", "does-not-satisfy)")
	if !strings.HasPrefix(mustLine(t, out, "pandoc_status:"), "pandoc_status: UNKNOWN") {
		t.Fatalf("a non-numeric-dotted installed version must read UNKNOWN, not silently compared: %s", out)
	}
}

func TestTrack3PreflightScratchAncestry(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")

	t.Run("safe: sticky ancestor is accepted even though world-writable", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "sticky")
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
			t.Fatal(err)
		}
		out, _ := runPreflight(t, repo, filepath.Join(dir, "future-leaf"), "3.11", head)
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: VERIFIED") {
			t.Fatalf("a sticky, world-writable ancestor (the /tmp pattern) must be accepted: %s", out)
		}
	})

	t.Run("unsafe: world-writable ancestor without the sticky bit is refused", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "world-writable")
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		out, err := runPreflight(t, repo, filepath.Join(dir, "future-leaf"), "3.11", head)
		if err == nil {
			t.Fatalf("an unsafe ancestor must fail the overall gate: %s", out)
		}
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: NOT-CONFIGURED") {
			t.Fatalf("a world-writable, non-sticky ancestor must be refused: %s", out)
		}
	})

	// pomar's review of pr#142: group-writable without the sticky bit (the
	// safe-ancestor predicate examines mode&0022, group OR other) was
	// silently accepted -- only "others" (not "group") was ever checked.
	t.Run("unsafe: group-writable ancestor without the sticky bit is refused (pomar's finding)", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "group-writable")
		if err := os.Mkdir(dir, 0o770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o770); err != nil {
			t.Fatal(err)
		}
		out, _ := runPreflight(t, repo, filepath.Join(dir, "future-leaf"), "3.11", head)
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: NOT-CONFIGURED") {
			t.Fatalf("a group-writable, non-sticky ancestor must be refused, not silently accepted: %s", out)
		}
	})

	// Council: tests were missing for an unsafe GRANDPARENT -- the immediate
	// (nearest existing) ancestor is safe, but the walk must continue
	// upward and still refuse an unsafe ancestor further up the chain, not
	// stop at the first one it finds.
	t.Run("unsafe: an unsafe grandparent is refused even though the immediate parent is safe", func(t *testing.T) {
		root := t.TempDir()
		grandparent := filepath.Join(root, "unsafe-grandparent")
		if err := os.Mkdir(grandparent, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(grandparent, 0o777); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(grandparent, "safe-parent")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := runPreflight(t, repo, filepath.Join(parent, "future-leaf"), "3.11", head)
		if err == nil {
			t.Fatalf("an unsafe grandparent must fail the overall gate: %s", out)
		}
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: NOT-CONFIGURED") {
			t.Fatalf("an unsafe grandparent must be refused even with a safe immediate parent: %s", out)
		}
		if !strings.Contains(mustLine(t, out, "scratch_ancestry_status:"), grandparent) {
			t.Fatalf("the refusal should name the actual unsafe ancestor (the grandparent), got: %s", mustLine(t, out, "scratch_ancestry_status:"))
		}
	})

	// Council: tests were missing for a symlinked ancestor -- the walk must
	// canonicalize a symlink to its real target and check THAT target's
	// permissions, not the symlink's own (irrelevant) mode.
	t.Run("a symlinked ancestor is resolved to its real target's permissions", func(t *testing.T) {
		root := t.TempDir()
		realTarget := filepath.Join(root, "real-unsafe-target")
		if err := os.Mkdir(realTarget, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(realTarget, 0o777); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(root, "link-parent")
		if err := os.Mkdir(linkDir, 0o755); err != nil {
			t.Fatal(err)
		}
		symlink := filepath.Join(linkDir, "ancestor-link")
		if err := os.Symlink(realTarget, symlink); err != nil {
			t.Fatal(err)
		}
		out, err := runPreflight(t, repo, filepath.Join(symlink, "future-leaf"), "3.11", head)
		if err == nil {
			t.Fatalf("a symlinked ancestor resolving to an unsafe real target must fail the overall gate: %s", out)
		}
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: NOT-CONFIGURED") {
			t.Fatalf("a symlinked ancestor must be resolved to its real target and checked there: %s", out)
		}
	})

	t.Run("nonexistent leaf still checks the real existing ancestor chain", func(t *testing.T) {
		root := t.TempDir()
		out, err := runPreflight(t, repo, filepath.Join(root, "not-created-yet"), "3.11", head)
		mustRunOrGateFail(t, out, err)
		wantLine(t, out, "scratch_path:", mustLine(t, out, "scratch_path:"))
		if !strings.Contains(mustLine(t, out, "scratch_path:"), "leaf exists: NO") {
			t.Fatalf("expected leaf exists: NO: %s", out)
		}
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: VERIFIED") {
			t.Fatalf("a safe, existing ancestor with a not-yet-created leaf must still verify: %s", out)
		}
	})

	// Council P3: a nonexistent leaf suffix containing `..` must be
	// lexically canonicalized before the ancestor walk starts, or the walk
	// could be mis-anchored relative to the path's real meaning.
	t.Run("nonexistent leaf suffix containing .. is canonicalized before the walk", func(t *testing.T) {
		root := t.TempDir()
		safeDir := filepath.Join(root, "safe-ancestor")
		if err := os.Mkdir(safeDir, 0755); err != nil {
			t.Fatal(err)
		}
		// safe-ancestor/decoy/../future-leaf lexically resolves to
		// safe-ancestor/future-leaf; "decoy" itself never exists.
		scratch := filepath.Join(safeDir, "decoy", "..", "future-leaf")
		out, err := runPreflight(t, repo, scratch, "3.11", head)
		mustRunOrGateFail(t, out, err)
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: VERIFIED") {
			t.Fatalf("a .. in a nonexistent leaf suffix must be canonicalized, reaching the real safe ancestor: %s", out)
		}
	})

	// zeoreviewer's review (P158-1 remainder): safe/missing/../link/../future,
	// where "missing" never exists -- its own ".." pops it right back off,
	// landing on "safe" again, which DOES exist. The existing real symlinked
	// component "link" (-> unsafe/child) that follows must then be resolved
	// through the real filesystem, not lexically: a buggy implementation
	// that stays in "nonexistent suffix" mode past that point would walk
	// "link" as a plain lexical name and land back on safe/future, silently
	// verifying through the WRONG, safe ancestor instead of the real one
	// (unsafe, reached by actually following the symlink and its "..").
	t.Run("a .. cancelling a nonexistent component resumes real resolution for what follows", func(t *testing.T) {
		root := t.TempDir()
		safeDir := filepath.Join(root, "safe")
		if err := os.Mkdir(safeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		unsafeDir := filepath.Join(root, "unsafe")
		if err := os.Mkdir(unsafeDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unsafeDir, 0o777); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(unsafeDir, "child")
		if err := os.Mkdir(child, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(safeDir, "link")
		if err := os.Symlink(child, link); err != nil {
			t.Fatal(err)
		}
		// Built by concatenation, not filepath.Join/Clean, which would
		// collapse the ".." itself before the script ever saw it.
		scratch := safeDir + "/missing/../link/../future"
		out, err := runPreflight(t, repo, scratch, "3.11", head)
		if err == nil {
			t.Fatalf("a .. cancelling a nonexistent component, followed by a real symlink whose own real parent is unsafe, must fail the overall gate: %s", out)
		}
		line := mustLine(t, out, "scratch_ancestry_status:")
		if !strings.HasPrefix(line, "scratch_ancestry_status: NOT-CONFIGURED") {
			t.Fatalf("must be refused via the real (not lexical) resolution once past the cancelled component: %s", out)
		}
		if !strings.Contains(line, unsafeDir) {
			t.Fatalf("the refusal should name the real unsafe ancestor (unsafe), not the lexical decoy (safe): %s", line)
		}
	})

	// zeoreviewer's review of the above fix: this script's own `set -e` does
	// not propagate through a FAILING assignment made inside a nested
	// command substitution -- lexically_canonical's explicit `return 1` (on
	// a failed cd -P, e.g. ".." through an existing REGULAR FILE, which is
	// not a traversable directory) is only reachable via
	// `scratch_lexical=$(lexically_canonical ...)`, itself already inside
	// `report=$(preflight)`. Confirmed directly: that nesting silently
	// swallows the failure, leaving scratch_lexical empty, which then
	// empties the ancestor walk entirely and leaves scratch_ancestry_status
	// at its initialized VERIFIED -- never actually walking anything. The
	// caller must check the assignment's own exit status explicitly.
	t.Run("a failed lexical resolution (.. through an existing regular file) never leaves scratch ancestry VERIFIED", func(t *testing.T) {
		root := t.TempDir()
		existingFile := filepath.Join(root, "existing-file")
		if err := os.WriteFile(existingFile, []byte("not a directory"), 0644); err != nil {
			t.Fatal(err)
		}
		// Built by concatenation, not filepath.Join/Clean, which would
		// collapse the ".." itself before the script ever saw it.
		// existing-file/.. cannot be traversed for real: existing-file is a
		// regular file, not a directory, so cd -P into it must fail.
		scratch := existingFile + "/../future"
		out, err := runPreflight(t, repo, scratch, "3.11", head)
		if err == nil {
			t.Fatalf("a failed lexical resolution must fail the overall gate, never silently verify: %s", out)
		}
		line := mustLine(t, out, "scratch_ancestry_status:")
		if strings.HasPrefix(line, "scratch_ancestry_status: VERIFIED") {
			t.Fatalf("a failed lexical resolution must never report scratch_ancestry_status: VERIFIED: %s", out)
		}
	})

	// pomar's review (P158-PATH-COMPONENT-GLOB-1): `set -- $p` with $p
	// unquoted also pathname-expands each split component, since the
	// literal path's own ".." components are themselves part of the SAME
	// unquoted word list -- a component containing glob metacharacters
	// (here "pattern[12]", a perfectly valid POSIX filename) can be
	// silently rewritten to whatever matches that glob in the SCRIPT'S OWN
	// CWD, unrelated to the real path. Reproduced with the script's CWD set
	// to safeDir itself (where an ordinary "pattern1" directory exists):
	// the literal symlink "pattern[12]" (-> unsafe/child, UNSAFE) must
	// still be walked as itself, not rewritten to the harmless "pattern1".
	t.Run("a path component containing glob metacharacters is never pathname-expanded against the caller's CWD", func(t *testing.T) {
		root := t.TempDir()
		safeDir := filepath.Join(root, "safe")
		if err := os.Mkdir(safeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// An ordinary, harmless directory whose name happens to match the
		// glob pattern below -- the decoy the bug would substitute in.
		if err := os.Mkdir(filepath.Join(safeDir, "pattern1"), 0o755); err != nil {
			t.Fatal(err)
		}
		unsafeDir := filepath.Join(root, "unsafe")
		if err := os.Mkdir(unsafeDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unsafeDir, 0o777); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(unsafeDir, "child")
		if err := os.Mkdir(child, 0o755); err != nil {
			t.Fatal(err)
		}
		// The literal filename is "pattern[12]", containing real glob
		// metacharacters -- a symlink to the unsafe child.
		globLink := filepath.Join(safeDir, "pattern[12]")
		if err := os.Symlink(child, globLink); err != nil {
			t.Fatal(err)
		}
		// Built by concatenation, not filepath.Join/Clean.
		scratch := globLink + "/../future"
		out, err := runPreflightEnvDir(t, hermeticEnv(t, ""), safeDir, repo, scratch, "3.11", head)
		if err == nil {
			t.Fatalf("the literal glob-named symlink's real unsafe target must fail the overall gate, not be rewritten to the harmless decoy: %s", out)
		}
		line := mustLine(t, out, "scratch_ancestry_status:")
		if !strings.HasPrefix(line, "scratch_ancestry_status: NOT-CONFIGURED") {
			t.Fatalf("must be refused via the literal component, not a CWD-glob-rewritten one: %s", out)
		}
		if !strings.Contains(line, unsafeDir) {
			t.Fatalf("the refusal should name the real unsafe ancestor (unsafe), not the CWD-matched decoy (safe/pattern1): %s", line)
		}
	})

	// pomar's own follow-up: the prior case proves the UNSAFE literal name
	// is never silently masked, but not that a SAFE literal glob-named
	// ancestor is still correctly recognized as itself and verified --
	// `set -f` must not change which real filesystem object the literal
	// component names, only stop that name from being reinterpreted as a
	// pattern.
	t.Run("a safe ancestor literally named with glob metacharacters still verifies correctly", func(t *testing.T) {
		root := t.TempDir()
		safeGlobNamed := filepath.Join(root, "safe[1]")
		if err := os.Mkdir(safeGlobNamed, 0o755); err != nil {
			t.Fatal(err)
		}
		// Built by concatenation, not filepath.Join/Clean.
		scratch := safeGlobNamed + "/future-leaf"
		out, err := runPreflight(t, repo, scratch, "3.11", head)
		mustRunOrGateFail(t, out, err)
		if !strings.HasPrefix(mustLine(t, out, "scratch_ancestry_status:"), "scratch_ancestry_status: VERIFIED") {
			t.Fatalf("a safe ancestor literally named with glob metacharacters must still verify: %s", out)
		}
	})
}

// pomar's review of pr#142: when stat itself is unavailable, the permission
// string must never silently read as safe -- and Council's distinction:
// this is UNKNOWN (we could not determine), not NOT-CONFIGURED (we
// positively determined it's unsafe) -- both fail the ancestor closed, but
// the label must say which actually happened.
func TestTrack3PreflightFailsClosedWhenStatIsUnavailable(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "stat"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}

	out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, filepath.Join(root, "future-leaf"), "3.11", head)
	if err == nil {
		t.Fatalf("an unreadable ancestor must fail the overall gate: %s", out)
	}
	line := mustLine(t, out, "scratch_ancestry_status:")
	if !strings.HasPrefix(line, "scratch_ancestry_status: UNKNOWN") || !strings.Contains(line, "could not read permission metadata") {
		t.Fatalf("a failed stat must fail the ancestor closed as UNKNOWN, not read VERIFIED or NOT-CONFIGURED: %s", line)
	}
}

// pomar's re-review of pr#142 round 2: a stat that succeeds (exit 0) but
// emits a malformed, non-permission 10-byte string (their counterexample:
// "??????????") previously passed the old length-only check and read as
// VERIFIED. The actual ls-style grammar must be validated, not just length.
func TestTrack3PreflightFailsClosedOnMalformedTenByteStat(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "stat"), []byte("#!/bin/sh\nprintf '??????????'\n"), 0755); err != nil {
		t.Fatal(err)
	}

	out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, filepath.Join(root, "future-leaf"), "3.11", head)
	if err == nil {
		t.Fatalf("a malformed stat must fail the overall gate: %s", out)
	}
	line := mustLine(t, out, "scratch_ancestry_status:")
	if !strings.HasPrefix(line, "scratch_ancestry_status: UNKNOWN") || !strings.Contains(line, "could not read permission metadata") {
		t.Fatalf("a malformed 10-byte stat output must fail the ancestor closed as UNKNOWN: %s", line)
	}
}

// fakeUVScript builds a fake `uv` whose `lock` subcommand records its own
// environment to envFile (one KEY=VALUE per line) before acting out
// `behavior` (a shell fragment), so a test can assert exactly what the real
// script passed it -- in particular UV_OFFLINE/UV_PYTHON_DOWNLOADS/
// UV_CACHE_DIR, which Council's P1 requires on every invocation.
func fakeUVScript(envFile, behavior string) string {
	return "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --version) echo 'uv 0.0.0-fake' ;;\n" +
		"  lock) env > \"" + envFile + "\"; " + behavior + " ;;\n" +
		"esac\n"
}

// Council P1: the must-fix-first finding. `uv lock --check` must run with
// UV_OFFLINE=1 and UV_PYTHON_DOWNLOADS=never, plus a private UV_CACHE_DIR,
// so this "read-only" script genuinely cannot reach the network, trigger a
// Python download, or write into any shared/real uv cache.
func TestTrack3PreflightUvLockCheckRunsOfflineWithPrivateCache(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, "uv-env.txt")
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUVScript(envFile, "true")), 0755); err != nil {
		t.Fatal(err)
	}
	// This fixture's job target (3.11) may or may not be satisfied by the
	// real pandoc on this host -- irrelevant to what this test checks (the
	// env uv's lock subcommand actually received), so only a hard run
	// failure (not merely an unverified gate) is fatal here.
	out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, t.TempDir(), "3.11", head)
	mustRunOrGateFail(t, out, err)

	recorded, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("fake uv's lock subcommand never ran, or never recorded its env: %v", err)
	}
	env := string(recorded)
	checks := map[string]string{
		"UV_OFFLINE":          "1",
		"UV_PYTHON_DOWNLOADS": "never",
	}
	for k, want := range checks {
		line := k + "=" + want
		if !strings.Contains(env, line) {
			t.Fatalf("expected %q in uv's own environment, got:\n%s", line, env)
		}
	}
	cacheLine := ""
	for _, l := range strings.Split(env, "\n") {
		if strings.HasPrefix(l, "UV_CACHE_DIR=") {
			cacheLine = l
		}
	}
	if cacheLine == "" {
		t.Fatalf("expected a UV_CACHE_DIR to be set for uv, got none in:\n%s", env)
	}
	cacheDir := strings.TrimPrefix(cacheLine, "UV_CACHE_DIR=")
	if strings.HasPrefix(cacheDir, repo) {
		t.Fatalf("UV_CACHE_DIR must not be inside the read-only repo: %s", cacheDir)
	}
	if _, err := os.Stat(cacheDir); err == nil {
		t.Fatalf("the private UV_CACHE_DIR (%s) must be cleaned up after the script exits, not left behind", cacheDir)
	}
}

// Council's review of 6f0f91e (item 3): `uv lock --check` must validate a
// clean HEAD snapshot, not the repo's own working tree -- an uncommitted
// edit to pyproject.toml must not change whether the lock check passes.
func TestTrack3PreflightUvLockCheckReadsHeadSnapshotNotWorkingTree(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	// Records the directory it was actually invoked with, AND a copy of its
	// pyproject.toml content, at invocation time -- the real headsnap
	// directory is created under the script's own scratch_tmp and is
	// removed by its EXIT trap before this process returns, so inspecting
	// it from the test AFTER runPreflightEnv returns (an earlier version of
	// this test did exactly that) always finds it already gone. The fake
	// uv itself must capture what it saw, while it's still there to see.
	lockDirFile := filepath.Join(root, "lock-dir.txt")
	snapContentFile := filepath.Join(root, "snap-pyproject.txt")
	recordDirScript := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --version) echo 'uv 0.0.0-fake' ;;\n" +
		"  lock)\n" +
		"    shift\n" +
		"    while [ $# -gt 0 ]; do\n" +
		"      if [ \"$1\" = --directory ]; then\n" +
		"        echo \"$2\" > \"" + lockDirFile + "\"\n" +
		"        cat \"$2/pyproject.toml\" > \"" + snapContentFile + "\" 2>&1\n" +
		"      fi\n" +
		"      shift\n" +
		"    done\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(recordDirScript), 0755); err != nil {
		t.Fatal(err)
	}
	// Dirty the working tree's pyproject.toml -- the lock check must still
	// run against HEAD's own version, which this fake doesn't even inspect
	// the content of, but the --directory it's handed must not be the real
	// repo (an uncommitted edit there would otherwise be visible to it).
	if err := os.WriteFile(filepath.Join(repo, "pyproject.toml"), []byte("requires-python = \">=9.99\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, t.TempDir(), "3.11", head)
	mustRunOrGateFail(t, out, err)

	dirBytes, err := os.ReadFile(lockDirFile)
	if err != nil {
		t.Fatalf("the fake uv never recorded its --directory argument: %v", err)
	}
	lockDir := strings.TrimSpace(string(dirBytes))
	if lockDir == repo {
		t.Fatalf("uv lock --check was run directly against the working tree (%s), not a HEAD snapshot", repo)
	}
	snapContent, err := os.ReadFile(snapContentFile)
	if err != nil {
		t.Fatalf("the fake uv never captured the snapshot's pyproject.toml content: %v", err)
	}
	if strings.Contains(string(snapContent), "9.99") {
		t.Fatalf("the snapshot handed to uv contains the dirty working-tree edit, not HEAD's own content: %s", snapContent)
	}
	if !strings.Contains(string(snapContent), "3.14") {
		t.Fatalf("the snapshot handed to uv should contain HEAD's real requires-python (3.14): %s", snapContent)
	}
}

// Three distinct lock outcomes Council listed as missing: a real VERIFIED
// (rc 0), a real NOT-CONFIGURED (nonzero, not a timeout), and an UNKNOWN
// (uv genuinely absent from PATH).
func TestTrack3PreflightLockConsistencyOutcomes(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")

	t.Run("VERIFIED: fake uv lock --check exits 0", func(t *testing.T) {
		root := t.TempDir()
		fakeBin := filepath.Join(root, "bin")
		if err := os.Mkdir(fakeBin, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUVScript(filepath.Join(root, "env.txt"), "exit 0")), 0755); err != nil {
			t.Fatal(err)
		}
		out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, t.TempDir(), "3.11", head)
		mustRunOrGateFail(t, out, err)
		line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
		if !strings.HasSuffix(line, ": VERIFIED") {
			t.Fatalf("expected a VERIFIED lock-consistency line, got: %s", line)
		}
	})

	t.Run("NOT-CONFIGURED: fake uv lock --check exits nonzero, not a timeout", func(t *testing.T) {
		root := t.TempDir()
		fakeBin := filepath.Join(root, "bin")
		if err := os.Mkdir(fakeBin, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUVScript(filepath.Join(root, "env.txt"), "exit 1")), 0755); err != nil {
			t.Fatal(err)
		}
		out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, t.TempDir(), "3.11", head)
		if err == nil {
			t.Fatalf("an inconsistent lock must fail the overall gate: %s", out)
		}
		line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
		if !strings.HasSuffix(line, ": NOT-CONFIGURED") {
			t.Fatalf("expected a NOT-CONFIGURED lock-consistency line, got: %s", line)
		}
	})

	t.Run("UNKNOWN: uv genuinely absent from PATH", func(t *testing.T) {
		out, err := runPreflightEnv(t, hermeticEnv(t, ""), repo, t.TempDir(), "3.11", head)
		if err == nil {
			t.Fatalf("a missing uv must fail the overall gate: %s", out)
		}
		if !strings.HasPrefix(mustLine(t, out, "uv on PATH:"), "uv on PATH: DOES-NOT-EXIST") {
			t.Fatalf("expected uv reported absent: %s", out)
		}
		line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
		if !strings.Contains(line, ": UNKNOWN") {
			t.Fatalf("a missing uv must still emit an explicit UNKNOWN lock line, not omit it: %s", line)
		}
	})
}

// A council/pomar review: the uv lock--check call may consult the network
// or uv's own cache, so an unresponsive one must not hang a mandatory run
// indefinitely, and the child process must actually be killed, not merely
// abandoned. A fake `uv` that sleeps well past a short TRACK3_UV_TIMEOUT
// proves both: the script returns promptly, and the fake uv never reaches
// the point of writing its completion marker.
func TestTrack3PreflightUvLockCheckIsBoundedByTimeout(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "uv-finished")
	fakeUV := "#!/bin/sh\ncase \"$1\" in\n  --version) echo 'uv 0.0.0-fake' ;;\n  lock) sleep 5; : > \"$MARKER\" ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}

	env := hermeticEnv(t, fakeBin, "MARKER="+marker, "TRACK3_UV_TIMEOUT=1")
	start := time.Now()
	out, _ := runPreflightEnv(t, env, repo, filepath.Join(root, "scratch"), "3.11", head)
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Fatalf("script took %s, well over the ~1s timeout, want well under the fake uv's 5s sleep: %s", elapsed, out)
	}
	line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
	if !strings.Contains(line, "NOT-CONFIGURED (timed out after 1s, killed") {
		t.Fatalf("expected a timed-out, killed, NOT-CONFIGURED report: %s", line)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("fake uv's completion marker exists -- its child process was not actually killed before completing")
	}
}

// A command may handle the watchdog's TERM and exit zero. That is still a
// timeout, even if its rounded elapsed time equals the budget. Exit status
// and elapsed time alone must never turn this into a VERIFIED lock check.
func TestTrack3PreflightTermHandlerExitZeroIsStillTimeout(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "term-observed")
	fakeUV := "#!/bin/sh\ncase \"$1\" in\n --version) echo 'uv 0.0.0-fake' ;;\n lock) trap ': > \"$MARKER\"; exit 0' TERM; while :; do sleep 1; done ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}
	env := hermeticEnv(t, fakeBin, "MARKER="+marker, "TRACK3_UV_TIMEOUT=1")
	out, err := runPreflightEnv(t, env, repo, filepath.Join(root, "scratch"), "3.11", head)
	if err == nil {
		t.Fatalf("a command completing after watchdog TERM must fail the gate: %s", out)
	}
	line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
	if !strings.Contains(line, "NOT-CONFIGURED (timed out after 1s, completed on its own before termination was confirmed") {
		t.Fatalf("want a timeout without a false killed or VERIFIED claim: %s", line)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("TERM handler did not run: %v", err)
	}
}

// pomar's review of the previous fix (9098c0b8): "The present test only
// checks a cooperative shell and immediately absent marker; it does not
// prove later work cannot complete or descendants are gone." This fake uv
// explicitly ignores TERM (trap ” TERM), so the only way the script's
// bounded_run can actually stop it within budget is by escalating to the
// unmaskable KILL -- proving termination is genuinely bounded, not merely
// dependent on the child's cooperation, and that the script never falsely
// reports "killed" by trusting an ignorable signal alone.
func TestTrack3PreflightKillsATermIgnoringChildAndNeverClaimsFalseSuccess(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "uv-finished")
	fakeUV := "#!/bin/sh\ntrap '' TERM\ncase \"$1\" in\n  --version) echo 'uv 0.0.0-fake' ;;\n  lock) sleep 20; : > \"$MARKER\" ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}

	env := hermeticEnv(t, fakeBin, "MARKER="+marker, "TRACK3_UV_TIMEOUT=1")
	start := time.Now()
	out, _ := runPreflightEnv(t, env, repo, filepath.Join(root, "scratch"), "3.11", head)
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("script took %s, want well under the fake uv's 20s sleep (TERM-ignoring child must still be bounded): %s", elapsed, out)
	}
	line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
	if !strings.Contains(line, "NOT-CONFIGURED (timed out after 1s, killed") {
		t.Fatalf("expected a timed-out, killed, NOT-CONFIGURED report even though the child ignored TERM: %s", line)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("fake uv's completion marker exists -- a TERM-ignoring child was not actually force-killed before completing")
	}
}

// recordGrandchildPIDScript returns a fake uv `lock` body that backgrounds a
// genuinely separate process (`sh -c '...' &`, confirmed to report its own
// distinct pid via `$$` -- `( ... ) &` does NOT: confirmed directly on this
// shell that `$$` inside a backgrounded subshell still reports the PARENT's
// own pid, which silently makes a liveness check against it meaningless)
// and records that pid from the LEADER's own `$!` immediately after
// backgrounding it, before doing anything else. This must not rely on the
// grandchild's own script reaching its `echo $$ > pidfile` line: the real
// script's sweep now checks group membership via `kill -0` (a builtin,
// near-instant) rather than forking `ps`, and an earlier version of this
// helper that had the grandchild self-report its pid raced against that --
// confirmed directly, the sweep could (and, once kill -0 replaced ps -g,
// reproducibly did) kill the grandchild before it ever executed far enough
// to write its own pid file, failing the test for an unrelated timing
// reason having nothing to do with the sweep's actual correctness.
func recordGrandchildPIDScript(leaderTrapsTerm bool, leaderBehavior string) string {
	leaderTrap := ""
	if leaderTrapsTerm {
		leaderTrap = "trap '' TERM\n"
	}
	return "#!/bin/sh\n" +
		leaderTrap +
		"case \"$1\" in\n" +
		"  --version) echo 'uv 0.0.0-fake' ;;\n" +
		"  lock)\n" +
		"    sh -c 'trap \"\" TERM; sleep 20' &\n" +
		"    echo $! > \"$GRANDCHILD_PID_FILE\"\n" +
		"    " + leaderBehavior + "\n" +
		"    ;;\n" +
		"esac\n"
}

func assertGrandchildDead(t *testing.T, pidFile string) {
	t.Helper()
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the grandchild never recorded its own pid (never started?): %v", err)
	}
	var gpid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &gpid); err != nil {
		t.Fatalf("could not parse grandchild pid %q: %v", pidBytes, err)
	}
	if err := syscall.Kill(gpid, 0); err == nil {
		t.Fatalf("grandchild pid %d is still alive after the script returned", gpid)
	} else if err != syscall.ESRCH {
		t.Fatalf("unexpected error probing grandchild pid %d: %v", gpid, err)
	}
}

// Council P2: the watchdog must kill the WHOLE process group, not just the
// direct child -- a TERM-ignoring leader with its own backgrounded
// grandchild must lose both to the escalation. Checked by the grandchild's
// OWN liveness (syscall.Kill(pid, 0)) immediately after the script returns,
// not by waiting for a completion marker it would take the grandchild's
// full 20s sleep to write: an earlier version of this test used exactly
// that marker-file approach and, when mutant-verified by reverting the
// watchdog to a single-pid (non-group) kill, PASSED ANYWAY -- the window
// between the script returning (~6s in) and the marker's own 20s deadline
// was long enough for an orphaned-but-still-running grandchild to look
// indistinguishable from a killed one. Checking the pid directly removes
// that blind spot.
func TestTrack3PreflightKillsEntireProcessGroupIncludingGrandchildren(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	grandchildPIDFile := filepath.Join(root, "grandchild.pid")
	fakeUV := recordGrandchildPIDScript(true, "sleep 20")
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}

	env := hermeticEnv(t, fakeBin, "GRANDCHILD_PID_FILE="+grandchildPIDFile, "TRACK3_UV_TIMEOUT=1")
	start := time.Now()
	out, _ := runPreflightEnv(t, env, repo, filepath.Join(root, "scratch"), "3.11", head)
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("script took %s, want well under the 20s sleeps (the whole group must be bounded): %s", elapsed, out)
	}
	line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
	if !strings.Contains(line, "NOT-CONFIGURED (timed out after 1s, killed") {
		t.Fatalf("expected a timed-out, killed report: %s", line)
	}
	assertGrandchildDead(t, grandchildPIDFile)
}

// Council P2, finding N1: "KILL is never sent when the leader dies on TERM."
// Unlike the test above (where the LEADER itself also traps/ignores TERM,
// so `wait` only returns once the watchdog's own KILL eventually fires),
// this leader does NOT trap TERM at all -- it dies quickly, from the
// watchdog's own TERM, while its OWN backgrounded grandchild (which does
// trap TERM) would otherwise survive. The earlier bounded_run cancelled the
// watchdog's scheduled KILL step as soon as the leader was reaped, before
// that KILL step ever ran, so the grandchild was never actually killed.
func TestTrack3PreflightKillsSurvivingGrandchildWhenLeaderDiesQuicklyOnTerm(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	grandchildPIDFile := filepath.Join(root, "grandchild.pid")
	// No `trap '' TERM` on the leader itself -- it dies promptly when the
	// watchdog's TERM reaches the group; only its backgrounded grandchild
	// traps TERM and would otherwise outlive it.
	fakeUV := recordGrandchildPIDScript(false, "sleep 20")
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}

	env := hermeticEnv(t, fakeBin, "GRANDCHILD_PID_FILE="+grandchildPIDFile, "TRACK3_UV_TIMEOUT=1")
	start := time.Now()
	out, _ := runPreflightEnv(t, env, repo, filepath.Join(root, "scratch"), "3.11", head)
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("script took %s, want well under the grandchild's 20s sleep: %s", elapsed, out)
	}
	assertGrandchildDead(t, grandchildPIDFile)
}

// Council P2, finding N2: "leftovers after a normal exit are neither killed
// nor counted." This leader exits normally (rc 0), almost immediately, well
// within budget -- having left its own backgrounded grandchild running. The
// script must still sweep and kill that leftover, and must do so promptly
// (not by waiting out the full default budget).
func TestTrack3PreflightSweepsLeftoverAfterANormalFastExit(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	grandchildPIDFile := filepath.Join(root, "grandchild.pid")
	fakeUV := recordGrandchildPIDScript(false, "exit 0")
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}

	env := hermeticEnv(t, fakeBin, "GRANDCHILD_PID_FILE="+grandchildPIDFile)
	start := time.Now()
	out, err := runPreflightEnv(t, env, repo, t.TempDir(), "3.11", head)
	elapsed := time.Since(start)
	mustRunOrGateFail(t, out, err)
	// The leader itself exits in well under a second; the sweep's own grace
	// sleeps add at most ~2s. This must be nowhere near the grandchild's
	// 20s sleep or the default 30s uv timeout, proving the sweep runs
	// promptly rather than waiting for either.
	if elapsed > 10*time.Second {
		t.Fatalf("script took %s to sweep a leftover after a fast normal exit -- want well under 10s", elapsed)
	}
	line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
	if !strings.HasSuffix(line, ": VERIFIED") {
		t.Fatalf("the leader's own exit 0 should still report VERIFIED: %s", line)
	}
	assertGrandchildDead(t, grandchildPIDFile)
}

// Complementary to the above: a child that outlives TERM but happens to
// finish on its own, within the fixed TERM grace window and before KILL
// would otherwise fire, must be reported honestly -- it timed out (ran past
// budget) but was not actually killed by this check, and the script must
// say so rather than claim "killed" for a termination it didn't cause.
func TestTrack3PreflightReportsHonestlyWhenChildCompletesDuringTermGraceWithoutBeingKilled(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "uv-finished")
	// Ignores TERM, but finishes on its own in ~2s -- inside the fixed 5s
	// TERM grace window, so KILL should never need to fire.
	fakeUV := "#!/bin/sh\ntrap '' TERM\ncase \"$1\" in\n  --version) echo 'uv 0.0.0-fake' ;;\n  lock) sleep 2; : > \"$MARKER\"; exit 0 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}

	env := hermeticEnv(t, fakeBin, "MARKER="+marker, "TRACK3_UV_TIMEOUT=1")
	out, _ := runPreflightEnv(t, env, repo, filepath.Join(root, "scratch"), "3.11", head)
	if !strings.Contains(out, "completed on its own before termination was confirmed -- not killed by this check") {
		t.Fatalf("expected an honest not-killed-by-this-check report for a child that outlived TERM but finished before KILL: %s", out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the child's own marker should exist in this case (it really did complete on its own): %v", statErr)
	}
}

// Council P3: rc>=128 alone is not proof WE killed the child -- a process
// that crashes or self-signals well within budget can land in the exact
// same exit-status range. A fake uv that kills itself with SIGKILL almost
// immediately (budget never comes close to elapsing) must be reported as a
// plain failed completion, never as "timed out ... killed".
func TestTrack3PreflightDoesNotClaimKilledForAnInBudgetSelfCrash(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	fakeUV := "#!/bin/sh\ncase \"$1\" in\n  --version) echo 'uv 0.0.0-fake' ;;\n  lock) kill -KILL $$ ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte(fakeUV), 0755); err != nil {
		t.Fatal(err)
	}
	// A generous 30s budget (the default) that the near-instant self-kill
	// comes nowhere near -- if the script ever reports "killed" here, it can
	// only be from trusting the exit-status range alone, not real escalation
	// provenance. Must go through runPreflightEnv with fakeBin on PATH, not
	// a no-fakes convenience helper -- an earlier version of this test
	// called the latter by mistake, silently exercising the real uv instead
	// of this fake one, and passed regardless of the mutation under test
	// for that reason alone.
	out, _ := runPreflightEnv(t, hermeticEnv(t, fakeBin), repo, filepath.Join(root, "scratch"), "3.11", head)
	line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
	if !strings.HasSuffix(line, ": NOT-CONFIGURED") {
		t.Fatalf("expected a plain NOT-CONFIGURED report for a self-killed child: %s", line)
	}
	if strings.Contains(line, "timed out") || strings.Contains(line, "killed") {
		t.Fatalf("an in-budget self-crash must never be reported as a timeout/kill this check performed: %s", line)
	}
}

// pomar's review: TRACK3_UV_TIMEOUT must be validated as a positive, bounded
// integer -- not trusted blindly, which would otherwise make the arithmetic
// comparisons error out (non-numeric) or let an absurd value function as
// "no timeout" in practice.
func TestTrack3PreflightRejectsInvalidUvTimeout(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "uv"), []byte("#!/bin/sh\necho 'uv 0.0.0-fake'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"0", "-1", "not-a-number", "999999"} {
		t.Run(bad, func(t *testing.T) {
			env := hermeticEnv(t, fakeBin, "TRACK3_UV_TIMEOUT="+bad)
			out, err := runPreflightEnv(t, env, repo, filepath.Join(root, "scratch-"+bad), "3.11", head)
			if err == nil {
				t.Fatalf("an invalid timeout must fail the overall gate: %s", out)
			}
			if !strings.Contains(out, "TRACK3_UV_TIMEOUT invalid") {
				t.Fatalf("expected an explicit invalid-timeout report for %q: %s", bad, out)
			}
			line := mustLine(t, out, "uv_lock_consistency_with_pyproject")
			if !strings.Contains(line, ": UNKNOWN") {
				t.Fatalf("an invalid timeout must still emit an explicit UNKNOWN lock line, not silently drop it: %s", line)
			}
		})
	}
}

// Council: the repository-root check must refuse upward-search
// misattribution -- a path that is merely a SUBDIRECTORY of a real git
// repository (not the repo's own top level) must never have its facts
// reported as if they were about the requested path.
func TestTrack3PreflightRefusesSubdirectoryOfARealRepoAsMisattribution(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	subdir := filepath.Join(repo, "src")
	out, err := runPreflight(t, subdir, t.TempDir(), "3.11", head)
	if err == nil {
		t.Fatalf("a subdirectory, not the repo's own top level, must fail the overall gate: %s", out)
	}
	if !strings.HasPrefix(mustLine(t, out, "repo_root_status:"), "repo_root_status: NOT-CONFIGURED") {
		t.Fatalf("a requested path that is only a subdirectory of the real repo root must be refused: %s", out)
	}
	if strings.Contains(out, "pandoc floor (code minimum, from HEAD:src/zeo_core/integrations/pandoc/_runner.py): 2.15") {
		t.Fatalf("misattributed repo-root facts must not be reported as if verified: %s", out)
	}
}

func TestTrack3PreflightSameRevisionCheck(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")

	t.Run("the expected-revision argument is required, not optional", func(t *testing.T) {
		script, err := filepath.Abs("track3-preflight.sh")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script, repo, t.TempDir(), "3.11")
		cmd.Env = hermeticEnv(t, "")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("omitting the expected-revision argument must be refused, not silently treated as NOT-PROVIDED: %s", out)
		}
		if !strings.Contains(string(out), "usage:") {
			t.Fatalf("expected a usage refusal for a missing expected-revision argument: %s", out)
		}
	})

	t.Run("MATCH when the expected revision is the real HEAD", func(t *testing.T) {
		out, err := runPreflightEnv(t, hermeticEnv(t, ""), repo, t.TempDir(), "3.11", head)
		mustRunOrGateFail(t, out, err)
		wantLine(t, out, "same_revision_status:", "same_revision_status: MATCH (expected "+head+")")
	})

	t.Run("MISMATCH when the expected revision is a different, valid SHA", func(t *testing.T) {
		other := strings.Repeat("a", 40)
		if other == head {
			other = strings.Repeat("b", 40)
		}
		out, err := runPreflightEnv(t, hermeticEnv(t, ""), repo, t.TempDir(), "3.11", other)
		if err == nil {
			t.Fatalf("a revision mismatch must fail the overall gate: %s", out)
		}
		wantLine(t, out, "same_revision_status:", "same_revision_status: MISMATCH (expected "+other+")")
	})

	t.Run("a malformed expected revision is refused at the usage level", func(t *testing.T) {
		out, err := runPreflightEnv(t, hermeticEnv(t, ""), repo, t.TempDir(), "3.11", "not-a-sha")
		if err == nil {
			t.Fatalf("a malformed expected revision must be refused, not silently accepted: %s", out)
		}
		if !strings.Contains(out, "usage:") {
			t.Fatalf("expected a usage refusal for a malformed expected-revision argument: %s", out)
		}
	})
}

// Council: dirty-working-tree state must be surfaced explicitly, and the
// repo-fact reads must come from HEAD regardless -- an uncommitted edit to
// the pandoc floor file must NOT change what this script reports.
func TestTrack3PreflightReadsHeadNotWorkingTreeAndReportsDirtyState(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	runnerPath := filepath.Join(repo, "src", "zeo_core", "integrations", "pandoc", "_runner.py")
	if err := os.WriteFile(runnerPath, []byte("MIN_PANDOC_VERSION: Final = (9, 99)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runPreflight(t, repo, t.TempDir(), "3.11", head)
	mustRunOrGateFail(t, out, err)
	if !strings.HasPrefix(mustLine(t, out, "working_tree_status:"), "working_tree_status: DIRTY") {
		t.Fatalf("expected the dirty working tree to be reported explicitly: %s", out)
	}
	wantLine(t, out, "pandoc floor (code minimum, from HEAD:", "pandoc floor (code minimum, from HEAD:src/zeo_core/integrations/pandoc/_runner.py): 2.15")
}

// Council: a test was missing for source_remote -- the actual configured
// git remote URL must be reported verbatim on its own line.
func TestTrack3PreflightReportsSourceRemote(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	const remoteURL = "https://example.invalid/zeroemployeeorg/zeocore.git"
	gitFixture(t, repo, "remote", "add", "origin", remoteURL)
	out, err := runPreflight(t, repo, t.TempDir(), "3.11", head)
	mustRunOrGateFail(t, out, err)
	wantLine(t, out, "zeocore source_remote:", "zeocore source_remote: "+remoteURL)
}

// The final receipt_sha256 line must be a real sha256 over the report text
// actually printed, computed after capture so the two can never drift.
func TestTrack3PreflightReceiptMatchesReportSha256(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	out, _ := runPreflight(t, repo, t.TempDir(), "3.11", head)

	idx := strings.Index(out, "receipt_sha256: ")
	if idx < 0 {
		t.Fatalf("missing receipt_sha256 line: %s", out)
	}
	reportText := out[:idx]
	receiptLine := strings.TrimSpace(out[idx:])
	wantHash := fmt.Sprintf("receipt_sha256: %x", sha256Sum(reportText))
	if receiptLine != wantHash {
		t.Fatalf("receipt does not match sha256 of the printed report:\nwant %s\ngot  %s", wantHash, receiptLine)
	}
}

func TestTrack3PreflightRejectsIncompleteArguments(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	script, err := filepath.Abs("track3-preflight.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{},
		{repo},
		{repo, t.TempDir()},             // missing the job target
		{repo, t.TempDir(), "3.11"},     // missing the (now required) expected-revision
		{repo, t.TempDir(), "3.11", ""}, // explicitly empty expected-revision
	} {
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Env = hermeticEnv(t, "")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("args %v: expected a usage refusal, got success: %s", args, out)
		}
		if !strings.Contains(string(out), "usage:") {
			t.Fatalf("args %v: expected a usage message, got: %s", args, out)
		}
	}
	_ = head
}

// Council finding (6): the script must declare and enforce its bash
// requirement rather than silently misbehave under a plain POSIX sh. Run
// directly under `dash` (the real `/bin/sh` on most Linux hosts, including
// this test host's own /bin/dash) and confirm an explicit, early refusal --
// never a silent attempt to proceed with a broken process-group primitive.
func TestTrack3PreflightRefusesToRunUnderNonBashShell(t *testing.T) {
	if _, err := exec.LookPath("dash"); err != nil {
		t.Skip("no dash on PATH to exercise the non-bash refusal")
	}
	repo, head := zeocoreFixture(t, "3.1.3")
	script, err := filepath.Abs("track3-preflight.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("dash", script, repo, t.TempDir(), "3.11", head)
	cmd.Env = hermeticEnv(t, "")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("running under dash must be refused, not silently succeed with a broken group-kill primitive: %s", out)
	}
	if !strings.Contains(string(out), "requires bash") {
		t.Fatalf("expected an explicit bash-requirement refusal under dash, got: %s", out)
	}
}

// zeroemployee's review (POMAR158 independent review, 2026-10-29): resolving
// ".." lexically for the WHOLE scratch path, including through an EXISTING
// ancestor that is itself a symlink, can mis-anchor the ancestor walk onto a
// directory that was never really an ancestor at all. Here the lexical
// parent of the symlink (decoy-parent) is SAFE, but the symlink's REAL
// target's real parent (unsafe-container) is UNSAFE -- a buggy lexical ".."
// resolution would silently verify (checking only decoy-parent), masking the
// actually-unsafe location.
func TestTrack3PreflightScratchAncestryDotDotThroughExistingSymlinkIsResolvedForReal(t *testing.T) {
	repo, head := zeocoreFixture(t, "3.1.3")
	root := t.TempDir()

	decoyParent := filepath.Join(root, "decoy-parent")
	if err := os.Mkdir(decoyParent, 0o755); err != nil {
		t.Fatal(err)
	}
	unsafeContainer := filepath.Join(root, "unsafe-container")
	if err := os.Mkdir(unsafeContainer, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeContainer, 0o777); err != nil {
		t.Fatal(err)
	}
	realTarget := filepath.Join(unsafeContainer, "real-target")
	if err := os.Mkdir(realTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(decoyParent, "ancestor-link")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Fatal(err)
	}

	// decoy-parent/ancestor-link/../future-leaf: lexically this undoes
	// "ancestor-link" and lands back at decoy-parent (SAFE) -- the bug.
	// Resolved for real, ancestor-link's target's real parent is
	// unsafe-container (UNSAFE) -- the correct answer. Built by plain
	// concatenation, NOT filepath.Join/Clean -- those would collapse the
	// ".." themselves before the script ever saw it, defeating this test.
	scratch := link + "/../future-leaf"
	out, err := runPreflight(t, repo, scratch, "3.11", head)
	if err == nil {
		t.Fatalf("a .. through an existing symlinked ancestor landing on an unsafe real parent must fail the overall gate: %s", out)
	}
	line := mustLine(t, out, "scratch_ancestry_status:")
	if !strings.HasPrefix(line, "scratch_ancestry_status: NOT-CONFIGURED") {
		t.Fatalf("must be refused via the real (not lexical) resolution: %s", out)
	}
	if !strings.Contains(line, unsafeContainer) {
		t.Fatalf("the refusal should name the real unsafe ancestor (unsafe-container), not the lexical decoy: %s", line)
	}
}

// zeroemployee's review (POMAR158 independent review, 2026-10-29): every
// repo-side fact must be read from the EXACT $source_head SHA captured
// earlier in the script's run, never from a second, later re-resolution of
// the mutable "HEAD" ref -- or a repository that moves between the capture
// and these later reads (a concurrent checkout or commit) could make the
// reported facts belong to a different revision than source_head names.
//
// This is exercised with a fake `git` on PATH that delegates to the real
// git for everything, except that its FIRST "-C <dir> rev-parse HEAD" call
// also, as a side effect, checks the fixture repo out to a SECOND, already-
// prepared commit before returning -- faithfully simulating "the repo
// changed after source_head was captured." The fixed script must still
// report the FIRST commit's pandoc floor (it read it via the pinned SHA,
// not the now-moved "HEAD"); the pre-fix script would report the second
// commit's floor instead.
func TestTrack3PreflightPandocFactsArePinnedToSourceHeadNotMutableHEAD(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH to build the mutating fake")
	}

	root := t.TempDir()
	runner := filepath.Join(root, "src", "zeo_core", "integrations", "pandoc", "_runner.py")
	if err := os.MkdirAll(filepath.Dir(runner), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runner, []byte("MIN_PANDOC_VERSION: Final = (2, 15)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	toolsDir := filepath.Join(root, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "install-pandoc.sh"), []byte("VERSION=\"3.1.3\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("requires-python = \">=3.14\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "init", "-q")
	gitFixture(t, root, "add", "-A")
	gitFixture(t, root, "commit", "-q", "-m", "first")
	firstHead := strings.TrimSpace(gitFixture(t, root, "rev-parse", "HEAD"))

	// A second, later commit with a deliberately different, distinguishable
	// floor version -- what a buggy re-resolution of "HEAD" would leak.
	if err := os.WriteFile(runner, []byte("MIN_PANDOC_VERSION: Final = (9, 99)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "add", "-A")
	gitFixture(t, root, "commit", "-q", "-m", "second")
	secondHead := strings.TrimSpace(gitFixture(t, root, "rev-parse", "HEAD"))
	gitFixture(t, root, "checkout", "-q", firstHead)

	fakeBin := filepath.Join(root, "..", "fakebin")
	if err := os.Mkdir(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "..", "mutated-once")
	wrapper := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-C\" ] && [ \"$3\" = \"rev-parse\" ] && [ \"$4\" = \"HEAD\" ]; then\n" +
		"  out=$(\"" + realGit + "\" -C \"$2\" rev-parse HEAD)\n" +
		"  if [ ! -e \"" + sentinel + "\" ]; then\n" +
		"    : > \"" + sentinel + "\"\n" +
		"    \"" + realGit + "\" -C \"$2\" checkout -q " + secondHead + "\n" +
		"  fi\n" +
		"  echo \"$out\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "git"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}

	out, err := runPreflightEnv(t, hermeticEnv(t, fakeBin), root, filepath.Join(root, "..", "scratch"), "3.11", firstHead)
	mustRunOrGateFail(t, out, err)
	wantLine(t, out, "zeocore source_head:", "zeocore source_head: "+firstHead)
	if !strings.Contains(mustLine(t, out, "pandoc floor"), "2.15") {
		t.Fatalf("pandoc floor must be read from the pinned source_head (%s, floor 2.15), not the mutable HEAD that moved to %s (floor 9.99) between calls: %s", firstHead, secondHead, out)
	}
}
