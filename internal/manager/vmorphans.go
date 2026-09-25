package manager

import (
	"sort"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

// VMOrphan is a Virtualization VM service running under the manager's uid
// that no live attempt accounts for (elders' ruling r9 §2.2). A killed
// helper's VM service was seen to exit with it (POMAR-SOW-03 §30.3); this
// records every time that did not happen.
//
// It is reported, never signalled: the same uid may run other
// Virtualization users, and whether Pomar reaps a VM it did not link is a
// P-C decision.
type VMOrphan struct {
	PID       int       `json:"pid"`
	Start     string    `json:"start"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Gone is set once the process has left the listing.
	Gone bool `json:"gone,omitempty"`
}

// FindVMOrphans returns the VM services in ps under uid that are neither
// linked to a live entry nor inside an unlinked live entry's link window
// (where they may be about to link).
func FindVMOrphans(entries []Entry, ps []proc.Process, uid int) []proc.Process {
	linked := map[int]string{}
	var windows [][2]time.Time
	for _, e := range entries {
		if e.Terminal() {
			continue
		}
		if e.Peaks.VMPID != 0 {
			linked[e.Peaks.VMPID] = e.Peaks.VMStart
			continue
		}
		if hs, err := parseStart(e.Start); err == nil {
			windows = append(windows, [2]time.Time{hs, hs.Add(linkWindow)})
		} else {
			// An entry whose start cannot be read keeps every service
			// pending rather than risk calling its guest an orphan.
			windows = append(windows, [2]time.Time{time.Time{}, time.Unix(1<<62, 0)})
		}
	}
	var out []proc.Process
	for _, p := range ps {
		if p.UID != uid || p.Args != VMService {
			continue
		}
		if s, ok := linked[p.PID]; ok && s == p.Start {
			continue
		}
		vs, err := parseStart(p.Start)
		pending := false
		for _, w := range windows {
			if err != nil || (!vs.Before(w[0]) && !vs.After(w[1])) {
				pending = true
				break
			}
		}
		if !pending {
			out = append(out, p)
		}
	}
	return out
}

// checkVMOrphans records orphaned VM services, once each, and marks those
// that have gone. m.mu must be held.
func (m *Manager) checkVMOrphans(ps []proc.Process, now time.Time) {
	found := FindVMOrphans(m.t.list(), ps, m.cfg.UID)
	seen := map[int]bool{}
	for _, p := range found {
		seen[p.PID] = true
		o, ok := m.vmOrphans[p.PID]
		if !ok || o.Start != p.Start {
			m.vmOrphans[p.PID] = &VMOrphan{PID: p.PID, Start: p.Start, FirstSeen: now, LastSeen: now}
			m.event("vm-orphan", "", p.PID, "VM service with no live attempt, started "+p.Start+"; reported, not signalled")
			continue
		}
		o.LastSeen, o.Gone = now, false
	}
	for pid, o := range m.vmOrphans {
		if !seen[pid] && !o.Gone {
			if _, alive := proc.Find(ps, pid); !alive {
				o.Gone = true
				m.event("vm-orphan-gone", "", pid, "")
			}
		}
	}
}

// VMOrphans returns the orphaned VM services seen since the manager
// started, the live ones first.
func (m *Manager) VMOrphans() []VMOrphan {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]VMOrphan, 0, len(m.vmOrphans))
	for _, o := range m.vmOrphans {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Gone != out[j].Gone {
			return !out[i].Gone
		}
		return out[i].FirstSeen.Before(out[j].FirstSeen)
	})
	return out
}

// orphanEvery is how often the manager looks for orphaned VM services,
// whether or not any attempt is live.
const orphanEvery = 10 * time.Second

// orphanSweep runs checkVMOrphans at most every orphanEvery, or now with
// force. ps may be nil, in which case it lists the processes itself; an
// unreadable listing changes nothing.
func (m *Manager) orphanSweep(ps []proc.Process, force bool) {
	now := time.Now()
	if !force && now.Sub(m.lastSweep) < orphanEvery {
		return
	}
	if ps == nil {
		var err error
		if ps, err = m.cfg.Procs.List(); err != nil {
			return
		}
	}
	m.lastSweep = now
	m.mu.Lock()
	m.checkVMOrphans(ps, now)
	m.mu.Unlock()
}
