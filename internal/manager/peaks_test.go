package manager

import (
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func TestParseFootprint(t *testing.T) {
	out := "com.apple.Virtualization.VirtualMachine [14883]: 64-bit    Footprint: 1190 MB (16384 bytes per page)\n" +
		"    phys_footprint: 1190 MB\n    phys_footprint_peak: 1190 MB\n"
	if b, err := ParseFootprint(out); err != nil || b != 1190<<20 {
		t.Fatalf("ParseFootprint = %d, %v", b, err)
	}
	if b, _ := ParseFootprint("phys_footprint_peak: 1.5 GB"); b != 3<<29 {
		t.Fatalf("GB: %d", b)
	}
	if _, err := ParseFootprint("phys_footprint: 1 MB"); err == nil {
		t.Fatal("a footprint without its peak was accepted")
	}
}

func vmProc(pid int, start string, cpu time.Duration) proc.Process {
	return proc.Process{PID: pid, UID: me, Start: start, CPU: cpu, Args: VMService}
}

func openPeaks(t *testing.T, fp Footprinter, avail func() int64) *Manager {
	t.Helper()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Venue: v, HostBin: hostBin, Procs: &seqProcs{lists: [][]proc.Process{nil}}, UID: me, Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 8, MemoryBytes: 8 * capacity.GiB}, SampleEvery: time.Nanosecond, Footprint: fp,
		Space: func() (venue.Space, error) { return venue.Space{Avail: avail()}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestLinkVMs(t *testing.T) {
	m := openPeaks(t, func(int) (int64, error) { return 0, nil }, func() int64 { return 100 })
	m.t.entries["a"] = &Entry{Attempt: "a", Start: "Thu Sep 24 23:19:00 2026", State: StateRunning}
	m.t.entries["b"] = &Entry{Attempt: "b", Start: "Thu Sep 24 23:30:00 2026", State: StateRunning}
	m.t.entries["old"] = &Entry{Attempt: "old", Start: "Thu Sep 24 20:00:00 2026", State: StateExited}
	ps := []proc.Process{
		vmProc(10, "Thu Sep 24 23:19:02 2026", 0), // a's: 2 s after a's helper
		vmProc(11, "Thu Sep 24 23:30:01 2026", 0), // b's
		vmProc(12, "Thu Sep 24 23:10:00 2026", 0), // before both: nobody's
		{PID: 13, UID: me, Start: "Thu Sep 24 23:19:02 2026", Args: "/bin/sleep 5"},
		{PID: 14, UID: me + 1, Start: "Thu Sep 24 23:19:03 2026", Args: VMService}, // another user's
	}
	m.mu.Lock()
	m.linkVMs(ps)
	m.mu.Unlock()
	if p := m.t.entries["a"].Peaks; p.VMPID != 10 || p.VMStart != "Thu Sep 24 23:19:02 2026" {
		t.Errorf("a linked to %+v, want pid 10", p)
	}
	if p := m.t.entries["b"].Peaks; p.VMPID != 11 {
		t.Errorf("b linked to %+v, want pid 11", p)
	}
	if m.t.entries["old"].Peaks.VMPID != 0 {
		t.Error("an ended attempt was linked")
	}
}

// Two helpers started in the same second: neither VM service is guessed.
func TestLinkVMsRefusesToGuess(t *testing.T) {
	m := openPeaks(t, func(int) (int64, error) { return 0, nil }, func() int64 { return 100 })
	m.t.entries["a"] = &Entry{Attempt: "a", Start: "Thu Sep 24 23:19:00 2026", State: StateRunning}
	m.t.entries["b"] = &Entry{Attempt: "b", Start: "Thu Sep 24 23:19:00 2026", State: StateRunning}
	m.mu.Lock()
	m.linkVMs([]proc.Process{vmProc(10, "Thu Sep 24 23:19:01 2026", 0), vmProc(11, "Thu Sep 24 23:19:01 2026", 0)})
	m.mu.Unlock()
	for _, id := range []string{"a", "b"} {
		if p := m.t.entries[id].Peaks; p.VMPID != 0 {
			t.Errorf("%s linked to %d in an ambiguous window", id, p.VMPID)
		}
	}
}

func TestSamplePeaks(t *testing.T) {
	var fp int64 = 1100 << 20
	free := int64(50 << 30)
	m := openPeaks(t, func(pid int) (int64, error) {
		if pid != 10 {
			t.Errorf("footprint read for pid %d", pid)
		}
		return fp, nil
	}, func() int64 { return free })
	e := &Entry{Attempt: "a", Start: "Thu Sep 24 23:19:00 2026", State: StateRunning,
		Peaks: Peaks{VMPID: 10, VMStart: "Thu Sep 24 23:19:02 2026"}}
	m.t.entries["a"] = e
	t0 := time.Now()
	sample := func(at time.Time, cpu time.Duration) {
		m.mu.Lock()
		m.samplePeaks([]proc.Process{vmProc(10, "Thu Sep 24 23:19:02 2026", cpu)}, at)
		m.mu.Unlock()
	}
	sample(t0, 1*time.Second)
	free -= 3 << 30
	fp = 1190 << 20
	sample(t0.Add(2*time.Second), 4*time.Second) // 3 s of CPU in 2 s: 1.5 cores
	free += 1 << 30
	fp = 900 << 20 // a later, lower reading does not lower the peak
	sample(t0.Add(4*time.Second), 5*time.Second)
	p := e.Peaks
	if p.MemPeakBytes != 1190<<20 || p.DiskPeakBytes != 3<<30 || p.CPUSeconds != 5 || p.PeakCores != 1.5 || p.Samples != 3 {
		t.Fatalf("peaks = %+v", p)
	}
	// A reused pid (another start time) is not read.
	fp = 9 << 30
	m.mu.Lock()
	m.samplePeaks([]proc.Process{vmProc(10, "Fri Sep 25 01:00:00 2026", time.Hour)}, t0.Add(6*time.Second))
	m.mu.Unlock()
	if e.Peaks.MemPeakBytes != 1190<<20 || e.Peaks.Samples != 3 {
		t.Fatalf("a reused pid was sampled: %+v", e.Peaks)
	}
}
