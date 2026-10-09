package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"github.com/zeroemployeeorg/pomar/internal/distribution"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestDistRefusesEmbeddedBuilderPathsBeforeCreatingOutput(t *testing.T) {
	bins := t.TempDir()
	source := t.TempDir()
	out := filepath.Join(t.TempDir(), "not-created")
	for _, secret := range []string{"/Users/" + "builder/work", source} {
		if err := os.WriteFile(filepath.Join(bins, "pomar"), []byte("fixture "+secret), 0755); err != nil {
			t.Fatal(err)
		}
		err := run([]string{"-version", "v0.1.0", "-commit", "0123456789abcdef0123456789abcdef01234567", "-bin-dir", bins, "-source", source, "-out", out})
		if err == nil || !strings.Contains(err.Error(), "builder/source paths") {
			t.Fatalf("wrong refusal: %v", err)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("created output before refusing unsafe bytes")
		}
	}
}
