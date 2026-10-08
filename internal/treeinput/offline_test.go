package treeinput

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMirrorExecutableConfigNeverRuns(t *testing.T) {
	mirror, sha, _ := fixture(t)
	marker := filepath.Join(t.TempDir(), "CONFIG-EXECUTED")
	// Even apparently harmless complete mirrors must not execute fsmonitor,
	// ssh, credentials, filters, includes or hooks from their own configuration.
	for _, kv := range [][2]string{{"core.sshCommand", "touch " + marker + "; false"}, {"core.fsmonitor", "touch " + marker + "; false"}, {"credential.helper", "!touch " + marker}, {"filter.hostile.smudge", "touch " + marker}, {"core.hooksPath", filepath.Join(t.TempDir(), "hooks")}, {"include.path", filepath.Join(t.TempDir(), "nonexistent")}} {
		runGit(t, mirror, "config", kv[0], kv[1])
	}
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, filepath.Join(t.TempDir(), "input"), nil, MaxArchive); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("repository command executed")
	}
	runGit(t, mirror, "config", "remote.origin.promisor", "true")
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, filepath.Join(t.TempDir(), "partial"), nil, MaxArchive); e == nil {
		t.Fatal("promisor mirror accepted")
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("promisor transport executed")
	}
}
func TestIncompleteMirrorCannotLazyFetch(t *testing.T) {
	mirror, sha, _ := fixture(t)
	marker := filepath.Join(t.TempDir(), "SSH-RAN")
	runGit(t, mirror, "config", "core.sshCommand", "touch "+marker+"; false")
	runGit(t, mirror, "config", "remote.origin.url", "ssh://invalid.invalid/source")
	runGit(t, mirror, "config", "extensions.partialClone", "origin")
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, filepath.Join(t.TempDir(), "input"), nil, MaxArchive); e == nil {
		t.Fatal("partial clone accepted")
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("lazy fetch executed configured command")
	}
}
func TestCanonicalVerifierRefusesHiddenExtractionMetadata(t *testing.T) {
	mirror, sha, _ := fixture(t)
	out := filepath.Join(t.TempDir(), "input")
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, out, nil, MaxArchive); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(out)
	for _, kind := range []string{"xattr", "owner", "time", "link", "pax", "gzip-padding"} {
		t.Run(kind, func(t *testing.T) {
			changed := rewrite(t, b, func(h *tar.Header, data []byte) (*tar.Header, []byte) {
				if h.Name == "foo/run" {
					switch kind {
					case "xattr":
						h.PAXRecords = map[string]string{"SCHILY.xattr.security.capability": "value"}
						h.Format = tar.FormatPAX
					case "owner":
						h.Uid = 1000
						h.Uname = "root"
					case "time":
						h.ModTime = h.ModTime.Add(1000000000)
					case "link":
						h.Linkname = "other"
					case "pax":
						h.PAXRecords = map[string]string{"comment": "unexpected"}
						h.Format = tar.FormatPAX
					}
				}
				return h, data
			}, nil)
			if kind == "gzip-padding" {
				changed = append(changed, 0, 0, 0)
			}
			if _, e := Verify(bytes.NewReader(changed), sha, nil); e == nil {
				t.Fatal("unexpected archive metadata accepted")
			}
		})
	}
}
func TestVerifiedExtractionUsesCopiedBytesAndNeverOverwrites(t *testing.T) {
	mirror, sha, _ := fixture(t)
	out := filepath.Join(t.TempDir(), "input")
	if _, e := Build(context.Background(), mirror, "refs/heads/main", sha, out, nil, MaxArchive); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(out)
	dst := filepath.Join(t.TempDir(), "extracted")
	m, e := Extract(bytes.NewReader(b), sha, []string{"package-lock.json"}, dst)
	if e != nil || m.Commit != sha {
		t.Fatal(m, e)
	}
	data, e := os.ReadFile(filepath.Join(dst, "hidden.txt"))
	if e != nil || string(data) != "must remain\n" {
		t.Fatal("tracked bytes lost", e)
	}
	if _, e := os.Stat(filepath.Join(dst, ".pomar-extraction-incomplete")); !os.IsNotExist(e) {
		t.Fatal("completed extraction marked incomplete")
	}
	if _, e := Extract(bytes.NewReader(b), sha, nil, dst); e == nil {
		t.Fatal("existing output reused")
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if _, e := Extract(bytes.NewReader(append(b, 1)), sha, nil, bad); e == nil {
		t.Fatal("unverified extraction accepted")
	}
	if _, e := os.Stat(bad); !os.IsNotExist(e) {
		t.Fatal("unverified bytes touched destination")
	}
	if e := portablePaths([]Entry{{Path: "A/x"}, {Path: "a/y"}}); e == nil {
		t.Fatal("colliding directories accepted")
	}
	if e := portablePaths([]Entry{{Path: "README"}, {Path: "readme"}}); e == nil {
		t.Fatal("colliding files accepted")
	}
	// An independently pinned commit remains necessary on extraction too.
	if _, e := Extract(bytes.NewReader(b), strings.Repeat("f", 40), nil, filepath.Join(t.TempDir(), "wrong")); e == nil {
		t.Fatal("wrong pin extracted")
	}
}
