package manager

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/mirror"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// npmFixture is an upstream repository, a fake npm registry serving one
// tarball, and an npm-class manager that syncs the repository.
func npmFixture(t *testing.T, shim string) (*Manager, *mirror.Mirrors, []string, string, []byte, *httptest.Server) {
	t.Helper()
	tarball := []byte("left-pad tarball")
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/left-pad/-/left-pad-1.3.0.tgz" {
			w.Write(tarball)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(reg.Close)
	env := gitEnv(t)
	up := t.TempDir()
	git(t, env, up, "init", "--quiet", "--initial-branch=main")
	v, err := venue.Open(shortDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	mirrors := &mirror.Mirrors{Venue: v, Env: env}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, Mirrors: mirrors,
		MirrorURLs: map[string]string{"up": "file://" + up}, NPM: true, ShimBin: shim, NPMUpstream: reg.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, mirrors, env, up, tarball, reg
}

func npmLockJSON(reg string, integrity string) string {
	b, _ := json.Marshal(map[string]any{"lockfileVersion": 3, "packages": map[string]any{
		"":                      map[string]any{"name": "demo"},
		"node_modules/left-pad": map[string]any{"resolved": reg + "/left-pad/-/left-pad-1.3.0.tgz", "integrity": integrity},
	}})
	return string(b)
}

func sriSHA512(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

// An npm class refuses, before capacity is claimed, a commit with no lock or
// with a lock the proxy refuses, a start with no source, and a manager with no
// shim; a good lock reaches admission.
func TestNPMClassRefusals(t *testing.T) {
	self, _ := os.Executable()
	m, _, env, up, tarball, reg := npmFixture(t, self)
	commit(t, env, up, "no lock yet")
	if _, err := m.Start("n1", []string{"true"}, &Source{Mirror: "up", Ref: "main"}); err == nil || !strings.Contains(err.Error(), "has no package-lock.json") {
		t.Fatalf("a commit with no lock: %v", err)
	}
	os.WriteFile(filepath.Join(up, "package-lock.json"), []byte(npmLockJSON(reg.URL, "sha1-AAAAAAAAAAAAAAAAAAAAAAAAAAA=")), 0o644)
	git(t, env, up, "add", "package-lock.json")
	git(t, env, up, "commit", "--quiet", "-m", "an sha1 lock")
	if _, err := m.Start("n2", []string{"true"}, &Source{Mirror: "up", Ref: "main"}); err == nil || !strings.Contains(err.Error(), "no sha512") {
		t.Fatalf("an sha1-only lock: %v", err)
	}
	if _, err := m.Start("n3", []string{"true"}, nil); err == nil || !strings.Contains(err.Error(), "npm needs a source") {
		t.Fatalf("no source: %v", err)
	}
	os.WriteFile(filepath.Join(up, "package-lock.json"), []byte(npmLockJSON(reg.URL, sriSHA512(tarball))), 0o644)
	git(t, env, up, "commit", "--quiet", "-am", "a good lock")
	// Past the lock and admission, it stops at the unentitled test binary.
	if _, err := m.Start("n4", []string{"true"}, &Source{Mirror: "up", Ref: "main"}); err == nil || strings.Contains(err.Error(), "npm") || strings.Contains(err.Error(), "lock") {
		t.Fatalf("a good lock: %v, want past the lock", err)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d entries; a refused start created one", n)
	}

	noShim, _, env2, up2, _, _ := npmFixture(t, "")
	commit(t, env2, up2, "one")
	if _, err := noShim.Start("n5", []string{"true"}, &Source{Mirror: "up", Ref: "main"}); err == nil || !strings.Contains(err.Error(), "no shim") {
		t.Fatalf("no shim: %v", err)
	}
}

// An attempt's npm registry serves exactly its lock on its own socket, is
// closed with the attempt, and is served again from the record after a
// restart.
func TestNPMRegistryServesTheLockOnItsSocket(t *testing.T) {
	self, _ := os.Executable()
	m, mirrors, env, up, tarball, reg := npmFixture(t, self)
	os.WriteFile(filepath.Join(up, "package-lock.json"), []byte(npmLockJSON(reg.URL, sriSHA512(tarball))), 0o644)
	git(t, env, up, "add", "package-lock.json")
	git(t, env, up, "commit", "--quiet", "-m", "lock")
	sha := git(t, env, up, "rev-parse", "HEAD")
	if err := mirrors.Sync(context.Background(), "up", "file://"+up); err != nil {
		t.Fatal(err)
	}
	lock, rec, err := m.readNPMLock(&Source{Mirror: "up"}, sha)
	if err != nil || rec.Packages != 1 || len(rec.SHA256) != 64 {
		t.Fatalf("readNPMLock: %+v, %v", rec, err)
	}
	id := "npm-1"
	os.MkdirAll(filepath.Join(m.cfg.Venue.Root(), attemptsDir, id), 0o700)
	get := func(path string) (int, string) {
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", m.npmSocket(id))
		}}}
		resp, err := c.Get("http://npm" + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	m.mu.Lock()
	err = m.listenNPM(id, lock)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get("/left-pad/-/left-pad-1.3.0.tgz"); code != 200 || body != string(tarball) {
		t.Fatalf("the locked tarball: %d %q", code, body)
	}
	if code, _ := get("/left-pad"); code != 404 {
		t.Fatalf("a packument: %d", code)
	}
	m.mu.Lock()
	m.closeNPM(id)
	m.mu.Unlock()
	if _, err := os.Stat(m.npmSocket(id)); err == nil {
		t.Fatal("the socket outlived its attempt")
	}
	// After a restart: served again from the lock kept in the record.
	m.mu.Lock()
	err = m.reopenNPM(id)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get("/left-pad/-/left-pad-1.3.0.tgz"); code != 200 || body != string(tarball) {
		t.Fatalf("after a reopen: %d %q", code, body)
	}
	m.mu.Lock()
	m.closeNPM(id)
	m.mu.Unlock()
}
