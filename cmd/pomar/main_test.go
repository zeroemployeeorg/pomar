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
	if got := run(nil, &out, &errb); got != 2 {
		t.Errorf("run() = %d, want 2", got)
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

// A result reply as `pomar attempt result` prints it (indented) verifies, and
// an edited copy does not. The printed reply re-indents the signed document.
func TestResultVerifyAcceptsThePrintedReply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := result.LoadOrCreate(dir, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := result.Canonical(map[string]any{"attempt": "a1", "exit_code": 0, "state": "exited", "command": []string{"make", "verify"}})
	reply := manager.ResultReply{Result: doc, Signature: base64.StdEncoding.EncodeToString(s.Sign(doc)), KeyID: s.ID}
	printed, _ := json.MarshalIndent(reply, "", "  ") // as attemptCmd prints it
	pub := base64.StdEncoding.EncodeToString(s.Public())
	check := func(b []byte) int {
		f := filepath.Join(t.TempDir(), "reply.json")
		os.WriteFile(f, b, 0o600)
		var out, errb bytes.Buffer
		return run([]string{"result", "verify", "-reply", f, "-public-key", pub}, &out, &errb)
	}
	if got := check(printed); got != 0 {
		t.Fatalf("the printed reply did not verify: exit %d", got)
	}
	compact, _ := json.Marshal(reply)
	if got := check(compact); got != 0 {
		t.Fatalf("the compact reply did not verify: exit %d", got)
	}
	edited := bytes.Replace(printed, []byte(`"exit_code": 0`), []byte(`"exit_code": 1`), 1)
	if bytes.Equal(edited, printed) {
		t.Fatal("the test did not edit the reply")
	}
	if got := check(edited); got != 1 {
		t.Fatalf("an edited reply verified: exit %d", got)
	}
}
