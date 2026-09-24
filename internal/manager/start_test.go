package manager

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/sign"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

type noProcs struct{}

func (noProcs) List() ([]proc.Process, error) { return nil, nil }

// The test binary is ad-hoc signed without the Virtualization entitlement,
// so using it as the helper must be refused before anything is created.
func TestStartRefusesUnentitledHelper(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	before := len(v.OpenObjects())
	if _, err := m.Start("a1", []string{"/bin/true"}, nil); !errors.Is(err, sign.ErrMissing) {
		t.Fatalf("Start with unentitled helper = %v, want sign.ErrMissing", err)
	}
	if got := len(v.OpenObjects()); got != before {
		t.Fatalf("open objects went from %d to %d; the refusal created something", before, got)
	}
	if len(m.List()) != 0 {
		t.Fatalf("table has entries after a refused start: %+v", m.List())
	}
	if un, err := v.Unaccounted(); err != nil || len(un) != 0 {
		t.Fatalf("unaccounted after refusal: %v, %v", un, err)
	}
}
