// Package capacity decides, when an attempt is claimed, whether the host can
// take it: free CPU and memory slots for its job class, and free disk for the
// class's peak plus fixed headroom. It decides only at claim time. A running
// attempt is never refused for capacity.
package capacity

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GiB is 2^30 bytes.
const GiB = int64(1) << 30

// HeadroomBytes is added to every claim's disk peak.
const HeadroomBytes = 10 * GiB

// MaxFillPercent is the fill floor kept while the data root shares its
// container with the operator's data.
const MaxFillPercent = 85

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
}

// VMOverhead is the per-guest memory the host pays above the cap: measured
// at 175 MB for 1 and 2 GiB guests; the elders set 0.3 GB (r12 §2).
const VMOverhead = 300 << 20

// CI is the CI job class as the elders set it from the measurements
// (ruling r12 §2): 2 vCPU, 2 GiB plus the VM overhead, a 3 GiB disk peak.
// The 1-vCPU shape was refused for the measured gate: single-vCPU guests
// failed its timing tests under load.
var CI = Class{Name: "ci", VCPU: 2, MemoryBytes: 2 * GiB, VMOverheadBytes: VMOverhead, DiskPeakBytes: 3 * GiB, Measured: true}

// MemoryClaim returns the memory a class's attempt claims at admission.
func (c Class) MemoryClaim() int64 { return c.MemoryBytes + c.VMOverheadBytes }

// Claim returns the disk a class's attempt claims at admission.
func (c Class) Claim() int64 { return c.DiskPeakBytes + HeadroomBytes }

func (c Class) validate() error {
	if c.Name == "" || c.VCPU < 1 || c.MemoryBytes < 1 || c.DiskPeakBytes < 0 {
		return fmt.Errorf("capacity: invalid class %+v", c)
	}
	return nil
}

// Host is what attempts may claim in total on this host.
type Host struct {
	CPUSlots    int   `json:"cpu_slots"`
	MemoryBytes int64 `json:"memory_bytes"`
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
	Shared bool
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
)

// Refusal says which resource refused a claim.
type Refusal struct {
	Reason string
	Detail string
}

func (r *Refusal) Error() string { return "capacity: refused (" + r.Reason + "): " + r.Detail }
func (r *Refusal) Unwrap() error { return ErrRefused }

// Admit decides whether an attempt of class c fits beside the live claims.
// Every live claim keeps its full disk claim, although part of it may already
// be written and so already missing from Avail: counting it twice errs toward
// refusing.
func Admit(c Class, live []Class, h Host, d Disk) error {
	if err := c.validate(); err != nil {
		return err
	}
	cpu, mem, disk := c.VCPU, c.MemoryClaim(), c.Claim()
	same := 1
	for _, l := range live {
		cpu += l.VCPU
		mem += l.MemoryClaim()
		if l.Name == c.Name {
			same++
		}
		disk += l.Claim()
	}
	if c.Concurrency > 0 && same > c.Concurrency {
		return &Refusal{ReasonClassConcurrency, fmt.Sprintf("%d attempts of class %s with this one, measured limit %d", same, c.Name, c.Concurrency)}
	}
	if cpu > h.CPUSlots {
		return &Refusal{ReasonCPU, fmt.Sprintf("%d vCPU claimed with this attempt, %d slots", cpu, h.CPUSlots)}
	}
	if mem > h.MemoryBytes {
		return &Refusal{ReasonMemory, fmt.Sprintf("%s claimed with this attempt, %s of slots", gib(mem), gib(h.MemoryBytes))}
	}
	if disk > d.Avail {
		return &Refusal{ReasonDisk, fmt.Sprintf("%s claimed with this attempt (peak + %s each), %s free", gib(disk), gib(HeadroomBytes), gib(d.Avail))}
	}
	if d.Shared {
		// The fill once every claim is written in full must stay at or
		// under the floor.
		total := d.Used + d.Avail
		if total <= 0 || (d.Used+disk)*100 > MaxFillPercent*total {
			return &Refusal{ReasonFloor, fmt.Sprintf("%s used + %s claimed would exceed %d%% of %s", gib(d.Used), gib(disk), MaxFillPercent, gib(total))}
		}
	}
	return nil
}

// Fits reports how many attempts of class c fit on an idle host: R-34's
// min(CPU slots, memory slots, free ÷ (peak + headroom)), with the fill floor
// on a shared disk. It is a plan figure only when c is measured.
func Fits(c Class, h Host, d Disk) int {
	n := 0
	var live []Class
	for Admit(c, live, h, d) == nil {
		n++
		live = append(live, c)
	}
	return n
}

func gib(b int64) string { return strconv.FormatFloat(float64(b)/float64(GiB), 'f', 1, 64) + " GiB" }
