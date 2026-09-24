package manager

import (
	"path/filepath"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/cache"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// CacheReport is the budgets' state and what an eviction pass removed.
type CacheReport struct {
	Usage      []cache.Usage    `json:"usage"`
	Planned    []cache.Eviction `json:"planned"`
	Evicted    []cache.Eviction `json:"evicted"`
	Unbudgeted []cache.Item     `json:"unbudgeted,omitempty"`
	Errors     []string         `json:"errors,omitempty"`
}

// Caches sizes every budgeted object and, with apply, evicts oldest first
// until each budget is met. Only terminal attempts' records are evictable
// among attempt objects; their table entries go with them.
func (m *Manager) Caches(apply bool) (CacheReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cachesLocked(apply)
}

func (m *Manager) cachesLocked(apply bool) (CacheReport, error) {
	v := m.cfg.Venue
	open := v.OpenObjects()
	var pinned []string
	if m.cfg.Pinned != nil {
		for _, p := range m.cfg.Pinned() {
			if rel, err := filepath.Rel(v.Root(), p); err == nil && !strings.HasPrefix(rel, "..") {
				pinned = append(pinned, rel)
			}
		}
	}
	var items []cache.Item
	var rep CacheReport
	for _, o := range open {
		if o.Path == "" {
			continue
		}
		record := o.Class == venue.ClassAttempt && o.Kind == venue.KindVolume && strings.HasPrefix(o.ID, "attempt-")
		if o.Class != venue.ClassCache && !record {
			continue
		}
		size, err := cache.Size(filepath.Join(v.Root(), o.Path))
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			continue
		}
		it := cache.Item{Object: o, Size: size}
		switch {
		case record:
			e, ok := m.t.entries[strings.TrimPrefix(o.ID, "attempt-")]
			if !ok {
				it.Keep = "not in the attempt table"
			} else if !e.Terminal() {
				it.Keep = "attempt is live"
			}
		case cache.Contains(o, open):
			it.Keep = "holds an open object"
		case isPinned(o.Path, pinned):
			it.Keep = "pinned by the running configuration"
		}
		items = append(items, it)
	}
	usage, plan, unbudgeted := cache.Plan(m.cfg.Budgets, items)
	rep.Usage, rep.Unbudgeted, rep.Planned = usage, unbudgeted, plan
	if !apply {
		return rep, nil
	}
	for _, ev := range plan {
		var err error
		if id, ok := strings.CutPrefix(ev.Object.ID, "attempt-"); ok && ev.Object.Class == venue.ClassAttempt {
			err = m.removeLocked(id, ev.Note())
		} else {
			err = v.TeardownNote(ev.Object.Kind, ev.Object.ID, ev.Note())
		}
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			m.event("evict-error", "", 0, ev.Object.ID+": "+err.Error())
			continue
		}
		rep.Evicted = append(rep.Evicted, ev)
		m.event("evicted", "", 0, ev.Object.ID+": "+ev.Note())
	}
	return rep, nil
}

func isPinned(objPath string, pinned []string) bool {
	p := filepath.Clean(objPath)
	for _, q := range pinned {
		q = filepath.Clean(q)
		if q == p || strings.HasPrefix(q, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// evict runs an eviction pass and logs its errors; it never fails a caller.
func (m *Manager) evict() {
	rep, err := m.Caches(true)
	if err != nil {
		m.event("evict-error", "", 0, err.Error())
		return
	}
	for _, u := range rep.Usage {
		if u.Over > 0 {
			m.event("budget-over", "", 0, u.Budget.Name+": everything left is kept")
		}
	}
}
