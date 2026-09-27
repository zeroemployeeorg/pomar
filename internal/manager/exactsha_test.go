package manager

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/mirror"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// A start that names an exact commit must name a full SHA; anything else is
// refused before anything is synced, resolved or created.
func TestStartRefusesAMalformedExactSHA(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, Mirrors: &mirror.Mirrors{Venue: v}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, sha := range []string{"abc123", strings.Repeat("A", 40), "main", strings.Repeat("0", 39)} {
		_, err := m.Start("a1", []string{"true"}, &Source{Mirror: "demo", Ref: "main", SHA: sha})
		if err == nil || !strings.Contains(err.Error(), "not a full commit SHA") {
			t.Errorf("sha %q: %v", sha, err)
		}
	}
	if len(m.List()) != 0 {
		t.Fatal("a refused start left an entry")
	}
}
