package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func reconciliationFixture(t *testing.T) (*Host, *Environment, ReconcileRequest, []byte) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	spec := VMSpec{HostConfig: HostConfig{Root: filepath.Dir(dir), Helper: "/retained/helper"}, Environment: "env", Session: "session", Incarnation: "actor", Directory: dir, Rootfs: filepath.Join(dir, "workspace.ext4")}
	old := &Fence{Incarnation: "actor", ScopeID: "scope-actor", NetworkRevoked: true, AdapterAccessRevoked: true, State: "execution_unknown"}
	e := &Environment{Spec: spec, Phase: "execution_unknown", Process: proc.Process{PID: 42, UID: os.Getuid(), Start: "Sun Oct 4 05:09:57 2026", Args: spec.Helper + " agent-environment --config " + filepath.Join(dir, "vm-config-actor.json")}, Actions: map[string]Action{"launch": {Kind: "start", Expected: "created", State: "completed"}, "stop": {Kind: "stop", Expected: "actor", State: "acceptance_unknown", Fence: old}}, LastFence: old}
	b, _ := json.Marshal(spec)
	os.WriteFile(filepath.Join(dir, "vm-config-actor.json"), b, 0600)
	original := []byte(`{"environment":"env","session":"session","incarnation":"actor","vm_stopped":"false"}`)
	os.WriteFile(filepath.Join(dir, "vm-status.json"), original, 0600)
	os.WriteFile(spec.Rootfs, []byte("retained"), 0600)
	h := &Host{config: HostConfig{Helper: "/new/helper"}, envs: map[string]*Environment{"env": e}}
	h.probe = func(_ context.Context, r FenceProbeRequest) (FenceProbeEvidence, error) {
		fi, _ := os.Stat(spec.Rootfs)
		s := fi.Sys().(*syscall.Stat_t)
		return FenceProbeEvidence{Environment: r.Environment, Session: r.Session, Incarnation: r.Incarnation, Operation: r.Operation, ObservedAt: time.Now().UTC().Format(time.RFC3339), Coverage: "direct-vz-private-disk/v1", ObserverUID: os.Getuid(), ProcessCount: 10, OwnProcessesChecked: 2, RootfsDevice: uint32(s.Dev), RootfsInode: s.Ino, Confirmed: true, Issues: []string{}}, nil
	}
	return h, e, ReconcileRequest{OperationID: "reconcile", SessionID: "session", Incarnation: "actor", LaunchOperation: "launch", FenceOperation: "stop"}, original
}
func reconcileCall(h *Host, r ReconcileRequest) *httptest.ResponseRecorder {
	b, _ := json.Marshal(r)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/env/reconcile", strings.NewReader(string(b))))
	return w
}
func TestReconcileCurrentFenceKeepsOriginalUnknown(t *testing.T) {
	h, e, r, original := reconciliationFixture(t)
	oldFence := *e.Actions["stop"].Fence
	w := reconcileCall(h, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a := e.Actions[r.OperationID]
	if e.Phase != "fenced" || !a.Reconciliation.ReplacementEligible || a.Reconciliation.HistoricalExecution != "unknown" || !a.Fence.WorkspaceWriteRevoked {
		t.Fatalf("%+v", a)
	}
	if *e.Actions["stop"].Fence != oldFence || e.Actions["stop"].State != "acceptance_unknown" {
		t.Fatal("historical fence rewritten")
	}
	b, _ := os.ReadFile(filepath.Join(e.Spec.Directory, "vm-status.json"))
	if string(b) != string(original) {
		t.Fatal("historical receipt rewritten")
	}
	b, _ = os.ReadFile(filepath.Join(e.Spec.Directory, a.Reconciliation.EvidenceFile))
	if digest(b) != a.Reconciliation.EvidenceSHA256 {
		t.Fatal("unbound evidence")
	}
	h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
		t.Fatal("retry repeated probe")
		return FenceProbeEvidence{}, nil
	}
	if w = reconcileCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	r.FenceOperation = "different"
	if w = reconcileCall(h, r); w.Code != 409 {
		t.Fatal("changed operation identity accepted")
	}
}
func TestReconcileIncompleteCoverageAndWriteFailureHold(t *testing.T) {
	for _, mode := range []string{"probe error", "unverified writer", "identity mismatch", "evidence write failure"} {
		t.Run(mode, func(t *testing.T) {
			h, e, r, _ := reconciliationFixture(t)
			originalProbe := h.probe
			h.probe = func(ctx context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
				v, err := originalProbe(ctx, p)
				switch mode {
				case "probe error":
					return v, errors.New("pid 99: executable inaccessible")
				case "unverified writer":
					v.Issues = []string{"pid 99: workspace fd inaccessible"}
				case "identity mismatch":
					v.Session = "wrong"
				case "evidence write failure":
					os.Mkdir(filepath.Join(e.Spec.Directory, "reconcile-evidence-reconcile.json"), 0700)
				}
				return v, err
			}
			w := reconcileCall(h, r)
			if e.Phase == "fenced" || e.Actions[r.OperationID].Reconciliation.ReplacementEligible {
				t.Fatal("unknown writer/durability admitted replacement")
			}
			if mode == "evidence write failure" && w.Code == 200 {
				t.Fatal("write failure success")
			}
			if err := h.start(e); err == nil {
				t.Fatal("launch admitted through hold")
			}
		})
	}
}
func TestReconcileBlocksConcurrentLaunchThroughEvidenceCommit(t *testing.T) {
	h, e, r, _ := reconciliationFixture(t)
	originalProbe := h.probe
	entered := make(chan struct{})
	release := make(chan struct{})
	h.probe = func(ctx context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
		close(entered)
		<-release
		return originalProbe(ctx, p)
	}
	finished := make(chan struct{})
	go func() { reconcileCall(h, r); close(finished) }()
	<-entered
	acquired := make(chan struct{})
	go func() { e.operationMu.Lock(); close(acquired); e.operationMu.Unlock() }()
	select {
	case <-acquired:
		t.Fatal("concurrent lifecycle acquired operation lock during evidence")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-finished
	<-acquired
	b, _ := os.ReadFile(filepath.Join(e.Spec.Directory, "environment.json"))
	var stored Environment
	if json.Unmarshal(b, &stored) != nil || stored.Phase != "fenced" {
		t.Fatal("replacement lock released before durable fence")
	}
}
