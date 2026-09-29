package manager

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
)

// A drain refuses every new start by name while live attempts run on, and
// lifting it lets starts through again (the elders' ruling of 2026-09-27
// 17:21Z §2 item 2).
func TestDrainRefusesNewStartsAndLeavesLiveOnes(t *testing.T) {
	m, ctl, v := openSigning(t, false)
	owner := NewClient(v.Root())
	m.mu.Lock()
	m.t.entries["live-1"] = &Entry{Attempt: "live-1", State: StateRunning, Class: capacity.CI, Created: time.Now()}
	m.mu.Unlock()

	var c Capacity
	if err := owner.Do("POST", "/v1/drain", map[string]bool{"drain": true}, &c); err != nil || !c.Draining || c.Live != 1 {
		t.Fatalf("drain: %+v, %v", c, err)
	}
	_, err := m.StartIn("", "new-1", []string{"true"}, nil)
	var ref *capacity.Refusal
	if !errors.As(err, &ref) || ref.Reason != capacity.ReasonDraining {
		t.Fatalf("a start while draining: %v, want a draining refusal", err)
	}
	// Through the control socket: 503 with the reason named.
	err = ctl.Do("POST", "/v1/attempts", StartRequest{ID: "new-2", Command: []string{"true"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("a start through the control socket while draining: %v", err)
	}
	if _, ok := m.Get("new-1"); ok {
		t.Fatal("a refused start created an entry")
	}
	if e, _ := m.Get("live-1"); e.State != StateRunning {
		t.Fatalf("the live attempt was touched: %s", e.State)
	}
	// The control socket reads the drain, and cannot set or lift it.
	if err := ctl.Do("GET", "/v1/capacity", nil, &c); err != nil || !c.Draining {
		t.Fatalf("capacity through the control socket: %+v, %v", c, err)
	}
	if err := ctl.Do("POST", "/v1/drain", map[string]bool{"drain": false}, nil); err == nil {
		t.Fatal("the control socket lifted the drain")
	}
	// Lifted: a start passes admission again (and stops only at the
	// unentitled test binary).
	if err := owner.Do("POST", "/v1/drain", map[string]bool{"drain": false}, &c); err != nil || c.Draining {
		t.Fatalf("lift: %+v, %v", c, err)
	}
	if _, err := m.StartIn("", "new-3", []string{"true"}, nil); err == nil || errors.As(err, &ref) {
		t.Fatalf("a start after the drain is lifted: %v, want past admission", err)
	}
}

func TestDrainNeedsAnExplicitValue(t *testing.T) {
	_, _, v := openSigning(t, false)
	owner := NewClient(v.Root())
	if err := owner.Do("POST", "/v1/drain", map[string]string{}, nil); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("an empty drain request: %v, want 400", err)
	}
}
