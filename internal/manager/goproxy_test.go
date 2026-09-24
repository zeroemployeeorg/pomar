package manager

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/cache"
	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/goproxy"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// shortTemp gives a directory whose paths fit a Unix socket; t.TempDir's
// can be too long on macOS.
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func openProxy(t *testing.T, upstream string) (*Manager, *venue.Venue) {
	t.Helper()
	v, err := venue.Open(shortTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.EnsureCache(venue.KindVolume, "goproxy", "goproxy"); err != nil {
		t.Fatal(err)
	}
	p := &goproxy.Proxy{Cache: filepath.Join(v.Root(), "goproxy"), ModuleUpstream: upstream, SumUpstream: upstream}
	m, err := Open(Config{Venue: v, HostBin: hostBin, Procs: &seqProcs{lists: [][]proc.Process{nil}}, UID: me, Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, GoProxy: p, ShimBin: "/nonexistent/shim"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, v
}

func TestProxySocketServesThenCloses(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream %s", r.URL.Path)
	}))
	defer up.Close()
	m, v := openProxy(t, up.URL)
	if err := os.MkdirAll(filepath.Join(v.Root(), attemptsDir, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	err := m.listenProxy("a")
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	sock := m.proxySocket("a")
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := c.Get("http://guest/golang.org/x/sys/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "upstream /golang.org/x/sys/@v/list" {
		t.Fatalf("body %q", b)
	}
	log, _ := os.ReadFile(filepath.Join(v.Root(), attemptsDir, "a", "goproxy.log"))
	if !strings.Contains(string(log), `"/golang.org/x/sys/@v/list" 200`) {
		t.Fatalf("goproxy.log = %q", log)
	}
	m.mu.Lock()
	m.closeProxy("a")
	m.mu.Unlock()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left after close: %v", err)
	}
}

func TestProxySocketPathTooLongIsRefused(t *testing.T) {
	m, _ := openProxy(t, "http://unused.invalid")
	long := strings.Repeat("x", 40)
	m.mu.Lock()
	defer m.mu.Unlock()
	// A 40-character id under a short root still fits; the check is
	// against the platform limit, exercised with a deep root below.
	if err := os.MkdirAll(filepath.Join(m.cfg.Venue.Root(), attemptsDir, long), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.listenProxy(long); err != nil {
		t.Fatalf("a %d-byte socket path refused: %v", len(m.proxySocket(long)), err)
	}
	m.closeProxy(long)

	deep := filepath.Join(m.cfg.Venue.Root(), strings.Repeat("d", 60))
	os.MkdirAll(deep, 0o700)
	v2, err := venue.Open(deep)
	if err != nil {
		t.Fatal(err)
	}
	m2 := &Manager{cfg: Config{Venue: v2, GoProxy: m.cfg.GoProxy}, proxies: map[string]*proxyListener{}}
	if err := m2.listenProxy(long); err == nil || !strings.Contains(err.Error(), "over the 103") {
		t.Fatalf("an over-long socket path was not refused: %v", err)
	}
}

func TestGoproxyCacheKeptWhileAttemptsAreLive(t *testing.T) {
	m, v := openProxy(t, "http://unused.invalid")
	if err := os.WriteFile(filepath.Join(v.Root(), "goproxy", "blob"), make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.Budgets = []cache.Budget{{Name: "goproxy", Prefix: "goproxy", Bytes: 1 << 20, Evict: true, Class: venue.ClassCache, KeepWhileLive: true}}
	m.mu.Lock()
	m.t.entries["live"] = &Entry{Attempt: "live", State: StateRunning}
	m.mu.Unlock()
	rep, err := m.Caches(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Evicted) != 0 || !v.IsOpen(venue.KindVolume, "goproxy") {
		t.Fatalf("the proxy cache was evicted under a live attempt: %+v", rep.Evicted)
	}
	m.mu.Lock()
	m.t.entries["live"].State = StateExited
	m.mu.Unlock()
	rep, err = m.Caches(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Evicted) != 1 || v.IsOpen(venue.KindVolume, "goproxy") {
		t.Fatalf("with no live attempt, the over-budget cache was kept: %+v", rep)
	}
}
