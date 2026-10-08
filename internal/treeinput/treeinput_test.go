package treeinput

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("fixture Git: %v: %s", e, b)
	}
	return strings.TrimSpace(string(b))
}
func fixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	os.Mkdir(work, 0700)
	runGit(t, work, "init", "-q", "-b", "main")
	files := map[string]string{"package.json": "{}\n", "package-lock.json": "{\"lockfileVersion\":3}\n", ".gitattributes": "hidden.txt export-ignore\nsubst.txt export-subst\n", "hidden.txt": "must remain\n", "subst.txt": "$Format:%H$\n", "foo.bar": "sorting\n", "foo/run": "#!/bin/sh\nexit 0\n"}
	for p, b := range files {
		f := filepath.Join(work, p)
		os.MkdirAll(filepath.Dir(f), 0700)
		if e := os.WriteFile(f, []byte(b), 0600); e != nil {
			t.Fatal(e)
		}
	}
	os.Chmod(filepath.Join(work, "foo/run"), 0755)
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "fixture")
	sha := runGit(t, work, "rev-parse", "HEAD")
	mirror := filepath.Join(root, "mirror.git")
	runGit(t, work, "clone", "--mirror", "--no-hardlinks", "-q", work, mirror)
	return mirror, sha, work
}
func TestExactTreeIndependentOfArchiveAttributes(t *testing.T) {
	mirror, sha, _ := fixture(t)
	out := filepath.Join(t.TempDir(), "engine.tar.gz")
	req := []string{"package.json", "package-lock.json", "hidden.txt", "subst.txt"}
	before := runGit(t, mirror, "show-ref")
	m, e := Build(context.Background(), mirror, "refs/heads/main", sha, out, req, MaxArchive)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(out)
	got, e := Verify(bytes.NewReader(b), sha, req)
	if e != nil {
		t.Fatal(e)
	}
	if got.Tree != m.Tree || len(got.Entries) != 7 {
		t.Fatalf("bad tree: %+v", got)
	}
	for _, x := range got.Entries {
		if x.Path == "subst.txt" && x.SHA256 != sha256ID([]byte("$Format:%H$\n")) {
			t.Fatal("export-subst rewrote tracked bytes")
		}
	}
	if after := runGit(t, mirror, "show-ref"); after != before {
		t.Fatal("mirror refs changed")
	}
	fi, _ := os.Stat(out)
	if fi.Mode().Perm() != 0600 {
		t.Fatal("output not private")
	}
	out2 := filepath.Join(t.TempDir(), "again.tar.gz")
	if _, e = Build(context.Background(), mirror, "refs/heads/main", sha, out2, req, MaxArchive); e != nil {
		t.Fatal(e)
	}
	b2, _ := os.ReadFile(out2)
	if !bytes.Equal(b, b2) {
		t.Fatal("same tree did not reproduce")
	}
	if _, e = Verify(bytes.NewReader(b), strings.Repeat("1", 40), req); e == nil {
		t.Fatal("wrong independent commit accepted")
	}
}
func TestExportRefusesUnreachableMissingLockLinksAndBounds(t *testing.T) {
	mirror, sha, work := fixture(t)
	for _, req := range [][]string{{"missing-lock.json"}, {"../package.json"}} {
		if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, filepath.Join(t.TempDir(), "bad"), req, MaxArchive); e == nil {
			t.Fatal("missing/unsafe required path accepted")
		}
	}
	if _, e := Build(context.Background(), mirror, "refs/heads/missing", sha, filepath.Join(t.TempDir(), "bad"), nil, MaxArchive); e == nil {
		t.Fatal("missing branch accepted")
	}
	out := filepath.Join(t.TempDir(), "tiny")
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, out, nil, 1); e == nil {
		t.Fatal("compressed bound ignored")
	}
	if _, e := os.Stat(out); !os.IsNotExist(e) {
		t.Fatal("partial own output remains")
	}
	if e := os.WriteFile(out, []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, out, nil, MaxArchive); e == nil {
		t.Fatal("existing output overwritten")
	}
	b, _ := os.ReadFile(out)
	if string(b) != "original" {
		t.Fatal("existing output touched")
	}
	os.Symlink("package.json", filepath.Join(work, "link"))
	runGit(t, work, "add", "link")
	runGit(t, work, "commit", "-qm", "symlink")
	newSHA := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, mirror, "fetch", "-q", work, "+refs/heads/main:refs/heads/main")
	if _, e := Build(context.Background(), mirror, "refs/heads/main", newSHA, filepath.Join(t.TempDir(), "bad"), nil, MaxArchive); e == nil {
		t.Fatal("symlink accepted")
	}
}
func rewrite(t *testing.T, original []byte, change func(*tar.Header, []byte) (*tar.Header, []byte), extra []byte) []byte {
	t.Helper()
	g, e := gzip.NewReader(bytes.NewReader(original))
	if e != nil {
		t.Fatal(e)
	}
	tr := tar.NewReader(g)
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(tr)
		h, b = change(h, b)
		h.Size = int64(len(b))
		if e = tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		tw.Write(b)
	}
	tw.Close()
	gz.Write(extra)
	gz.Close()
	g.Close()
	return out.Bytes()
}
func TestVerifierRejectsTamperAndTraversal(t *testing.T) {
	mirror, sha, _ := fixture(t)
	out := filepath.Join(t.TempDir(), "input")
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, out, nil, MaxArchive); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(out)
	cases := map[string]func(*tar.Header, []byte) (*tar.Header, []byte){
		"bytes": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "package.json" {
				b = []byte("changed")
			}
			return h, b
		},
		"mode": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "package.json" {
				h.Mode = 0755
			}
			return h, b
		},
		"traversal": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "package.json" {
				h.Name = "../outside"
			}
			return h, b
		},
		"git-path": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == "package.json" {
				h.Name = ".Git/config"
			}
			return h, b
		},
		"manifest-omission": func(h *tar.Header, b []byte) (*tar.Header, []byte) {
			if h.Name == ManifestName {
				var m Manifest
				json.Unmarshal(b, &m)
				m.Entries = m.Entries[1:]
				b, _ = json.Marshal(m)
			}
			return h, b
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := Verify(bytes.NewReader(rewrite(t, b, fn, nil)), sha, nil); e == nil {
				t.Fatal("tampered input accepted")
			}
		})
	}
	identity := func(h *tar.Header, b []byte) (*tar.Header, []byte) { return h, b }
	if _, e := Verify(bytes.NewReader(rewrite(t, b, identity, []byte("hidden trailing data"))), sha, nil); e == nil {
		t.Fatal("trailing tar data accepted")
	}
	if _, e := Verify(bytes.NewReader(append(append([]byte{}, b...), b...)), sha, nil); e == nil {
		t.Fatal("second gzip member accepted")
	}
	if _, e := Verify(bytes.NewReader(b), sha, []string{"not-in-tree"}); e == nil {
		t.Fatal("missing consumer requirement accepted")
	}
}
