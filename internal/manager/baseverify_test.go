package manager

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/base"
	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// The manager verifies each class's base once, when it opens (before it
// serves), survives a failure, and records both in its events.
func TestOpenVerifiesEachBaseOnce(t *testing.T) {
	v, _ := venue.Open(t.TempDir())
	v.Init()
	self, _ := os.Executable()
	calls := map[string]int{}
	ci := JobClass{Class: capacity.CI, VerifyBase: func() error { calls["ci"]++; return nil }}
	py := capacity.CI
	py.Name = "py"
	pyc := JobClass{Class: py, VerifyBase: func() error { calls["py"]++; return fmt.Errorf("%w: tampered", base.ErrTampered) }}
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 8, MemoryBytes: 16 * capacity.GiB}, Classes: []JobClass{ci, pyc, {Class: capacity.Class{Name: "none"}}}})
	if err != nil {
		t.Fatalf("a failed base verify stopped the manager: %v", err)
	}
	defer m.Close()
	if calls["ci"] != 1 || calls["py"] != 1 {
		t.Fatalf("verify calls = %v, want one each", calls)
	}
	ev, _ := os.ReadFile(m.dir + "/events.jsonl")
	if !strings.Contains(string(ev), `"event":"base-verified"`) || !strings.Contains(string(ev), `"event":"base-verify-failed"`) {
		t.Fatalf("events = %s", ev)
	}
}

// A class whose base is not verified since this boot is refused at
// admission with base-unverified, and nothing is created.
func TestStartRefusesAnUnverifiedBase(t *testing.T) {
	v, _ := venue.Open(t.TempDir())
	v.Init()
	self, _ := os.Executable()
	hashed := false
	ci := JobClass{Class: capacity.CI,
		Base:       func() (string, error) { return "", fmt.Errorf("%w: /x/rootfs.ext4", base.ErrUnverified) },
		VerifyBase: func() error { hashed = true; return errors.New("tampered") }}
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 8, MemoryBytes: 16 * capacity.GiB},
		Space: func() (venue.Space, error) {
			return venue.Space{Used: 10 * capacity.GiB, Avail: 500 * capacity.GiB, Shared: true}, nil
		}, Classes: []JobClass{ci}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	hashed = false
	_, err = m.Start("a1", []string{"/bin/true"}, nil)
	var ref *capacity.Refusal
	if !errors.As(err, &ref) || ref.Reason != capacity.ReasonBaseUnverified {
		t.Fatalf("Start = %v, want a base-unverified refusal", err)
	}
	if hashed {
		t.Fatal("a start hashed the base")
	}
	if len(m.List()) != 0 {
		t.Fatal("a refused start left an entry")
	}
}
