package capacity

import (
	"fmt"
	"math"

	"github.com/zeroemployeeorg/pomar/internal/hostpolicy"
)

// Resources uses checked integers: 1000 millicores is one logical/vCPU slot.
// Memory includes per-VM overhead; disk includes peak plus per-job headroom.
type Resources struct {
	CPUMillicores int64 `json:"cpu_millicores"`
	MemoryBytes   int64 `json:"memory_bytes"`
	DiskBytes     int64 `json:"disk_bytes"`
}

// Failure is a local claim-time predicate, not a persistent queue occurrence.
// This legacy response does not identify a host; a pooled wait export must bind
// host identity separately. No private job IDs/arguments are needed.
type Failure struct {
	Resource  string `json:"resource"`
	Reason    string `json:"reason"` // existing API reason retained for clients
	Unit      string `json:"unit"`
	Demand    int64  `json:"demand"`
	Available int64  `json:"available"`
	Deficit   int64  `json:"deficit"`
}

// Allocation describes all known nonterminal claims. Free is signed: negative
// means an already overcommitted vector, not a wrap or a fresh zero observation.
type Allocation struct {
	Reserved    Resources `json:"reserved"`
	Free        Resources `json:"free"`
	Demand      Resources `json:"demand"`
	FitsCurrent int       `json:"fits_current"`
	Failures    []Failure `json:"failures"`
	// DiskAllowance is Avail limited by the observed shared host fill policy.
	DiskAllowance int64                `json:"disk_allowance_bytes"`
	DiskFloor     int64                `json:"disk_floor_bytes"`
	FillLimit     hostpolicy.DiskLimit `json:"fill_limit"`
}

func resources(c Class) (Resources, error) {
	if err := c.validate(); err != nil {
		return Resources{}, err
	}
	return Resources{int64(c.VCPU) * 1000, c.MemoryClaim(), c.Claim()}, nil
}

func add(a, b Resources) (Resources, error) {
	if a.CPUMillicores > math.MaxInt64-b.CPUMillicores || a.MemoryBytes > math.MaxInt64-b.MemoryBytes || a.DiskBytes > math.MaxInt64-b.DiskBytes {
		return Resources{}, fmt.Errorf("capacity: retained resource sum overflows")
	}
	return Resources{a.CPUMillicores + b.CPUMillicores, a.MemoryBytes + b.MemoryBytes, a.DiskBytes + b.DiskBytes}, nil
}

// Evaluate is the common accounting for admission and current status. Class
// ceilings count only that class on THIS host, never all jobs or other hosts.
// It does not reserve anything; callers perform check + durable reservation under
// their own admission mutex. A live job is not an exclusive-host predicate.
func Evaluate(c Class, live []Class, h Host, d Disk) (Allocation, error) {
	a := Allocation{Failures: []Failure{}, FillLimit: d.FillLimit.Validated()}
	var err error
	if a.Demand, err = resources(c); err != nil {
		return a, err
	}
	if h.CPUSlots < 0 || int64(h.CPUSlots) > math.MaxInt64/1000 || h.MemoryBytes < 0 || d.Used < 0 || d.Avail < 0 || d.Used > math.MaxInt64-d.Avail {
		return a, fmt.Errorf("capacity: invalid host or disk counters")
	}
	same := int64(0)
	for _, other := range live {
		r, err := resources(other)
		if err != nil {
			return a, err
		}
		if a.Reserved, err = add(a.Reserved, r); err != nil {
			return a, err
		}
		if other.Name == c.Name {
			same++
		}
	}
	a.DiskAllowance = d.Avail
	floorAllowance := d.Avail
	if d.Shared {
		total := d.Used + d.Avail
		// floor(total*limit/100) without the overflow-prone multiplication.
		limit := int64(a.FillLimit.Percent)
		maxUsed := (total/100)*limit + (total%100)*limit/100
		a.DiskFloor = total - maxUsed
		floorAllowance = maxUsed - d.Used
		a.DiskAllowance = min(a.DiskAllowance, floorAllowance)
	}
	// A negative floor allowance can make a free disk subtraction underflow.
	if a.DiskAllowance < 0 && a.Reserved.DiskBytes > math.MaxInt64+a.DiskAllowance+1 {
		return Allocation{}, fmt.Errorf("capacity: free disk counter underflows")
	}
	a.Free = Resources{int64(h.CPUSlots)*1000 - a.Reserved.CPUMillicores,
		h.MemoryBytes - a.Reserved.MemoryBytes, a.DiskAllowance - a.Reserved.DiskBytes}
	var deficitError error
	fail := func(resource, reason, unit string, demand, available int64) {
		if demand > available {
			// All demand values are nonnegative. Do not overflow the deficit.
			if available < 0 && demand > math.MaxInt64+available {
				deficitError = fmt.Errorf("capacity: %s deficit overflows", resource)
				return
			}
			a.Failures = append(a.Failures, Failure{resource, reason, unit, demand, available, demand - available})
		}
	}
	if c.Concurrency > 0 {
		fail("CLASS_LIMIT", ReasonClassConcurrency, "attempts", 1, int64(c.Concurrency)-same)
	}
	cpuReason, memReason := ReasonCPU, ReasonMemory
	if h.Budget {
		cpuReason, memReason = ReasonHostBudget, ReasonHostBudget
	}
	fail("CPU_BUDGET", cpuReason, "millicores", a.Demand.CPUMillicores, a.Free.CPUMillicores)
	fail("MEMORY_BUDGET", memReason, "bytes", a.Demand.MemoryBytes, a.Free.MemoryBytes)
	fail("DISK_BUDGET", ReasonDisk, "bytes", a.Demand.DiskBytes, d.Avail-a.Reserved.DiskBytes)
	if d.Shared {
		if floorAllowance < 0 && a.Reserved.DiskBytes > math.MaxInt64+floorAllowance+1 {
			return Allocation{}, fmt.Errorf("capacity: fill floor counter underflows")
		}
		fail("DISK_FLOOR", ReasonFloor, "bytes", a.Demand.DiskBytes, floorAllowance-a.Reserved.DiskBytes)
	}
	if deficitError != nil {
		return Allocation{}, deficitError
	}
	if len(a.Failures) == 0 {
		n := min(a.Free.CPUMillicores/a.Demand.CPUMillicores, a.Free.MemoryBytes/a.Demand.MemoryBytes, a.Free.DiskBytes/a.Demand.DiskBytes)
		if c.Concurrency > 0 {
			n = min(n, int64(c.Concurrency)-same)
		}
		a.FitsCurrent = int(n) // bounded by checked Host.CPUSlots, an int
	}
	return a, nil
}
