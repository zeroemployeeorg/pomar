package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/manager"
	"github.com/zeroemployeeorg/pomar/internal/result"
)

func TestRun(t *testing.T) {
	var out, errb bytes.Buffer
	if got := run([]string{"version"}, &out, &errb); got != 0 {
		t.Errorf("run(version) = %d, want 0", got)
	}
	if got := run(nil, &out, &errb); got != 0 {
		t.Errorf("run() = %d, want 0", got)
	}
}

func TestVenueStatus(t *testing.T) {
	var out, errb bytes.Buffer
	if got := run([]string{"venue", "status", "-root", t.TempDir()}, &out, &errb); got != 0 {
		t.Fatalf("status = %d, stderr %q", got, errb.String())
	}
	if !strings.Contains(out.String(), "open objects: 0") {
		t.Fatalf("output = %q", out.String())
	}
	t.Setenv("POMAR_DATA_ROOT", "")
	out.Reset()
	errb.Reset()
	if got := run([]string{"venue", "status"}, &out, &errb); got != 1 {
		t.Fatalf("status with no root = %d, want 1", got)
	}
}

func TestVenueUnaccountedExit(t *testing.T) {
	root := t.TempDir()
	var out, errb bytes.Buffer
	if got := run([]string{"venue", "init", "-root", root}, &out, &errb); got != 0 {
		t.Fatalf("init = %d, %q", got, errb.String())
	}
	if err := os.WriteFile(filepath.Join(root, "stray"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if got := run([]string{"venue", "status", "-root", root}, &out, &errb); got != 3 {
		t.Fatalf("status with a stray = %d, want 3; out %q", got, out.String())
	}
	if !strings.Contains(out.String(), "unaccounted: 1") {
		t.Fatalf("output = %q", out.String())
	}
}

// A result reply as the manager serves it (and `pomar attempt result` prints
// it) verifies as received, with <, >, & and non-ASCII in the document. A
// re-indented or edited copy does not: the verifier checks the bytes it
// received, never a rebuilt document (the elders' ruling of 2026-09-27 §2.1).
func TestResultVerifyChecksTheReceivedBytes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := result.LoadOrCreate(dir, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := result.Canonical(map[string]any{"attempt": "a1", "exit_code": 0, "state": "exited",
		"command": []string{"/bin/sh", "-c", "make verify > /pomar/outputs/output.log 2>&1 && echo 'déjà vu' <in"}})
	if !bytes.Contains(doc, []byte("> /pomar")) || !bytes.Contains(doc, []byte("&&")) || !bytes.Contains(doc, []byte("déjà")) {
		t.Fatalf("the canonical document escaped something: %s", doc)
	}
	served := []byte(`{"result":` + string(doc) + `,"signature":"` + base64.StdEncoding.EncodeToString(s.Sign(doc)) + `","key_id":"` + s.ID + `"}` + "\n")
	pub := base64.StdEncoding.EncodeToString(s.Public())
	check := func(b []byte) int {
		f := filepath.Join(t.TempDir(), "reply.json")
		os.WriteFile(f, b, 0o600)
		var out, errb bytes.Buffer
		return run([]string{"result", "verify", "-reply", f, "-public-key", pub}, &out, &errb)
	}
	if got := check(served); got != 0 {
		t.Fatalf("the served reply did not verify: exit %d", got)
	}
	var r manager.ResultReply
	json.Unmarshal(served, &r)
	indented, _ := json.MarshalIndent(r, "", "  ")
	if got := check(indented); got != 1 {
		t.Fatalf("a re-indented copy verified: exit %d", got)
	}
	escaped, _ := json.Marshal(r) // Go's default encoder escapes <, > and &
	if bytes.Equal(escaped, bytes.TrimSpace(served)) {
		t.Fatal("the test needs an escaping encoder")
	}
	if got := check(escaped); got != 1 {
		t.Fatalf("an escaped copy verified: exit %d", got)
	}
	edited := bytes.Replace(served, []byte(`"exit_code":0`), []byte(`"exit_code":1`), 1)
	if bytes.Equal(edited, served) {
		t.Fatal("the test did not edit the reply")
	}
	if got := check(edited); got != 1 {
		t.Fatalf("an edited reply verified: exit %d", got)
	}
}
