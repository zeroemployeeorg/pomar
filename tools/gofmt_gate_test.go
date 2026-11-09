package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real Make target: empty output is not evidence that a formatter
// ran successfully. Fixtures have no source files, network or host effects.
func TestGofmtGatePropagatesFormatterFailure(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Fatal("make is required by the source gate", err)
	}
	makefile, err := filepath.Abs("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		success    bool
		message    string
	}{
		{"clean", "exit 0\n", true, ""},
		{"unformatted", "echo unformatted.go\nexit 0\n", false, "unformatted.go"},
		{"empty-failure", "exit 73\n", false, "Error 73"},
		{"unavailable", "", false, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			if tc.body != "" {
				if err := os.WriteFile(filepath.Join(bin, "gofmt"), []byte("#!/bin/sh\n"+tc.body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(makeBin, "--no-print-directory", "-f", makefile, "gofmt")
			cmd.Dir = dir
			// Shell builtins suffice for this target. A missing host gofmt cannot be
			// accidentally satisfied by a formatter elsewhere on the inherited PATH.
			cmd.Env = []string{"PATH=" + bin, "HOME=" + dir, "LC_ALL=C"}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.success {
				t.Fatalf("success=%v want%v, output:%s", err == nil, tc.success, out)
			}
			if tc.message != "" && !strings.Contains(string(out), tc.message) {
				t.Fatalf("missing %q in %s", tc.message, out)
			}
		})
	}
}
