package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// shortDir is a temporary directory with a short path: a Unix socket's path is
// limited to 104 bytes on macOS, and t.TempDir's paths can be longer.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "pctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// serveWithCtl opens a manager on a fresh data root with a control socket in
// ctlDir (made with ctlMode), starts Serve, and waits for both sockets.
func serveWithCtl(t *testing.T, ctlMode os.FileMode) (owner, ctl *Client, ctlSock string, serveErr <-chan error) {
	t.Helper()
	base := shortDir(t)
	if err := os.Mkdir(filepath.Join(base, "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	v, err := venue.Open(filepath.Join(base, "root"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	ctlDir := filepath.Join(base, "run")
	if err := os.Mkdir(ctlDir, ctlMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ctlDir, ctlMode); err != nil { // Mkdir's mode is masked by the umask
		t.Fatal(err)
	}
	ctlSock = filepath.Join(ctlDir, "ctl.sock")
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, CtlSocket: ctlSock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		errs <- m.Serve(ctx)
		close(stopped)
	}()
	// Wait on stopped, not errs: a test may already have read Serve's error.
	t.Cleanup(func() {
		cancel()
		<-stopped
		m.Close()
	})
	for i := 0; i < 200; i++ {
		_, e1 := os.Stat(SocketPath(v.Root()))
		_, e2 := os.Stat(ctlSock)
		if e1 == nil && e2 == nil {
			break
		}
		select {
		case err := <-errs:
			errs <- err
			return nil, nil, ctlSock, errs
		case <-time.After(10 * time.Millisecond):
		}
	}
	return NewClient(v.Root()), NewSocketClient(ctlSock), ctlSock, errs
}

// The stream's control socket serves start, stop and reads, and nothing that
// removes a record or evicts a cache; the owner's socket keeps every route.
func TestCtlSocketServesTheStreamsRoutesOnly(t *testing.T) {
	owner, ctl, ctlSock, _ := serveWithCtl(t, 0o750)
	if owner == nil {
		t.Fatal("manager did not start")
	}
	fi, err := os.Stat(ctlSock)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o660 {
		t.Fatalf("control socket mode %o, want 660", got)
	}
	for _, path := range []string{"/v1/attempts", "/v1/capacity", "/v1/caches", "/v1/vm-orphans", "/v1/reconcile"} {
		if err := ctl.Do("GET", path, nil, nil); err != nil {
			t.Fatalf("control socket GET %s: %v", path, err)
		}
	}
	// A start reaches the manager's admission through the control socket: an
	// invalid id is refused by Start itself (400), not by the route table.
	err = ctl.Do("POST", "/v1/attempts", StartRequest{ID: "Bad ID", Command: []string{"true"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("control socket start with a bad id: %v, want a 400 from admission", err)
	}
	// Removing a record and evicting caches are the owner's only.
	for _, c := range []struct{ method, path string }{
		{"DELETE", "/v1/attempts/some-attempt"},
		{"POST", "/v1/caches/evict"},
	} {
		err := ctl.Do(c.method, c.path, nil, nil)
		if err == nil || !(strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "405")) {
			t.Fatalf("control socket %s %s: %v, want 404 or 405", c.method, c.path, err)
		}
	}
	// The owner still has them: the delete reaches Remove (409: no such attempt).
	if err := owner.Do("DELETE", "/v1/attempts/some-attempt", nil, nil); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("owner DELETE: %v, want 409 from Remove", err)
	}
	if err := owner.Do("POST", "/v1/caches/evict", nil, nil); err != nil {
		t.Fatalf("owner evict: %v", err)
	}
}

// A control-socket directory that others can enter is refused before the
// manager serves anything.
func TestCtlSocketRefusesADirectoryOpenToOthers(t *testing.T) {
	owner, _, _, errs := serveWithCtl(t, 0o755)
	if owner != nil {
		t.Fatal("manager served with a control socket directory open to others")
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "open to others") {
		t.Fatalf("Serve: %v, want a refusal naming the open directory", err)
	}
}
