package manager

import (
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// seqProcs returns its listings in turn, then the last one forever. before
// runs ahead of each listing.
type seqProcs struct {
	lists  [][]proc.Process
	n      int
	before func(n int)
}

func (s *seqProcs) List() ([]proc.Process, error) {
	if s.before != nil {
		s.before(s.n)
	}
	i := min(s.n, len(s.lists)-1)
	s.n++
	return s.lists[i], nil
}

func openWatch(t *testing.T, procs *seqProcs) *Manager {
	t.Helper()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Venue: v, HostBin: hostBin, Procs: procs, UID: me, Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func helperProc(attempt string, pid int) proc.Process {
	return proc.Process{PID: pid, UID: me, Start: "T1", Args: helperArgs(attempt)}
}

func state(m *Manager, id string) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.entries[id].State
}

// The race seen on the bounded volume: a helper that Start adds while the
// watcher's listing is being taken must not be judged against that listing.
func TestPollDoesNotJudgeEntriesNewerThanItsListing(t *testing.T) {
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openWatch(t, procs)
	m.t.entries["old"] = &Entry{Attempt: "old", PID: 100, Start: "T1", State: StateRunning}
	procs.lists = [][]proc.Process{{helperProc("old", 100)}}
	procs.n = 0
	procs.before = func(n int) {
		if n == 0 {
			m.mu.Lock()
			m.t.entries["new"] = &Entry{Attempt: "new", PID: 101, Start: "T1", State: StateStarting}
			m.mu.Unlock()
		}
	}
	m.poll()
	if s := state(m, "new"); s != StateStarting {
		t.Fatalf("an entry added during the listing was judged: %s", s)
	}
	if s := state(m, "old"); s != StateRunning {
		t.Fatalf("old = %s", s)
	}
}

func TestPollConfirmsAnEndWithAFreshListing(t *testing.T) {
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openWatch(t, procs)
	m.t.entries["a"] = &Entry{Attempt: "a", PID: 100, Start: "T1", State: StateRunning}
	// The first listing misses the helper; the second shows it.
	procs.lists = [][]proc.Process{nil, {helperProc("a", 100)}}
	procs.n = 0
	m.poll()
	if s := state(m, "a"); s != StateRunning {
		t.Fatalf("one missed listing ended the attempt: %s", s)
	}
	// Two listings without it: the end is recorded.
	procs.lists = [][]proc.Process{nil}
	procs.n = 0
	m.poll()
	if s := state(m, "a"); s != StateLost {
		t.Fatalf("after two agreeing listings: %s, want lost", s)
	}
}

func TestPollRequiresTheSameUID(t *testing.T) {
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openWatch(t, procs)
	m.t.entries["a"] = &Entry{Attempt: "a", PID: 100, Start: "T1", State: StateRunning}
	p := helperProc("a", 100)
	p.UID = me + 1
	procs.lists = [][]proc.Process{{p}}
	procs.n = 0
	m.poll()
	if s := state(m, "a"); s != StateLost {
		t.Fatalf("a helper under another uid kept the attempt live: %s", s)
	}
}
