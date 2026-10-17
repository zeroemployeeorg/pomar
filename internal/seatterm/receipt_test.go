package seatterm

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// A matching ExpectedSHA256 launches normally; a mismatch refuses the
// launch outright, before anything runs. The pinned program is this test's
// own script, never a macOS platform binary: AMFI kills a copy of one of
// those on exec (confirmed: copying /bin/sh and exec'ing the copy exits
// 137), which checkExpectedSHA256's Darwin copy would trip over for a
// platform binary specifically. An agent's own binary (Pomar's, Claude
// Code's, Codex's) is ordinarily signed, not a platform binary, and isn't
// affected; this is named here, not glossed over.
func TestExpectedSHA256PinsTheExactBinary(t *testing.T) {
	h, _ := newHost(t)
	dir := t.TempDir()
	path := dir + "/prog"
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := SessionSpec{Name: "agent", Kind: "agent", Command: Command{Path: path, UID: -1, GID: -1}, Cols: 80, Rows: 24}
	spec.Command.ExpectedSHA256 = sha256OfFile(t, path)
	s, _, err := h.Create("op-sha-1", "operator", spec)
	if err != nil {
		t.Fatalf("a matching hash should launch: %v", err)
	}
	collect(t, mustAttach(t, s, "v"), nil, "ok")

	spec2 := spec
	spec2.Command.ExpectedSHA256 = strings.Repeat("0", 64)
	if _, _, err := h.Create("op-sha-2", "operator", spec2); err == nil {
		t.Fatal("a mismatched hash should refuse the launch")
	}
}

func mustAttach(t *testing.T, s *TermSession, principal string) *Client {
	t.Helper()
	c, _, err := s.Attach(principal)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Health is a passive, heuristic read of the current screen: it fires on
// each pattern and nothing else, and never on an ordinary screen.
func TestHealthPatternsAreHeuristicNotAuthoritative(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-health-1", "operator", sh(`printf 'all good\n'; sleep 30`))
	if err != nil {
		t.Fatal(err)
	}
	c := mustAttach(t, s, "v")
	collect(t, c, nil, "all good")
	if hl := s.Health(); hl.PossiblePanic || hl.PossibleReadonlyRefusal || hl.PossibleApprovalPrompt {
		t.Fatalf("an ordinary screen shouldn't trip any pattern: %+v", hl)
	}

	s2, _, err := h.Create("op-health-2", "operator", sh(`printf 'panic: boom\n'; sleep 30`))
	if err != nil {
		t.Fatal(err)
	}
	c2 := mustAttach(t, s2, "v")
	collect(t, c2, nil, "panic: boom")
	if hl := s2.Health(); !hl.PossiblePanic || hl.PossibleReadonlyRefusal || hl.PossibleApprovalPrompt {
		t.Fatalf("a panic screen should trip only possible_panic: %+v", hl)
	}

	s3, _, err := h.Create("op-health-3", "operator", sh(`printf 'touch: read-only file system\n'; sleep 30`))
	if err != nil {
		t.Fatal(err)
	}
	c3 := mustAttach(t, s3, "v")
	collect(t, c3, nil, "read-only")
	if hl := s3.Health(); hl.PossiblePanic || !hl.PossibleReadonlyRefusal || hl.PossibleApprovalPrompt {
		t.Fatalf("a read-only refusal should trip only possible_readonly_refusal: %+v", hl)
	}

	s4, _, err := h.Create("op-health-4", "operator", sh(`printf 'Do you want to proceed?\n'; sleep 30`))
	if err != nil {
		t.Fatal(err)
	}
	c4 := mustAttach(t, s4, "v")
	collect(t, c4, nil, "proceed")
	if hl := s4.Health(); hl.PossiblePanic || hl.PossibleReadonlyRefusal || !hl.PossibleApprovalPrompt {
		t.Fatalf("an approval prompt should trip only possible_approval_prompt: %+v", hl)
	}
}

// Scrollback gives the lines scrolled off the screen, oldest first,
// readable without attaching, and -lines bounds it to the most recent.
func TestScrollbackGivesHistoryBeyondTheScreen(t *testing.T) {
	h, _ := newHost(t)
	spec := sh(`i=1; while [ $i -le 20 ]; do printf 'line%02d\n' $i; i=$((i+1)); done; sleep 30`)
	spec.Rows = 4
	s, _, err := h.Create("op-scroll-1", "operator", spec)
	if err != nil {
		t.Fatal(err)
	}
	c := mustAttach(t, s, "v")
	collect(t, c, nil, "line20")

	sb := s.Scrollback(0)
	if len(sb.Lines) == 0 {
		t.Fatal("no scrollback: the screen is only 4 rows but 20 lines were printed")
	}
	found1, found20 := false, false
	for _, l := range sb.Lines {
		if l == "line01" {
			found1 = true
		}
		if l == "line20" {
			found20 = true
		}
	}
	if !found1 {
		t.Fatalf("the earliest scrolled line is missing: %v", sb.Lines)
	}
	if found20 {
		t.Fatalf("the last lines are still on screen, not scrollback: %v", sb.Lines)
	}

	limited := s.Scrollback(2)
	if len(limited.Lines) != 2 || !limited.Truncated {
		t.Fatalf("-lines should bound and flag truncation: %+v", limited)
	}
	full := s.Scrollback(0)
	if limited.Lines[0] != full.Lines[len(full.Lines)-2] || limited.Lines[1] != full.Lines[len(full.Lines)-1] {
		t.Fatalf("a bounded read should be the most recent lines: got %v, want the tail of %v", limited.Lines, full.Lines)
	}
}

// A file replaced at path after the check, before the exec that follows,
// can't be what actually runs: checkExpectedSHA256 pins the launch to the
// exact bytes it hashed, by platform-specific means (an inherited
// descriptor on Linux, a private copy on Darwin), not merely to the name.
func TestExpectedSHA256SurvivesReplacementAfterTheCheck(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/prog"
	original := []byte("#!/bin/sh\necho original\n")
	if err := os.WriteFile(path, original, 0o700); err != nil {
		t.Fatal(err)
	}
	expected := sha256OfFile(t, path)

	cmd := exec.Command(path)
	closeAfterStart, err := checkExpectedSHA256(cmd, path, expected, false)
	if err != nil {
		t.Fatalf("the check should pass on the original bytes: %v", err)
	}
	if cmd.Path == path {
		t.Fatal("the launch must be pinned away from the original path, not merely re-checked at exec time")
	}

	// The race: the original is replaced the instant the check returns,
	// before anything execs.
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho replaced\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := cmd.Output()
	if closeAfterStart != nil {
		closeAfterStart()
	}
	if err != nil {
		t.Fatalf("the pinned launch should still run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "original" {
		t.Fatalf("the launch reflected the replaced file, not what was checked: got %q", got)
	}
}

// The Darwin/Codex-resource-path decision: a program that resolves
// resources relative to its own installed location refuses pinning
// outright, on both platforms, rather than silently run it from a
// relocated path (a private copy on Darwin, /proc/self/fd on Linux) that
// may not find them. No file is even opened for the check once this is
// set: a mismatched hash never races with a path concern.
func TestExpectedSHA256RefusesToPinAProgramThatPreservesItsInstallPath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/prog"
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	expected := sha256OfFile(t, path)

	cmd := exec.Command(path)
	closeAfterStart, err := checkExpectedSHA256(cmd, path, expected, true)
	if err == nil {
		t.Fatal("pinning a path-preserving program should refuse, not silently relocate it")
	}
	if closeAfterStart != nil {
		t.Fatal("a refusal should hand back nothing to close")
	}
	if cmd.Path != path {
		t.Fatalf("a refused launch must not have modified cmd.Path: got %q", cmd.Path)
	}
}

// A pattern that has already scrolled off the visible screen is still
// found, up to healthScanLines back; the version string names which list
// matched.
func TestHealthScansScrollbackNotOnlyTheScreen(t *testing.T) {
	h, _ := newHost(t)
	spec := sh(`printf 'panic: boom\n'; i=1; while [ $i -le 10 ]; do printf 'line%02d\n' $i; i=$((i+1)); done; sleep 30`)
	spec.Rows = 4
	s, _, err := h.Create("op-health-scroll-1", "operator", spec)
	if err != nil {
		t.Fatal(err)
	}
	c := mustAttach(t, s, "v")
	collect(t, c, nil, "line10")
	if scr := s.Screen(); strings.Contains(strings.Join(scr.Lines, "\n"), "panic:") {
		t.Fatal("the panic line should have scrolled off the 4-row screen by now")
	}
	hl := s.Health()
	if !hl.PossiblePanic {
		t.Fatalf("a panic scrolled into history should still be found: %+v", hl)
	}
	if hl.PatternVersion != PatternVersion {
		t.Fatalf("health should name its pattern version: %+v", hl)
	}
}
