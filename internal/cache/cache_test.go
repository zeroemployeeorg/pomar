package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

var t0 = time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

func obj(id, path string, class venue.Class, ageDays int) venue.Object {
	return venue.Object{Kind: venue.KindVolume, Class: class, ID: id, Path: path, Last: venue.OpCreated,
		Created: t0.Add(-time.Duration(ageDays) * 24 * time.Hour)}
}

func ids(ev []Eviction) []string {
	var out []string
	for _, e := range ev {
		out = append(out, e.Object.ID)
	}
	return out
}

func TestPlanEvictsOldestFirstUntilWithinBudget(t *testing.T) {
	b := []Budget{{Name: "mirrors", Prefix: "mirrors/", Bytes: 10, Evict: true, Class: venue.ClassCache}}
	items := []Item{
		{Object: obj("new", "mirrors/new.git", venue.ClassCache, 1), Size: 6},
		{Object: obj("old", "mirrors/old.git", venue.ClassCache, 9), Size: 4},
		{Object: obj("mid", "mirrors/mid.git", venue.ClassCache, 5), Size: 5},
	}
	u, ev, un := Plan(b, items)
	// 15 bytes against 10: the oldest (4) leaves 11, the next (5) leaves 6.
	if got := ids(ev); len(got) != 2 || got[0] != "old" || got[1] != "mid" {
		t.Fatalf("evictions = %v, want [old mid]", got)
	}
	if u[0].Bytes != 15 || u[0].Over != 0 || len(un) != 0 {
		t.Fatalf("usage = %+v, unbudgeted = %v", u, un)
	}
}

func TestPlanSkipsKeptItemsAndReportsOver(t *testing.T) {
	b := []Budget{{Name: "bases", Prefix: "bases/", Bytes: 5, Evict: true, Class: venue.ClassCache}}
	items := []Item{
		{Object: obj("pinned", "bases/a", venue.ClassCache, 9), Size: 8, Keep: "pinned"},
		{Object: obj("spare", "bases/b", venue.ClassCache, 1), Size: 2},
	}
	u, ev, _ := Plan(b, items)
	if got := ids(ev); len(got) != 1 || got[0] != "spare" {
		t.Fatalf("evictions = %v, want [spare]", got)
	}
	if u[0].Over != 3 {
		t.Fatalf("over = %d, want 3 (the kept 8 against 5)", u[0].Over)
	}
}

func TestPlanReportOnlyBudgetNeverEvicts(t *testing.T) {
	b := []Budget{{Name: "tmp", Prefix: "tmp", Bytes: 1, Evict: false, Class: venue.ClassCache}}
	u, ev, _ := Plan(b, []Item{{Object: obj("tmp", "tmp", venue.ClassCache, 1), Size: 5}})
	if len(ev) != 0 || u[0].Over != 4 {
		t.Fatalf("evictions = %v, over = %d", ev, u[0].Over)
	}
}

func TestPlanWithinBudgetEvictsNothing(t *testing.T) {
	_, ev, _ := Plan(Budgets, []Item{{Object: obj("kernel", "kernels/vmlinux", venue.ClassCache, 3), Size: 30 << 20}})
	if len(ev) != 0 {
		t.Fatalf("evictions = %v", ev)
	}
}

func TestPlanCoversByClassAndPrefix(t *testing.T) {
	items := []Item{
		{Object: obj("attempt-a", "attempts/a", venue.ClassAttempt, 1), Size: 1},
		{Object: obj("store", "store", venue.ClassCache, 1), Size: 1},
		{Object: obj("guest", "store/containers/a", venue.ClassAttempt, 1), Size: 1}, // not the store
		{Object: obj("stray", "elsewhere", venue.ClassCache, 1), Size: 1},
		{Object: obj("storeroom", "storeroom", venue.ClassCache, 1), Size: 1}, // exact prefix only
	}
	u, _, un := Plan(Budgets, items)
	by := map[string]int64{}
	for _, x := range u {
		by[x.Budget.Name] = x.Bytes
	}
	if by["attempt-records"] != 1 || by["image-store"] != 1 {
		t.Fatalf("usage = %v", by)
	}
	if len(un) != 2 || un[0].Object.ID != "stray" || un[1].Object.ID != "storeroom" {
		t.Fatalf("unbudgeted = %+v, want stray and storeroom", un)
	}
}

func TestEvictionNoteNamesTheBudget(t *testing.T) {
	e := Eviction{Budget: "mirrors", Size: 42}
	if e.Note() != "evicted: budget mirrors, oldest first, 42 bytes" {
		t.Fatal(e.Note())
	}
}

func TestContains(t *testing.T) {
	store := obj("store", "store", venue.ClassCache, 1)
	guest := venue.Object{Kind: venue.KindVM, ID: "a", Path: "store/containers/a"}
	other := obj("storeroom", "storeroom", venue.ClassCache, 1)
	if !Contains(store, []venue.Object{store, guest}) {
		t.Error("store holding a live guest not seen")
	}
	if Contains(store, []venue.Object{store, other}) {
		t.Error("a sibling with a common name prefix counted as contained")
	}
}

func TestSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	n, err := Size(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1<<20 || n > 2<<20 {
		t.Fatalf("Size = %d, want about 1 MiB (the symlink not followed)", n)
	}
	if n, err := Size(filepath.Join(dir, "missing")); err != nil || n != 0 {
		t.Fatalf("Size(missing) = %d, %v", n, err)
	}
}
