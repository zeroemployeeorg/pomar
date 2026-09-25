package manager

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/sign"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

type noProcs struct{}

func (noProcs) List() ([]proc.Process, error) { return nil, nil }

// The test binary is ad-hoc signed without the Virtualization entitlement,
// so using it as the helper must be refused before anything is created.
func TestStartRefusesUnentitledHelper(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	before := len(v.OpenObjects())
	if _, err := m.Start("a1", []string{"/bin/true"}, nil); !errors.Is(err, sign.ErrMissing) {
		t.Fatalf("Start with unentitled helper = %v, want sign.ErrMissing", err)
	}
	if got := len(v.OpenObjects()); got != before {
		t.Fatalf("open objects went from %d to %d; the refusal created something", before, got)
	}
	if len(m.List()) != 0 {
		t.Fatalf("table has entries after a refused start: %+v", m.List())
	}
	if un, err := v.Unaccounted(); err != nil || len(un) != 0 {
		t.Fatalf("unaccounted after refusal: %v, %v", un, err)
	}
}

func openAdmission(t *testing.T, h capacity.Host, sp venue.Space) (*Manager, *venue.Venue) {
	t.Helper()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: h, Space: func() (venue.Space, error) { return sp, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, v
}

func refusedFor(err error) string {
	var r *capacity.Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// A refused claim creates nothing, and a claim that passes admission goes on
// to the entitlement check (which the unentitled test binary then fails).
func TestStartAdmission(t *testing.T) {
	roomy := venue.Space{Used: 10 * capacity.GiB, Avail: 500 * capacity.GiB, Shared: true}
	for _, tc := range []struct {
		name   string
		h      capacity.Host
		sp     venue.Space
		live   *Entry
		reason string
	}{
		{"admitted", capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, roomy, nil, ""},
		{"no cpu slot", capacity.Host{CPUSlots: 1, MemoryBytes: 8 * capacity.GiB}, roomy, nil, capacity.ReasonCPU},
		{"live attempt holds the slot", capacity.Host{CPUSlots: 3, MemoryBytes: 8 * capacity.GiB}, roomy,
			&Entry{Attempt: "live", State: StateRunning, Class: capacity.CI}, capacity.ReasonCPU},
		{"entry from before classes counts as CI", capacity.Host{CPUSlots: 3, MemoryBytes: 8 * capacity.GiB}, roomy,
			&Entry{Attempt: "old", State: StateRunning}, capacity.ReasonCPU},
		{"ended attempt holds nothing", capacity.Host{CPUSlots: 3, MemoryBytes: 8 * capacity.GiB}, roomy,
			&Entry{Attempt: "done", State: StateExited, Class: capacity.CI}, ""},
		{"no memory slot", capacity.Host{CPUSlots: 4, MemoryBytes: capacity.GiB / 2}, roomy, nil, capacity.ReasonMemory},
		{"disk below peak plus headroom", capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB},
			venue.Space{Avail: 12 * capacity.GiB}, nil, capacity.ReasonDisk}, // one claim is 3 + 10 GiB
		{"fill floor on a shared disk", capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB},
			venue.Space{Used: 80 * capacity.GiB, Avail: 20 * capacity.GiB, Shared: true}, nil, capacity.ReasonFloor},
		{"no floor on a dedicated volume", capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB},
			venue.Space{Used: 80 * capacity.GiB, Avail: 20 * capacity.GiB}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, v := openAdmission(t, tc.h, tc.sp)
			if tc.live != nil {
				m.t.entries[tc.live.Attempt] = tc.live
			}
			before := len(v.OpenObjects())
			_, err := m.Start("a1", []string{"/bin/true"}, nil)
			if tc.reason == "" {
				if !errors.Is(err, sign.ErrMissing) {
					t.Fatalf("Start = %v, want admission to pass and the entitlement check to refuse", err)
				}
			} else if got := refusedFor(err); got != tc.reason {
				t.Fatalf("Start = %v, want a %s refusal", err, tc.reason)
			}
			if got := len(v.OpenObjects()); got != before {
				t.Fatalf("open objects went from %d to %d", before, got)
			}
			if _, ok := m.t.entries["a1"]; ok {
				t.Fatal("refused attempt is in the table")
			}
		})
	}
}

func TestCapacityReport(t *testing.T) {
	m, _ := openAdmission(t, capacity.Host{CPUSlots: 14, MemoryBytes: 40 * capacity.GiB},
		venue.Space{Used: 100 * capacity.GiB, Avail: 900 * capacity.GiB, Shared: true})
	c, err := m.Capacity()
	if err != nil {
		t.Fatal(err)
	}
	// CI (r12): 7 by CPU; 17 by memory (2 GiB + 300 MiB); 69 by disk; the floor allows
	// 750 GiB more, 53 claims of 14 GiB.
	if c.FitsIdle != 7 || !c.Class.Measured || c.Live != 0 {
		t.Fatalf("Capacity = %+v", c)
	}
}
