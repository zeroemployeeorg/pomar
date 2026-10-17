package seatterm

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// Layer 5's MEDIUM-LOW: checkExpectedSHA256's Linux path hands its opened
// file to the child via cmd.ExtraFiles, then returns closeAfterStart, which
// the caller must call once Start has returned -- os/exec never closes an
// ExtraFiles entry itself. Before this fix, nothing ever called it: one fd
// leaked per launch, for as long as the host process ran (every session
// made with -expected-sha256). This confirms closeAfterStart actually
// closes the descriptor: a raw syscall read on its fd number, taken before
// the close, must fail (EBADF) afterward.
//
// This file is named *_linux.go's test counterpart on purpose: it only
// builds and runs under GOOS=linux, the one platform this leak exists on
// (exec_darwin.go never hands over an inherited fd at all). It has been
// cross-compiled (GOOS=linux go vet/build) from a Darwin development
// machine and type-checks cleanly, but has NOT been executed on an actual
// Linux host as part of this change -- that is a real, stated limitation,
// not a claim of having run it.
func TestCheckExpectedSHA256ClosesItsFileAfterStart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/prog"
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrue\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	expected := sha256OfFile(t, path)

	cmd := exec.Command(path)
	closeAfterStart, err := checkExpectedSHA256(cmd, path, expected, false)
	if err != nil {
		t.Fatal(err)
	}
	if closeAfterStart == nil {
		t.Fatal("the Linux path always hands over an fd on a match; closeAfterStart must not be nil")
	}
	if len(cmd.ExtraFiles) != 1 {
		t.Fatalf("expected exactly one ExtraFiles entry, got %d", len(cmd.ExtraFiles))
	}
	fd := cmd.ExtraFiles[0].Fd()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closeAfterStart()
	cmd.Wait()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != syscall.EBADF {
		t.Fatalf("the parent's own fd should be closed after closeAfterStart: Fstat err = %v, want EBADF", err)
	}
}
