package manager

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func twoClasses(t *testing.T) (*Manager, JobClass, JobClass) {
	t.Helper()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	ci := JobClass{Class: capacity.CI, Guest: Guest{ImageDigest: "sha256:go", ImageArm64: "arm-go", PackageSet: "ps-go"}}
	py := capacity.CI
	py.Name, py.VCPU, py.MemoryBytes, py.Concurrency, py.Measured = "py", 1, capacity.GiB, 1, false
	pyc := JobClass{Class: py, Guest: Guest{ImageDigest: "sha256:py", ImageArm64: "arm-py", PackageSet: "ps-py"}}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 8, MemoryBytes: 16 * capacity.GiB}, Classes: []JobClass{ci, pyc}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, ci, pyc
}

func TestJobClassesNeedDistinctNames(t *testing.T) {
	v, _ := venue.Open(t.TempDir())
	v.Init()
	self, _ := os.Executable()
	for _, classes := range [][]JobClass{
		{{Class: capacity.CI}, {Class: capacity.CI}},
		{{Class: capacity.Class{}}},
	} {
		// Duplicate names, and an empty name (never defaulted), are refused.
		cfg := Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
			Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, Classes: classes}
		if m, err := Open(cfg); err == nil {
			m.Close()
			t.Fatalf("classes %v accepted", classes)
		}
	}
}

func TestStartChoosesTheNamedClass(t *testing.T) {
	m, ci, py := twoClasses(t)
	if jc, _ := m.jobClass(""); jc.Class.Name != ci.Class.Name {
		t.Fatalf("default class %q, want the first (%q)", jc.Class.Name, ci.Class.Name)
	}
	if jc, _ := m.jobClass("py"); jc.Guest.ImageDigest != "sha256:py" {
		t.Fatalf("class py boots %q", jc.Guest.ImageDigest)
	}
	// An unknown class is refused before anything is created.
	if _, err := m.StartIn("nope", "a1", []string{"true"}, nil); err == nil || !strings.Contains(err.Error(), "no job class") {
		t.Fatalf("unknown class: %v", err)
	}
	if len(m.List()) != 0 {
		t.Fatal("an unknown class created an entry")
	}
	// Each class is admitted against its own limit: py allows one at a time.
	m.mu.Lock()
	m.t.entries["busy"] = &Entry{Attempt: "busy", State: StateRunning, Class: py.Class, Created: time.Now()}
	m.mu.Unlock()
	_, err := m.StartIn("py", "a2", []string{"true"}, nil)
	var ref *capacity.Refusal
	if !errors.As(err, &ref) || ref.Reason != "class-concurrency" {
		t.Fatalf("a second py attempt: %v, want a class-concurrency refusal", err)
	}
	// The default class is not held up by py's limit: it passes admission and
	// stops only at the unentitled test binary.
	_, err = m.StartIn("", "a3", []string{"true"}, nil)
	if err == nil || errors.As(err, &ref) {
		t.Fatalf("a ci attempt: %v, want past admission", err)
	}
	// The pins are the class's own.
	p, err := m.pinsFor([]string{"true"}, py.Guest)
	if err != nil || p.Image != "sha256:py" || p.PackageSet != "ps-py" {
		t.Fatalf("py pins %+v (%v)", p, err)
	}
	c, err := m.Capacity()
	if err != nil || len(c.Classes) != 2 || c.Class.Name != "ci" {
		t.Fatalf("capacity %+v (%v)", c, err)
	}
}
