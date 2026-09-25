package manager

import (
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

func TestFindVMOrphans(t *testing.T) {
	entries := []Entry{
		// Linked: its own service is accounted for.
		{Attempt: "linked", State: StateRunning, Start: "Thu Sep 25 10:00:00 2026",
			Peaks: Peaks{VMPID: 10, VMStart: "Thu Sep 25 10:00:01 2026"}},
		// Unlinked and just started: a service in its window may be its own.
		{Attempt: "starting", State: StateStarting, Start: "Thu Sep 25 11:00:00 2026"},
		// Ended: accounts for nothing.
		{Attempt: "done", State: StateExited, Start: "Thu Sep 25 09:00:00 2026",
			Peaks: Peaks{VMPID: 12, VMStart: "Thu Sep 25 09:00:01 2026"}},
	}
	ps := []proc.Process{
		vmProc(10, "Thu Sep 25 10:00:01 2026", 0),                                   // linked
		vmProc(11, "Thu Sep 25 11:00:30 2026", 0),                                   // pending, in "starting"'s window
		vmProc(12, "Thu Sep 25 09:00:01 2026", 0),                                   // its attempt ended: orphan
		vmProc(13, "Thu Sep 25 08:00:00 2026", 0),                                   // nobody's: orphan
		vmProc(14, "Thu Sep 25 11:05:00 2026", 0),                                   // after the window: orphan
		{PID: 15, UID: me + 1, Start: "Thu Sep 25 08:00:00 2026", Args: VMService},  // another user's
		{PID: 16, UID: me, Start: "Thu Sep 25 08:00:00 2026", Args: "/bin/sleep 9"}, // not a VM service
		// pid 10 reused by another VM service: not the linked one.
	}
	got := FindVMOrphans(entries, ps, me)
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	if len(pids) != 3 || pids[0] != 12 || pids[1] != 13 || pids[2] != 14 {
		t.Fatalf("orphans %v, want [12 13 14]", pids)
	}
	reused := []proc.Process{vmProc(10, "Thu Sep 25 12:00:00 2026", 0)}
	if o := FindVMOrphans(entries[:1], reused, me); len(o) != 1 {
		t.Fatalf("a reused pid was taken for the linked service: %v", o)
	}
}

// An entry whose start cannot be read keeps every service pending: the
// check never calls a guest an orphan on a guess.
func TestFindVMOrphansIsConservative(t *testing.T) {
	entries := []Entry{{Attempt: "odd", State: StateRunning, Start: "T1"}}
	if o := FindVMOrphans(entries, []proc.Process{vmProc(13, "Thu Sep 25 08:00:00 2026", 0)}, me); len(o) != 0 {
		t.Fatalf("orphans %v with an unreadable start", o)
	}
}

// The sweep records each orphan once, marks it gone when it leaves, and
// never signals it (the sweep has no signalling path at all).
func TestOrphanSweepRecordsOnceAndMarksGone(t *testing.T) {
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openWatch(t, procs)
	orphan := proc.Process{PID: 13, UID: me, Start: "Thu Sep 25 08:00:00 2026", Args: VMService}
	procs.lists, procs.n = [][]proc.Process{{orphan}}, 0
	m.orphanSweep(nil, true)
	m.orphanSweep(nil, true)
	o := m.VMOrphans()
	if len(o) != 1 || o[0].PID != 13 || o[0].Gone {
		t.Fatalf("orphans %+v", o)
	}
	procs.lists, procs.n = [][]proc.Process{nil}, 0
	m.orphanSweep(nil, true)
	if o := m.VMOrphans(); len(o) != 1 || !o[0].Gone {
		t.Fatalf("after it left: %+v", o)
	}
	// Throttled: without force, a sweep within orphanEvery does nothing.
	procs.lists, procs.n = [][]proc.Process{{{PID: 21, UID: me, Start: "Thu Sep 25 08:00:00 2026", Args: VMService}}}, 0
	m.orphanSweep(nil, false)
	if o := m.VMOrphans(); len(o) != 1 {
		t.Fatalf("a throttled sweep ran: %+v", o)
	}
	m.lastSweep = time.Now().Add(-orphanEvery)
	m.orphanSweep(nil, false)
	if o := m.VMOrphans(); len(o) != 2 {
		t.Fatalf("a due sweep did not run: %+v", o)
	}
}
