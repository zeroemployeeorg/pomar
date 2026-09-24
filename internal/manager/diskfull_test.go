package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func openFull(t *testing.T, procs proc.Lister, avail int64, stall time.Duration) *Manager {
	t.Helper()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Venue: v, HostBin: hostBin, Procs: procs, UID: me, Poll: time.Hour, StallWait: stall,
		Host:  capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB},
		Space: func() (venue.Space, error) { return venue.Space{Avail: avail}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func writeStatus(t *testing.T, m *Manager, id, body string) {
	t.Helper()
	dir := filepath.Join(m.cfg.Venue.Root(), attemptsDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func entry(m *Manager, id string) Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.t.entries[id]
}

// The shape this macOS showed on a bounded volume: the guest got an I/O
// error and its command exited 1. The helper's free-space reading at the
// end names it.
func TestGuestIOErrorOnAFullHostIsHostDiskFull(t *testing.T) {
	m := openFull(t, &seqProcs{lists: [][]proc.Process{nil}}, 100*capacity.GiB, time.Hour)
	m.t.entries["a"] = &Entry{Attempt: "a", PID: 100, Start: "T1", State: StateRunning}
	writeStatus(t, m, "a", `{"phase":"exited","exit_code":"1","host_free_bytes":"120606720"}`)
	m.poll()
	e := entry(m, "a")
	if e.State != StateFailed || !strings.HasPrefix(e.Reason, capacity.ReasonHostDiskFull) || e.HostCondition != capacity.ReasonHostDiskFull {
		t.Fatalf("entry = %+v, want failed host-disk-full", e)
	}
	if e.ExitCode == nil || *e.ExitCode != 1 {
		t.Fatalf("exit code lost: %v", e.ExitCode)
	}
}

// Run 2 on the bounded volume: fsync got EIO, the command went on and
// exited 0. The exit 0 is not trusted.
func TestExitZeroOnAFullHostIsStillHostDiskFull(t *testing.T) {
	m := openFull(t, &seqProcs{lists: [][]proc.Process{nil}}, 100*capacity.GiB, time.Hour)
	m.t.entries["a"] = &Entry{Attempt: "a", PID: 100, Start: "T1", State: StateRunning}
	writeStatus(t, m, "a", `{"phase":"exited","exit_code":"0","host_free_bytes":"37441536"}`)
	m.poll()
	e := entry(m, "a")
	if e.State != StateFailed || e.Reason != capacity.ReasonHostDiskFull+" (exited 0)" {
		t.Fatalf("entry = %+v, want failed host-disk-full (exited 0)", e)
	}
	if e.ExitCode == nil || *e.ExitCode != 0 {
		t.Fatalf("exit code lost: %v", e.ExitCode)
	}
}

func TestFailureWithRoomIsNotHostDiskFull(t *testing.T) {
	m := openFull(t, &seqProcs{lists: [][]proc.Process{nil}}, 100*capacity.GiB, time.Hour)
	m.t.entries["a"] = &Entry{Attempt: "a", PID: 100, Start: "T1", State: StateRunning}
	writeStatus(t, m, "a", `{"phase":"exited","exit_code":"2","host_free_bytes":"53687091200"}`)
	m.poll()
	if e := entry(m, "a"); e.State != StateExited || e.HostCondition != "" {
		t.Fatalf("entry = %+v, want exited 2", e)
	}
}

// sleeper starts a child that stands in for a helper, so that the only
// process a stall can signal is the test's own child. It returns the pid and
// a channel that receives the child's end.
func sleeper(t *testing.T) (int, chan error) {
	t.Helper()
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	t.Cleanup(func() { child.Process.Kill() })
	return child.Process.Pid, done
}

// A helper whose guest has stopped making progress on a full host (a paused
// or hung guest) is stopped, and its end is named host-disk-full. The first
// sighting starts the clock; the stop comes once StallWait has passed.
func TestStalledAttemptOnAFullHostIsStopped(t *testing.T) {
	pid, done := sleeper(t)
	// The lister shows the helper only after Open: at Open it would be an
	// orphan, and reconciliation would signal it.
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openFull(t, procs, 100<<20, 20*time.Millisecond)
	procs.lists, procs.n = [][]proc.Process{{{PID: pid, UID: me, Start: "T1", Args: helperArgs("a")}}}, 0
	m.t.entries["a"] = &Entry{Attempt: "a", PID: pid, Start: "T1", State: StateRunning}
	writeStatus(t, m, "a", `{"phase":"running"}`)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(m.cfg.Venue.Root(), attemptsDir, "a", "status.json"), old, old)

	m.poll()
	if e := entry(m, "a"); e.State != StateRunning || e.HostCondition != capacity.ReasonHostDiskFull {
		t.Fatalf("at first sighting: %+v, want running and marked", e)
	}
	time.Sleep(40 * time.Millisecond)
	m.poll()
	if e := entry(m, "a"); e.State != StateStopping {
		t.Fatalf("after the stall: %+v, want stopping", e)
	}
	select {
	case err := <-done:
		ws, _ := err.(*exec.ExitError)
		if ws == nil || ws.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Fatalf("child ended with %v, want SIGTERM", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled helper was not signalled")
	}
	// The helper is gone and left no terminal status.
	procs.lists, procs.n = [][]proc.Process{nil}, 0
	m.poll()
	if e := entry(m, "a"); e.State != StateFailed || !strings.HasPrefix(e.Reason, capacity.ReasonHostDiskFull) {
		t.Fatalf("after it ended: %+v, want failed host-disk-full", e)
	}
}

// A live attempt that is still making progress is never stopped for a full
// host: it is marked, and left to end on its own.
func TestProgressingAttemptOnAFullHostIsLeftRunning(t *testing.T) {
	// Its pid is this test's own: were it ever signalled, the test would die
	// rather than a stranger.
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openFull(t, procs, 100<<20, time.Hour)
	procs.lists, procs.n = [][]proc.Process{{helperProc("a", os.Getpid())}}, 0
	m.t.entries["a"] = &Entry{Attempt: "a", PID: os.Getpid(), Start: "T1", State: StateRunning}
	writeStatus(t, m, "a", `{"phase":"running"}`)
	m.poll()
	if e := entry(m, "a"); e.State != StateRunning || e.HostCondition != capacity.ReasonHostDiskFull {
		t.Fatalf("entry = %+v, want running and marked", e)
	}
}

// A table written before host conditions had a disk_full flag; it loads as
// the host-disk-full condition.
func TestOldDiskFullFlagLoadsAsHostCondition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table.json")
	if err := os.WriteFile(path, []byte(`[{"attempt":"a","state":"running","disk_full":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	tb, err := loadTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if e := tb.entries["a"]; e.HostCondition != capacity.ReasonHostDiskFull || e.DiskFull {
		t.Fatalf("entry = %+v", e)
	}
}
