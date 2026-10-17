package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/sessiond"
)

// council P3-B on #137: internal/sessiond's own test for -preserves-install-path
// (TestCreateRequestPlumbsPreservesInstallPathToTheLaunch) posts a
// CreateRequest directly to the HTTP route and never touches this file at
// all, so it proves nothing about the CLI's own flag literal at
// session.go's `new` case -- dropping -preserves-install-path from that
// sessiond.CreateRequest{...} construction (session.go:200) would pass
// every existing test while silently discarding the flag a caller
// actually typed. This drives the real `pomar session new
// -preserves-install-path -expected-sha256 ...` command line through
// run(), so the only way it can refuse is if the CLI's own flag parsing
// and request construction carried the flag all the way to the launch.
func TestSessionNewCLIPlumbsPreservesInstallPathToTheLaunch(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "pscpip")
	t.Cleanup(func() { os.RemoveAll(dir) })
	root := filepath.Join(dir, "r")

	s, err := sessiond.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan struct{})
	go func() { defer close(done); s.Serve(ctx, func() { close(ready) }) }()
	<-ready
	defer func() { cancel(); <-done }()

	progPath := filepath.Join(dir, "prog")
	content := []byte("#!/bin/sh\necho ok\n")
	if err := os.WriteFile(progPath, content, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	expected := hex.EncodeToString(sum[:])

	var out, errb bytes.Buffer
	rc := run([]string{"session", "new", "-root", root, "-id", "op-cli-pip-1",
		"-expected-sha256", expected, "-preserves-install-path",
		"pip", "--", progPath}, &out, &errb)
	if rc == 0 {
		t.Fatalf("a correctly-hashed program with -preserves-install-path should still refuse the launch via the CLI, not silently pin it: rc=%d out=%s", rc, out.String())
	}
	if !strings.Contains(errb.String(), "resolves resources relative to its installed location") {
		// Pins that this is really the pin-refusal reaching all the way
		// from the CLI's own flag through CreateRequest to the launch,
		// not some other failure shape that happens to also be non-zero.
		t.Fatalf("expected the pin-refusal's own message on stderr, got: %s", errb.String())
	}

	// Without the flag, the identical hash launches normally -- confirming
	// the refusal above was really about the CLI's -preserves-install-path,
	// not an unrelated problem with the request.
	out.Reset()
	errb.Reset()
	if rc := run([]string{"session", "new", "-root", root, "-id", "op-cli-pip-2",
		"-expected-sha256", expected,
		"pip2", "--", progPath}, &out, &errb); rc != 0 {
		t.Fatalf("the same hash without the flag should launch via the CLI: rc=%d err=%s", rc, errb.String())
	}
}
