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

// A head-and-base start is refused before anything unless it is a repository
// source naming two full SHAs; a source with neither a ref nor that pair is
// refused too.
func TestCheckExactSource(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, ok := range []*Source{
		nil,
		{Mirror: "m", Ref: "main"},
		{Mirror: "m", Ref: "main", SHA: a},
		{Mirror: "m", SHA: a, BaseSHA: b, Git: true},
		{Mirror: "m", Ref: "feature", SHA: a, BaseSHA: b, Git: true},
	} {
		if err := checkExactSource(ok); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
	for name, bad := range map[string]*Source{
		"no ref and no pair":  {Mirror: "m"},
		"a base without git":  {Mirror: "m", SHA: a, BaseSHA: b},
		"a base without head": {Mirror: "m", Ref: "main", BaseSHA: b, Git: true},
		"a short base":        {Mirror: "m", SHA: a, BaseSHA: "bbbb", Git: true},
		"a ref as head":       {Mirror: "m", SHA: "main", BaseSHA: b, Git: true},
	} {
		if err := checkExactSource(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
