package serviceupgrade

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestBoundedFileReadRejectsIndirectionAndFIFO(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "file")
	if e := os.WriteFile(f, []byte("bounded"), 0600); e != nil {
		t.Fatal(e)
	}
	uid := uint32(os.Getuid())
	if b, e := readFile(f, 7, uid); e != nil || string(b) != "bounded" {
		t.Fatal(string(b), e)
	}
	if _, e := readFile(f, 6, uid); e == nil {
		t.Fatal("oversized accepted")
	}
	sy := filepath.Join(d, "symlink")
	if e := os.Symlink(f, sy); e != nil {
		t.Fatal(e)
	}
	if _, e := readFile(sy, 7, uid); e == nil {
		t.Fatal("symlink accepted")
	}
	hard := filepath.Join(d, "hardlink")
	if e := os.Link(f, hard); e != nil {
		t.Fatal(e)
	}
	if _, e := readFile(f, 7, uid); e == nil {
		t.Fatal("hardlinked file accepted")
	}
	fifo := filepath.Join(d, "fifo")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := readFile(fifo, 7, uid); done <- e }()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("fifo accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("fifo blocked")
	}
}
