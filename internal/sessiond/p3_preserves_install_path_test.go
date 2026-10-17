package sessiond

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// council P3 on #137: no test covered CreateRequest's own JSON field, or
// cmd/pomar/session.go's CLI flag, actually reaching seatterm.Command --
// the existing test called checkExpectedSHA256 directly, so dropping the
// field at either plumbing point (server.go's CreateRequest->Command
// construction, or the CLI's own CreateRequest literal) would pass every
// existing test while silently discarding -preserves-install-path. This
// drives the real POST /v1/sessions route, with a correctly-hashed file,
// so the only way it can refuse is if PreservesInstallPath actually made
// it all the way from the request body through to the launch.
func TestCreateRequestPlumbsPreservesInstallPathToTheLaunch(t *testing.T) {
	_, c := serve(t, root(t))
	dir := t.TempDir()
	path := dir + "/prog"
	content := []byte("#!/bin/sh\necho ok\n")
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	expected := hex.EncodeToString(sum[:])

	req := CreateRequest{Operation: "op-pip-1", Name: "pip", Kind: "program", Path: path,
		Env: []string{"PATH=/usr/bin:/bin"}, Cols: 60, Rows: 8,
		ExpectedSHA256: expected, PreservesInstallPath: true}
	err := c.Do("POST", "/v1/sessions", req, nil)
	if err == nil {
		t.Fatal("a correctly-hashed program with PreservesInstallPath set should still refuse the launch, not silently pin it")
	}
	if !strings.Contains(err.Error(), "resolves resources relative to its installed location") {
		// Pins that this is really checkExpectedSHA256's own pin-refusal
		// reaching all the way back through the API, not some other
		// failure shape that happens to also return non-nil.
		t.Fatalf("expected the pin-refusal's own message, got: %v", err)
	}

	// Without PreservesInstallPath, the identical hash launches normally --
	// confirming the refusal above was really about the flag, not an
	// unrelated problem with the request.
	req2 := req
	req2.Operation, req2.PreservesInstallPath = "op-pip-2", false
	if err := c.Do("POST", "/v1/sessions", req2, nil); err != nil {
		t.Fatalf("the same hash without the flag should launch: %v", err)
	}
}
