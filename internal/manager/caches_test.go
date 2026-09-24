package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/cache"
	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

const mib = 1 << 20

// put ledgers an object and writes size bytes into it.
func put(t *testing.T, v *venue.Venue, k venue.Kind, c venue.Class, id, rel string, size int) {
	t.Helper()
	if err := v.Intent(k, c, id, rel, ""); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(v.Root(), rel)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Created(k, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct creation times: oldest first is by them
}

func TestCachesEvictOldestFirstThroughTheLedger(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	put(t, v, venue.KindVolume, venue.ClassCache, "mirror-old", "mirrors/old.git", 2*mib)
	put(t, v, venue.KindVolume, venue.ClassCache, "mirror-new", "mirrors/new.git", 2*mib)
	put(t, v, venue.KindImage, venue.ClassCache, "base-pinned", "bases/pinned", 2*mib)
	put(t, v, venue.KindImage, venue.ClassCache, "base-spare", "bases/spare", 2*mib)
	put(t, v, venue.KindImage, venue.ClassCache, "image-store", "store", 3*mib)
	put(t, v, venue.KindVM, venue.ClassAttempt, "live", "store/containers/live", 0)
	put(t, v, venue.KindVolume, venue.ClassAttempt, "attempt-done1", "attempts/done1", 2*mib)
	put(t, v, venue.KindVolume, venue.ClassAttempt, "attempt-live", "attempts/live", 2*mib)
	put(t, v, venue.KindVolume, venue.ClassAttempt, "attempt-done2", "attempts/done2", 2*mib)

	budgets := []cache.Budget{
		{Name: "mirrors", Prefix: "mirrors/", Bytes: 3 * mib, Evict: true, Class: venue.ClassCache},
		{Name: "bases", Prefix: "bases/", Bytes: 1 * mib, Evict: true, Class: venue.ClassCache},
		{Name: "image-store", Prefix: "store", Bytes: 1 * mib, Evict: true, Class: venue.ClassCache},
		{Name: "attempt-records", Prefix: "attempts/", Bytes: 5 * mib, Evict: true, Class: venue.ClassAttempt},
		{Name: "manager", Prefix: "manager", Bytes: 1 * mib, Class: venue.ClassCache},
		{Name: "tmp", Prefix: "tmp", Bytes: 1 * mib, Class: venue.ClassCache},
	}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, Budgets: budgets,
		Pinned: func() []string { return []string{filepath.Join(v.Root(), "bases/pinned/rootfs.ext4")} }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// The pass Open runs: mirrors are 4 against 3, so the oldest goes; of
	// the bases the pinned one stays and the spare goes; the image store
	// holds a live guest and stays; the records are not yet in the table
	// and so are all kept.
	for _, id := range []string{"mirror-old", "base-spare"} {
		k := venue.KindVolume
		if id == "base-spare" {
			k = venue.KindImage
		}
		if v.IsOpen(k, id) {
			t.Errorf("%s not evicted by the pass at Open", id)
		}
	}
	if !v.IsOpen(venue.KindVolume, "attempt-done1") {
		t.Fatal("a record not in the table was evicted")
	}
	m.mu.Lock()
	for _, e := range []*Entry{
		{Attempt: "done1", State: StateExited},
		{Attempt: "live", State: StateRunning},
		{Attempt: "done2", State: StateFailed},
	} {
		m.t.entries[e.Attempt] = e
	}
	m.mu.Unlock()

	dry, err := m.Caches(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Evicted) != 0 || len(dry.Planned) != 1 || !v.IsOpen(venue.KindVolume, "attempt-done1") {
		t.Fatalf("dry pass: planned %v, evicted %v", dry.Planned, dry.Evicted)
	}
	rep, err := m.Caches(true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range rep.Evicted {
		got = append(got, e.Object.ID)
	}
	// Records are 6 against 5: the oldest terminal one goes; the live one is
	// never a candidate.
	want := "attempt-done1"
	if strings.Join(got, ",") != want {
		t.Fatalf("evicted %v, want %s; errors %v", got, want, rep.Errors)
	}
	for _, id := range []string{"mirror-new", "base-pinned", "image-store", "attempt-live", "attempt-done2"} {
		k := venue.KindVolume
		if strings.HasPrefix(id, "base-") || id == "image-store" {
			k = venue.KindImage
		}
		if !v.IsOpen(k, id) {
			t.Errorf("%s was evicted", id)
		}
	}
	if _, ok := m.t.entries["done1"]; ok {
		t.Error("the evicted record's attempt is still in the table")
	}
	if _, err := os.Stat(filepath.Join(v.Root(), "mirrors/old.git")); !os.IsNotExist(err) {
		t.Errorf("evicted mirror still on disk: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(v.Root(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"id":"mirror-old","path":"mirrors/old.git","note":"evicted: budget mirrors, oldest first`) {
		t.Error("the mirror's eviction is not a noted removal in the ledger")
	}
	var overStore bool
	for _, u := range rep.Usage {
		if u.Budget.Name == "image-store" && u.Over > 0 {
			overStore = true
		}
	}
	if !overStore {
		t.Error("the kept image store is not reported over its budget")
	}
	if un, err := v.Unaccounted(); err != nil || len(un) != 0 {
		t.Fatalf("Unaccounted = %v, %v", un, err)
	}
}
