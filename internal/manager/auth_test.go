package manager

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// The kernel's account of the peer is the connecting process's uid.
func TestPeerUIDIsTheConnectingProcess(t *testing.T) {
	sock := filepath.Join(shortDir(t), "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("unix", sock)
		if err == nil {
			time.Sleep(200 * time.Millisecond)
			c.Close()
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uid, err := peerUID(c)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Fatalf("peerUID = %d, %v; want %d", uid, err, os.Getuid())
	}
	if _, err := peerUID(&net.TCPConn{}); err == nil {
		t.Fatal("a non-Unix connection gave a uid")
	}
}

// Allow lists are checked when the manager opens, and fail closed.
func TestClassAllowIsCheckedAtOpen(t *testing.T) {
	ci, py := JobClass{Class: capacity.CI}, JobClass{Class: capacity.CI}
	py.Class.Name = "py"
	me := uint32(os.Getuid())
	for name, tc := range map[string]struct {
		cfg  Config
		want string
	}{
		"a list for a class that does not exist": {Config{Classes: []JobClass{ci}, ClassAllow: map[string][]uint32{"nope": {me}}}, "does not have"},
		"an empty list":                          {Config{Classes: []JobClass{ci}, ClassAllow: map[string][]uint32{"ci": {}}}, "empty allow list"},
		"two classes on the control socket, one without a list": {
			Config{Classes: []JobClass{ci, py}, CtlSocket: "/x/ctl.sock", ClassAllow: map[string][]uint32{"ci": {me}}}, `class "py" needs an allow list`},
	} {
		if err := checkClassAllow(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, cfg := range map[string]Config{
		"one class, no list":                {Classes: []JobClass{ci}, CtlSocket: "/x/ctl.sock"},
		"two classes, no control socket":    {Classes: []JobClass{ci, py}},
		"two classes on the socket, listed": {Classes: []JobClass{ci, py}, CtlSocket: "/x/ctl.sock", ClassAllow: map[string][]uint32{"ci": {me}, "py": {me + 1}}},
	} {
		if err := checkClassAllow(cfg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// openTwoClassSockets is a manager with classes ci (this test's uid may use
// it) and py (another uid's), serving both sockets.
func openTwoClassSockets(t *testing.T) (*Manager, *Client, *Client) {
	t.Helper()
	return openNamedClassSockets(t, "ci")
}

func openNamedClassSockets(t *testing.T, ciName string) (*Manager, *Client, *Client) {
	t.Helper()
	base := shortDir(t)
	root := filepath.Join(base, "root")
	os.Mkdir(root, 0o700)
	v, err := venue.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(base, "run")
	os.Mkdir(run, 0o750)
	os.Chmod(run, 0o750)
	py := capacity.CI
	py.Name = "py"
	ci := capacity.CI
	ci.Name = ciName
	me := uint32(os.Getuid())
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 8, MemoryBytes: 16 * capacity.GiB}, CtlSocket: filepath.Join(run, "ctl.sock"),
		Classes:    []JobClass{{Class: ci}, {Class: py}},
		ClassAllow: map[string][]uint32{ciName: {me}, "py": {me + 1}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { m.Serve(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped; m.Close() })
	waitTestSocket(t, filepath.Join(run, "ctl.sock"), 0o660)
	return m, NewClient(root), NewSocketClient(filepath.Join(run, "ctl.sock"))
}

// Through the control socket, a caller sees and uses only the classes its uid
// is allowed; the owner's socket is not scoped (the council's ruling of
// 2026-10-01 23:11Z §3).
func TestControlSocketScopesEveryAttemptRouteByClass(t *testing.T) {
	m, owner, ctl := openTwoClassSockets(t)
	mine, theirs := terminalEntry("mine-1"), terminalEntry("theirs-1")
	theirs.Class.Name = "py"
	m.mu.Lock()
	m.t.entries["mine-1"], m.t.entries["theirs-1"] = &mine, &theirs
	m.mu.Unlock()

	var list []Entry
	if err := ctl.Do("GET", "/v1/attempts", nil, &list); err != nil || len(list) != 1 || list[0].Attempt != "mine-1" {
		t.Fatalf("list through the control socket: %+v, %v", list, err)
	}
	if err := owner.Do("GET", "/v1/attempts", nil, &list); err != nil || len(list) != 2 {
		t.Fatalf("list through the owner's socket: %d entries, %v", len(list), err)
	}
	for _, p := range []string{"/v1/attempts/theirs-1", "/v1/attempts/theirs-1/result", "/v1/attempts/theirs-1/log",
		"/v1/attempts/theirs-1/pins", "/v1/attempts/theirs-1/outputs/x"} {
		if err := ctl.Do("GET", p, nil, nil); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), `may not use class "py"`) {
			t.Errorf("GET %s through the control socket: %v, want 403", p, err)
		}
	}
	if err := ctl.Do("POST", "/v1/attempts/theirs-1/stop", nil, nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("stop through the control socket: %v, want 403", err)
	}
	if err := ctl.Do("GET", "/v1/attempts/mine-1", nil, nil); err != nil {
		t.Errorf("my own attempt: %v", err)
	}
	if err := owner.Do("GET", "/v1/attempts/theirs-1", nil, nil); err != nil {
		t.Errorf("the owner reads any attempt: %v", err)
	}
	// A start in py is refused before anything is done; one in ci passes the
	// check and stops only at the unentitled test binary.
	err := ctl.Do("POST", "/v1/attempts", StartRequest{ID: "s-py", Class: "py", Command: []string{"true"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "py") {
		t.Fatalf("a start in py: %v, want 403", err)
	}
	if err := ctl.Do("POST", "/v1/attempts", StartRequest{ID: "s-ci", Command: []string{"true"}}, nil); err == nil || strings.Contains(err.Error(), "403") {
		t.Fatalf("a start in ci (the default): %v, want past the class check", err)
	}
	var c Capacity
	if err := ctl.Do("GET", "/v1/capacity", nil, &c); err != nil || len(c.Classes) != 1 || c.Classes[0].Name != "ci" {
		t.Fatalf("capacity through the control socket: %+v, %v", c.Classes, err)
	}
	for _, p := range []string{"/v1/caches", "/v1/reconcile", "/v1/vm-orphans"} {
		if err := ctl.Do("GET", p, nil, nil); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("host-wide %s through the control socket: %v, want 403", p, err)
		}
		if err := owner.Do("GET", p, nil, nil); err != nil {
			t.Errorf("host-wide %s through the owner's socket: %v", p, err)
		}
	}
}

// An unidentified caller is refused (fail closed), and the starter's uid is
// in the signed result.
func TestUnidentifiedCallersAreRefusedAndTheStarterIsRecorded(t *testing.T) {
	m, _, _ := openTwoClassSockets(t)
	if m.mayUse(caller{known: false}, "ci") || m.mayUse(caller{uid: uint32(os.Getuid()) + 1, known: true}, "ci") {
		t.Fatal("ci was allowed to an unidentified caller, or to another uid")
	}
	if !m.mayUse(caller{owner: true}, "py") {
		t.Fatal("the owner's socket was scoped")
	}
	e := terminalEntry("by-1")
	uid := uint32(4242)
	e.StartedByUID = &uid
	if got := m.resultDoc(e)["started_by_uid"]; got != uid {
		t.Fatalf("started_by_uid = %v", got)
	}
}

// A retained attempt can name a class that is no longer configured. Absence
// from today's allow lists must not make its history or stop route public.
func TestControlSocketRefusesRetainedUnconfiguredClasses(t *testing.T) {
	m, owner, ctl := openNamedClassSockets(t, "current-ci")
	retired := terminalEntry("retired-1")
	retired.Class.Name = "retired-ci"
	legacy := terminalEntry("legacy-1")
	legacy.Class = capacity.Class{}
	m.mu.Lock()
	current := terminalEntry("current-1")
	current.Class.Name = "current-ci"
	m.t.entries[retired.Attempt] = &retired
	m.t.entries[legacy.Attempt] = &legacy
	m.t.entries[current.Attempt] = &current
	m.mu.Unlock()

	var list []Entry
	if err := ctl.Do("GET", "/v1/attempts", nil, &list); err != nil || len(list) != 1 || list[0].Attempt != current.Attempt {
		t.Fatalf("control history exposed a retired class: %+v, %v", list, err)
	}
	if err := owner.Do("GET", "/v1/attempts", nil, &list); err != nil || len(list) != 3 {
		t.Fatalf("owner lost retained history: %+v, %v", list, err)
	}
	for _, id := range []string{retired.Attempt, legacy.Attempt} {
		for _, suffix := range []string{"", "/result", "/log", "/pins", "/outputs/x"} {
			if err := ctl.Do("GET", "/v1/attempts/"+id+suffix, nil, nil); err == nil || !strings.Contains(err.Error(), "403") {
				t.Errorf("retained attempt %s%s accessible: %v", id, suffix, err)
			}
		}
		if err := ctl.Do("POST", "/v1/attempts/"+id+"/stop", nil, nil); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("retained attempt %s stop reached handler: %v", id, err)
		}
		if err := owner.Do("GET", "/v1/attempts/"+id, nil, nil); err != nil {
			t.Errorf("owner cannot read retained attempt %s: %v", id, err)
		}
	}
}

func TestSingleConfiguredClassWithoutAllowListDoesNotAllowOtherClasses(t *testing.T) {
	m := &Manager{cfg: Config{Classes: []JobClass{{Class: capacity.CI}}}}
	c := caller{uid: uint32(os.Getuid()), known: true}
	if !m.mayUse(c, "ci") {
		t.Fatal("configured single-class compatibility lost")
	}
	if m.mayUse(c, "unconfigured") {
		t.Fatal("missing allow list admitted an unconfigured class")
	}
	if !m.mayUse(caller{owner: true}, "unconfigured") {
		t.Fatal("owner lost access to retained unconfigured classes")
	}
}
