package main

import (
	"bytes"
	"strings"
	"testing"
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
