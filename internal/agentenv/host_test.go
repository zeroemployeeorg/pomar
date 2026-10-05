package agentenv

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

type processFixture struct {
	processes []proc.Process
	err       error
}

func (p processFixture) List() ([]proc.Process, error) { return p.processes, p.err }

func TestStopUncertaintyDoesNotPermitReplacement(t *testing.T) {
	for _, test := range []struct {
		name               string
		observer           processFixture
		receiptIncarnation string
		confirmed          bool
	}{
		{"observer error", processFixture{err: errors.New("unknown inventory")}, "actor", false},
		{"old still alive", processFixture{processes: []proc.Process{{PID: 42, Start: "birth"}}}, "actor", false},
		{"stale receipt", processFixture{}, "predecessor", false},
		{"matching stop and departure", processFixture{}, "actor", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			e := &Environment{Spec: VMSpec{Environment: "env", Incarnation: "actor", Directory: dir}, Phase: "running", Process: proc.Process{PID: 42, Start: "birth"}, Actions: map[string]Action{}}
			data, _ := json.Marshal(map[string]string{"incarnation": test.receiptIncarnation, "vm_stopped": "true"})
			os.WriteFile(filepath.Join(dir, "vm-status.json"), data, 0o600)
			g, _ := NewEgress([]string{"allowed.example"})
			h := &Host{envs: map[string]*Environment{"env": e}, gates: map[string]*Egress{"env": g}, servers: map[string]*http.Server{}, processes: test.observer, stopWait: time.Millisecond}
			h.mu.Lock()
			err := h.stop(e)
			h.mu.Unlock()
			if (err == nil) != test.confirmed {
				t.Fatalf("stop: %+v %v", e, err)
			}
			if e.LastFence == nil || !e.LastFence.NetworkRevoked || !e.LastFence.AdapterAccessRevoked {
				t.Fatal("partial fence was not retained")
			}
			if e.LastFence.WorkspaceWriteRevoked != test.confirmed || e.LastFence.TerminationConfirmed != test.confirmed {
				t.Fatal("unknown execution claimed revoked")
			}
			if !test.confirmed {
				if err = h.start(e); err == nil {
					t.Fatal("replacement admitted before execution fence")
				}
			}
		})
	}
}
