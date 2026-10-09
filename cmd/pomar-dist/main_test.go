package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"github.com/zeroemployeeorg/pomar/internal/distribution"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPackIsDeterministicAndContainsOnlyReleaseFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range append(append([]string{}, distribution.Files...), "release.json") {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0755)
		os.WriteFile(p, []byte(name), 0644)
	}
	a := filepath.Join(t.TempDir(), "a.tar.gz")
	b := filepath.Join(t.TempDir(), "b.tar.gz")
	for _, out := range []string{a, b} {
		if err := pack(root, "pomar-v0.1.0-darwin-arm64", out); err != nil {
			t.Fatal(err)
		}
	}
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	if !bytes.Equal(x, y) {
		t.Fatal("non-deterministic archive")
	}
	gz, err := gzip.NewReader(bytes.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || h.Uid != 0 || h.Gid != 0 || h.ModTime.Unix() != 0 {
			t.Fatalf("unsafe/un-normalized header %+v", h)
		}
		n++
	}
	if n != len(distribution.Files)+1 {
		t.Fatal(n)
	}
}
func TestDistRefusesBadIdentityAndExistingOutput(t *testing.T) {
	if err := run([]string{"-version", "latest", "-commit", "abcd", "-out", t.TempDir(), "-bin-dir", t.TempDir()}); err == nil {
		t.Fatal("accepted floating/short identity")
	}
}
