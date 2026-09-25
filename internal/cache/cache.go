// Package cache gives every cache in the data root a size budget and plans
// its eviction, oldest first. It only plans: the manager executes a plan
// through the ledger, so every eviction is recorded as a noted removal.
//
// An object is never evicted while it is in use: while it holds another open
// object (the image store holds live guests), while it is pinned by the
// running configuration (the kernel and base helpers boot from), or while
// its budget is kept rather than evicted (the manager's own directory).
package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// GiB is 2^30 bytes.
const GiB = int64(1) << 30

// Budget bounds the objects under one path prefix of the data root.
type Budget struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"` // a data-root path, or a directory followed by "/"
	Bytes  int64  `json:"bytes"`
	// Evict is false for a budget that is only reported, never enforced by
	// removal, because what it holds is in use for as long as Pomar runs.
	Evict bool `json:"evict"`
	// Class is the class of object the budget covers. Terminal attempt
	// records are attempt objects kept under a budget like a cache's.
	Class venue.Class `json:"class"`
	// KeepWhileLive keeps the budget's objects while any attempt is live:
	// they serve live attempts (the module proxy's cache).
	KeepWhileLive bool `json:"keep_while_live,omitempty"`
}

// Budgets are the data root's budgets. The figures are provisional, like the
// job class's, and are restated with the measured peaks.
var Budgets = []Budget{
	{Name: "kernels", Prefix: "kernels/", Bytes: 1 * GiB, Evict: true, Class: venue.ClassCache},
	{Name: "bases", Prefix: "bases/", Bytes: 8 * GiB, Evict: true, Class: venue.ClassCache},
	{Name: "image-store", Prefix: "store", Bytes: 16 * GiB, Evict: true, Class: venue.ClassCache},
	{Name: "mirrors", Prefix: "mirrors/", Bytes: 16 * GiB, Evict: true, Class: venue.ClassCache},
	{Name: "downloads", Prefix: "downloads/", Bytes: 4 * GiB, Evict: true, Class: venue.ClassCache},
	{Name: "goproxy", Prefix: "goproxy", Bytes: 8 * GiB, Evict: true, Class: venue.ClassCache, KeepWhileLive: true},
	{Name: "attempt-records", Prefix: "attempts/", Bytes: 4 * GiB, Evict: true, Class: venue.ClassAttempt},
	{Name: "manager", Prefix: "manager", Bytes: 1 * GiB, Evict: false, Class: venue.ClassCache},
	{Name: "tmp", Prefix: "tmp", Bytes: 4 * GiB, Evict: false, Class: venue.ClassCache},
}

func (b Budget) covers(o venue.Object) bool {
	if o.Class != b.Class {
		return false
	}
	p := filepath.ToSlash(filepath.Clean(o.Path))
	if strings.HasSuffix(b.Prefix, "/") {
		return strings.HasPrefix(p, b.Prefix)
	}
	return p == b.Prefix
}

// Item is one budgeted object, sized, with the reason it may not be evicted
// (empty when it may).
type Item struct {
	Object venue.Object `json:"object"`
	Size   int64        `json:"size"`
	Keep   string       `json:"keep,omitempty"`
}

// Eviction is one planned removal.
type Eviction struct {
	Budget string       `json:"budget"`
	Object venue.Object `json:"object"`
	Size   int64        `json:"size"`
}

// Note is the ledger note an eviction's removal carries.
func (e Eviction) Note() string {
	return fmt.Sprintf("evicted: budget %s, oldest first, %d bytes", e.Budget, e.Size)
}

// Usage is one budget's state.
type Usage struct {
	Budget Budget `json:"budget"`
	Bytes  int64  `json:"bytes"`
	Items  []Item `json:"items"`
	// Over is the excess left after the plan's evictions; it is non-zero
	// only when what remains is all kept.
	Over int64 `json:"over"`
}

// Plan returns, for each budget, its usage and the evictions that bring it
// within its budget, oldest first by creation time; and the objects of a
// budgeted class that no budget covers.
func Plan(budgets []Budget, items []Item) (usage []Usage, evict []Eviction, unbudgeted []Item) {
	claimed := make([]bool, len(items))
	for _, b := range budgets {
		u := Usage{Budget: b}
		var mine []Item
		for i, it := range items {
			if b.covers(it.Object) {
				claimed[i] = true
				mine = append(mine, it)
				u.Bytes += it.Size
			}
		}
		sort.SliceStable(mine, func(i, j int) bool { return mine[i].Object.Created.Before(mine[j].Object.Created) })
		u.Items = mine
		left := u.Bytes
		if b.Evict {
			for _, it := range mine {
				if left <= b.Bytes {
					break
				}
				if it.Keep != "" {
					continue
				}
				evict = append(evict, Eviction{Budget: b.Name, Object: it.Object, Size: it.Size})
				left -= it.Size
			}
		}
		if left > b.Bytes {
			u.Over = left - b.Bytes
		}
		usage = append(usage, u)
	}
	for i, it := range items {
		if !claimed[i] && it.Object.Class == venue.ClassCache {
			unbudgeted = append(unbudgeted, it)
		}
	}
	return usage, evict, unbudgeted
}

// Contains reports whether open object o's path holds another open object.
func Contains(o venue.Object, open []venue.Object) bool {
	p := filepath.Clean(o.Path)
	if p == "" || p == "." {
		return false
	}
	for _, x := range open {
		q := filepath.Clean(x.Path)
		if x.Kind == o.Kind && x.ID == o.ID {
			continue
		}
		if strings.HasPrefix(q, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Size is the space a path's files occupy: allocated blocks, summed over the
// tree without following symlinks. A missing path is size 0. Clones that
// share blocks are each counted in full, which errs toward evicting.
func Size(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			if errors.Is(err, syscall.ENOENT) {
				return nil
			}
			return err
		}
		total += st.Blocks * 512
		return nil
	})
	return total, err
}

// BudgetFor returns the budget that covers o, if any.
func BudgetFor(budgets []Budget, o venue.Object) (Budget, bool) {
	for _, b := range budgets {
		if b.covers(o) {
			return b, true
		}
	}
	return Budget{}, false
}
