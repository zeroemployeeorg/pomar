package manager

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

// VMService is the process Virtualization.framework runs each guest in. It
// is not the helper: the helper's own CPU and memory say nothing about the
// guest (POMAR-SOW-03 §30.3), so an attempt's memory and CPU are read from
// its VM service (elders' ruling r9 §2).
const VMService = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"

// linkWindow bounds how long after its helper starts a VM service may start
// and still be linked to it.
const linkWindow = 2 * time.Minute

// Peaks are what an attempt was measured to use, for the capacity plan.
type Peaks struct {
	// VMPID and VMStart identify the attempt's VM service, once linked.
	VMPID   int    `json:"vm_pid,omitempty"`
	VMStart string `json:"vm_start,omitempty"`
	// MemPeakBytes is the VM service's phys_footprint_peak, the memory the
	// host pays for the guest.
	MemPeakBytes int64 `json:"mem_peak_bytes,omitempty"`
	// CPUSeconds is the VM service's CPU time when last sampled; PeakCores
	// is the highest CPU time per wall second between two samples.
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	PeakCores  float64 `json:"peak_cores,omitempty"`
	// DiskPeakBytes is the largest drop in the data root's free space seen
	// while the attempt was live. Other writers count too: it is exact only
	// with one attempt at a time on an otherwise idle volume.
	DiskPeakBytes int64 `json:"disk_peak_bytes,omitempty"`
	// Samples counts the footprint readings taken.
	Samples int `json:"samples,omitempty"`
	// Link records why a VM service could not be linked, if it could not.
	Link string `json:"link,omitempty"`
}

// Footprinter reads a process's phys_footprint_peak in bytes.
type Footprinter func(pid int) (int64, error)

var footprintPeak = regexp.MustCompile(`phys_footprint_peak:\s+([0-9.]+)\s*(B|KB|MB|GB)`)

// ParseFootprint reads the phys_footprint_peak line of footprint(1).
func ParseFootprint(out string) (int64, error) {
	m := footprintPeak.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("manager: no phys_footprint_peak in footprint output")
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	unit := map[string]float64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30}[m[2]]
	return int64(v * unit), nil
}

// FootprintTool runs /usr/bin/footprint.
func FootprintTool(pid int) (int64, error) {
	out, err := exec.Command("/usr/bin/footprint", "-p", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("manager: footprint: %w", err)
	}
	return ParseFootprint(string(out))
}

// parseStart reads ps's lstart ("Thu Sep 24 23:19:02 2026", local time).
func parseStart(s string) (time.Time, error) {
	return time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(s), " "), time.Local)
}

// linkVMs links live entries to their VM services. A service is linked to
// an attempt only when it is the one unlinked service that started within
// linkWindow after that attempt's helper and no other unlinked attempt
// could claim it; ambiguity is recorded and left unlinked rather than
// guessed. m.mu must be held.
func (m *Manager) linkVMs(ps []proc.Process) {
	linked := map[int]bool{}
	var unlinked []*Entry
	for _, e := range m.t.entries {
		if e.Terminal() {
			continue
		}
		if e.Peaks.VMPID != 0 {
			linked[e.Peaks.VMPID] = true
		} else {
			unlinked = append(unlinked, e)
		}
	}
	if len(unlinked) == 0 {
		return
	}
	var services []proc.Process
	for _, p := range ps {
		if p.UID == m.cfg.UID && p.Args == VMService && !linked[p.PID] {
			services = append(services, p)
		}
	}
	claims := map[int][]*Entry{}
	for _, e := range unlinked {
		hs, err := parseStart(e.Start)
		if err != nil {
			continue
		}
		for _, p := range services {
			vs, err := parseStart(p.Start)
			if err == nil && !vs.Before(hs) && vs.Sub(hs) <= linkWindow {
				claims[p.PID] = append(claims[p.PID], e)
			}
		}
	}
	for _, e := range unlinked {
		var mine []proc.Process
		for _, p := range services {
			if c := claims[p.PID]; len(c) == 1 && c[0] == e {
				mine = append(mine, p)
			}
		}
		switch {
		case len(mine) == 1:
			e.Peaks.VMPID, e.Peaks.VMStart, e.Peaks.Link = mine[0].PID, mine[0].Start, ""
			m.t.save()
			m.event("vm-linked", e.Attempt, mine[0].PID, mine[0].Start)
		case len(mine) > 1:
			e.Peaks.Link = "ambiguous: several VM services started in its window"
		}
	}
}

// samplePeaks updates live entries' peaks: memory and CPU from their linked
// VM service (after re-checking it is still the same process), disk from
// the data root's free space. It reports whether a VM service was read,
// the moment worth saving; disk figures ride along. m.mu must be held.
func (m *Manager) samplePeaks(ps []proc.Process, now time.Time) (sampled bool) {
	sp, spErr := m.cfg.Space()
	for _, e := range m.t.entries {
		if e.Terminal() {
			continue
		}
		if spErr == nil {
			if e.freeAtStart == 0 {
				e.freeAtStart = sp.Avail
			} else if d := e.freeAtStart - sp.Avail; d > e.Peaks.DiskPeakBytes {
				e.Peaks.DiskPeakBytes = d
			}
		}
		if e.Peaks.VMPID == 0 || now.Sub(e.lastSample) < m.cfg.SampleEvery {
			continue
		}
		p, ok := proc.Find(ps, e.Peaks.VMPID)
		if !ok || p.Start != e.Peaks.VMStart || p.Args != VMService {
			continue // gone or reused: keep what was read
		}
		if b, err := m.cfg.Footprint(p.PID); err == nil && b > e.Peaks.MemPeakBytes {
			e.Peaks.MemPeakBytes = b
		}
		e.Peaks.Samples++
		sampled = true
		cpu := p.CPU.Seconds()
		if !e.lastSample.IsZero() {
			if dt := now.Sub(e.lastSample).Seconds(); dt > 0 {
				if cores := (cpu - e.Peaks.CPUSeconds) / dt; cores > e.Peaks.PeakCores {
					e.Peaks.PeakCores = cores
				}
			}
		}
		e.Peaks.CPUSeconds = cpu
		e.lastSample = now
	}
	return sampled
}
