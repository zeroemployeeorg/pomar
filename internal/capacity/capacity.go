// Package capacity decides, when an attempt is claimed, whether the host can
// take it: free CPU and memory slots for its job class, and free disk for the
// class's peak plus fixed headroom. It decides only at claim time. A running
// attempt is never refused for capacity.
package capacity

import (
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/hostpolicy"
)

// GiB is 2^30 bytes.
const GiB = int64(1) << 30

// HeadroomBytes is added to every claim's disk peak.
const HeadroomBytes = 10 * GiB

// MaxFillPercent is the fill floor kept while the data root shares its
// container with the operator's data.
const MaxFillPercent = hostpolicy.DefaultMaxFillPercent

// DiskFullBytes is the free space below which the data root's filesystem
// counts as full. On the bounded test volume APFS stopped a guest's writes
// with about 115 MiB still reported free, so the line sits well above that.
const DiskFullBytes = 1 * GiB

// ReasonHostDiskFull names an attempt that failed because the host's
// filesystem filled under it, whatever shape the failure took: a guest I/O
// error, a paused guest or a hung one.
const ReasonHostDiskFull = "host-disk-full"

// Class is a job class: what each of its attempts is given and claims.
type Class struct {
	Name string `json:"name"`
	// VCPU and MemoryBytes are the guest's caps.
	VCPU        int   `json:"vcpu"`
	MemoryBytes int64 `json:"memory_bytes"`
	// VMOverheadBytes is what the host pays per guest above its memory cap
	// (the VM service's footprint minus the cap). Admission counts the cap
	// plus this.
	VMOverheadBytes int64 `json:"vm_overhead_bytes"`
	// DiskPeakBytes is the most one attempt writes to the data root.
	DiskPeakBytes int64 `json:"disk_peak_bytes"`
	// Concurrency is the class's measured concurrency limit on this host:
	// the largest N that ran the class's real gate clean twice (elders'
	// ruling r12 §2). Zero means not measured here: only the slots bound it.
	// It is per host, so it is set on the host, never in the code.
	Concurrency int `json:"concurrency"`
	// Measured is false while the figures are placeholders. No capacity
	// claim is made from a class that is not measured.
	Measured bool `json:"measured"`
	// TimeLimitSeconds is the class's wall-clock limit per attempt: an
	// attempt still live that long after it was admitted is stopped, and
	// ends timed-out (elders' ruling r30 §4.1 item 2). Zero means none.
	TimeLimitSeconds int64 `json:"time_limit_s,omitempty"`
	// LogCapBytes caps what is recorded, signed and served of an attempt's
	// output log; a longer log is marked truncated. Zero means the manager's
	// default (the elders' ruling of 2026-09-27 §2.2: a per-class cap).
	LogCapBytes int64 `json:"log_cap_bytes,omitempty"`
	// OutputCapBytes caps the total named files copied out of an attempt.
	// Zero preserves the manager's historical 64 MiB limit. The admitted
	// value is retained with the attempt, including across manager restarts.
	OutputCapBytes int64 `json:"output_cap_bytes,omitempty"`
}

// VMOverhead is the per-guest memory the host pays above the cap: measured
// at 175 MB for 1 and 2 GiB guests; the elders set 0.3 GB (r12 §2).
const VMOverhead = 300 << 20

// CI is the CI job class as the elders set it from the measurements
// (ruling r12 §2): 2 vCPU, 2 GiB plus the VM overhead, a 3 GiB disk peak.
// The 1-vCPU shape was refused for the measured gate: single-vCPU guests
// failed its timing tests under load.
var CI = Class{Name: "ci", VCPU: 2, MemoryBytes: 2 * GiB, VMOverheadBytes: VMOverhead, DiskPeakBytes: 3 * GiB, Measured: true,
	// The CI gate takes about 6 minutes, 7 at once, on the host it was measured on.
	TimeLimitSeconds: 30 * 60, LogCapBytes: 16 << 20}

// MemoryClaim returns the memory a class's attempt claims at admission.
func (c Class) MemoryClaim() int64 { return c.MemoryBytes + c.VMOverheadBytes }

// Claim returns the disk a class's attempt claims at admission.
func (c Class) Claim() int64 { return c.DiskPeakBytes + HeadroomBytes }

func (c Class) validate() error {
	if c.Name == "" || c.VCPU < 1 || int64(c.VCPU) > math.MaxInt64/1000 || c.MemoryBytes < 1 || c.VMOverheadBytes < 0 || c.MemoryBytes > math.MaxInt64-c.VMOverheadBytes || c.DiskPeakBytes < 0 || c.DiskPeakBytes > math.MaxInt64-HeadroomBytes || c.Concurrency < 0 || c.TimeLimitSeconds < 0 || c.LogCapBytes < 0 || c.OutputCapBytes < 0 || c.OutputCapBytes > 64<<20 {
		return fmt.Errorf("capacity: invalid class %+v", c)
	}
	return nil
}

// Host is what attempts may claim in total on this host.
type Host struct {
	CPUSlots    int   `json:"cpu_slots"`
	MemoryBytes int64 `json:"memory_bytes"`
	// Budget marks slots set as the operator's CI budget rather than read from
	// the host: a start beyond them is refused as host-budget.
	Budget bool `json:"budget,omitempty"`
}

// Reserved for the host itself before slots are counted. Provisional, like
// the class figures; the capacity plan states them with the measured peaks.
const (
	ReservedCPUs   = 2
	ReservedMemory = 8 * GiB
)

// HostFrom derives the slots from the host's logical CPUs and memory.
func HostFrom(ncpu int, mem int64) Host {
	h := Host{CPUSlots: ncpu - ReservedCPUs, MemoryBytes: mem - ReservedMemory}
	if h.CPUSlots < 0 {
		h.CPUSlots = 0
	}
	if h.MemoryBytes < 0 {
		h.MemoryBytes = 0
	}
	return h
}

// ReadHost reads hw.ncpu and hw.memsize.
func ReadHost() (Host, error) {
	out, err := exec.Command("sysctl", "-n", "hw.ncpu", "hw.memsize").Output()
	if err != nil {
		return Host{}, fmt.Errorf("capacity: sysctl: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return Host{}, fmt.Errorf("capacity: sysctl printed %q", out)
	}
	ncpu, err1 := strconv.Atoi(f[0])
	mem, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return Host{}, fmt.Errorf("capacity: sysctl printed %q", out)
	}
	return HostFrom(ncpu, mem), nil
}

// Disk is the data root's filesystem at claim time.
type Disk struct {
	Used, Avail int64
	// Shared keeps the fill floor on: the data root's free space is also
	// the operator's.
	Shared    bool
	FillLimit hostpolicy.DiskLimit
}

// ErrRefused wraps every admission refusal. The reason names the resource.
var ErrRefused = errors.New("capacity: refused")

// Refusal reasons.
const (
	ReasonCPU    = "cpu-slots"
	ReasonMemory = "memory-slots"
	ReasonDisk   = "disk-peak"
	ReasonFloor  = "fill-floor"
	// ReasonClassConcurrency: the class's measured concurrency limit on this
	// host is reached (r12 §2).
	ReasonClassConcurrency = "class-concurrency"
	// ReasonBaseUnverified: the class's base has not been verified since this
	// boot; the manager verifies bases when it starts, never inline.
	ReasonBaseUnverified = "base-unverified"
	// ReasonMirrorUnavailable: the start's mirror could not be synced, or is
	// not there. ReasonCommitUnknown: the mirror does not have the commit, or
	// does not reach it from a synced branch or pull request (a base, from its
	// branch). Both are decided before capacity is claimed (the elders'
	// ruling of 2026-09-27 17:21Z §4).
	ReasonMirrorUnavailable = "mirror-unavailable"
	ReasonCommitUnknown     = "commit-unknown"
	// ReasonDraining: the owner has set the manager draining. Attempts already
	// live finish; no new one is admitted until the drain is lifted (the
	// elders' ruling of 2026-09-27 17:21Z §2 item 2; a precondition of CI-4).
	ReasonDraining = "draining"
)

// Refusal says which resource refused a claim.
type Refusal struct {
	Reason string
	Detail string
	// Failures is complete for numerical capacity checks. Earlier clients keep
	// their original reason; new clients need not infer CPU versus memory from it.
	Failures []Failure
}

func (r *Refusal) Error() string { return "capacity: refused (" + r.Reason + "): " + r.Detail }
func (r *Refusal) Unwrap() error { return ErrRefused }

// Admit decides whether an attempt of class c fits beside the live claims.
// Every live claim keeps its full disk claim, although part of it may already
// be written and so already missing from Avail: counting it twice errs toward
// refusing.
func Admit(c Class, live []Class, h Host, d Disk) error {
	a, err := Evaluate(c, live, h, d)
	if err != nil || len(a.Failures) == 0 {
		return err
	}
	f := a.Failures[0]
	detail := fmt.Sprintf("%s: demand %d %s, available %d, deficit %d", f.Resource, f.Demand, f.Unit, f.Available, f.Deficit)
	if f.Reason == ReasonFloor {
		p := d.FillLimit.Validated()
		detail += fmt.Sprintf("; fill limit %d%% from %s", p.Percent, p.Source)
	}
	return &Refusal{Reason: f.Reason, Detail: detail, Failures: a.Failures}
}

// Fits reports how many attempts of class c fit on an idle host: R-34's
// min(CPU slots, memory slots, free ÷ (peak + headroom)), with the fill floor
// on a shared disk. It is a plan figure only when c is measured.
func Fits(c Class, h Host, d Disk) int {
	a, err := Evaluate(c, nil, h, d)
	if err != nil {
		return 0
	}
	return a.FitsCurrent
}

func gib(b int64) string { return strconv.FormatFloat(float64(b)/float64(GiB), 'f', 1, 64) + " GiB" }

// ReasonHostBudget names a start refused because it would exceed the host-wide
// CI budget, in vCPUs or memory, across every class (the elders' ruling of
// 2026-09-27 §1.2).
const ReasonHostBudget = "host-budget"

// WithBudget returns the host's slots cut to a CI budget of vcpu and mem. The
// budget must fit in what the host has after Pomar's reserve (ReservedCPUs,
// ReservedMemory), so the reserve stays a stated part of it.
func (h Host) WithBudget(vcpu int, mem int64) (Host, error) {
	if vcpu < 1 || mem < 1 {
		return Host{}, fmt.Errorf("capacity: a budget needs vCPUs and memory (%d, %d)", vcpu, mem)
	}
	if vcpu > h.CPUSlots || mem > h.MemoryBytes {
		return Host{}, fmt.Errorf("capacity: a budget of %d vCPU and %s exceeds the host after the reserve (%d vCPU, %s)",
			vcpu, gib(mem), h.CPUSlots, gib(h.MemoryBytes))
	}
	return Host{CPUSlots: vcpu, MemoryBytes: mem, Budget: true}, nil
}
