package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func prebootFixture(t *testing.T) (*Host, *Environment, ReconcileRequest) {
	t.Helper()
	h, e, r, _ := reconciliationFixture(t)
	if err := os.Remove(e.Spec.Rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.Spec.Directory, "vm-status.json")); err != nil {
		t.Fatal(err)
	}
	e.Spec.Store = filepath.Join(e.Spec.Directory, "store")
	b, _ := json.Marshal(e.Spec)
	if err := os.WriteFile(filepath.Join(e.Spec.Directory, "vm-config-actor.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	e.HelperExit = &HelperExit{Incarnation: "actor", PID: 42, ObservedAt: "2026-10-04T05:10:00+01:00", ExitCode: 2}
	h.probe = func(_ context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
		if !p.WorkspaceAbsent || p.Container != filepath.Join(e.Spec.Store, "containers", "agent-env-actor") {
			t.Fatal("probe omitted the preboot disk/container binding")
		}
		return FenceProbeEvidence{Environment: p.Environment, Session: p.Session, Incarnation: p.Incarnation, Operation: p.Operation, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Coverage: "direct-vz-preboot-refusal/v1", ObserverUID: os.Getuid(), ProcessCount: 10, OwnProcessesChecked: 2, WorkspaceAbsent: true, ContainerAbsent: true, Confirmed: true}, nil
	}
	return h, e, r
}

func TestReconcileObservedPrebootRefusalKeepsOriginalAndReleasesOnce(t *testing.T) {
	h, e, r := prebootFixture(t)
	exitBytes, _ := json.Marshal(e.HelperExit)
	originalFence := *e.Actions["stop"].Fence
	calls := 0
	probe := h.probe
	h.probe = func(ctx context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
		calls++
		return probe(ctx, p)
	}
	if w := reconcileCall(h, r); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a := e.Actions[r.OperationID]
	if e.Phase != "fenced" || !a.Fence.WorkspaceWriteRevoked || !a.Fence.TerminationConfirmed || !a.Reconciliation.ReplacementEligible || a.Reconciliation.HistoricalExecution != "preboot_refused" || a.Reconciliation.OriginalStatusSHA256 != "" || a.Reconciliation.OriginalHelperExitSHA256 != digest(exitBytes) {
		t.Fatalf("%+v", a)
	}
	if *e.Actions["stop"].Fence != originalFence || e.Actions["stop"].State != "acceptance_unknown" {
		t.Fatal("rewrote original failed stop")
	}
	after, _ := json.Marshal(e.HelperExit)
	if string(after) != string(exitBytes) {
		t.Fatal("rewrote original helper exit")
	}
	if _, err := os.Lstat(e.Spec.Rootfs); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created or replaced workspace")
	}
	if w := reconcileCall(h, r); w.Code != 200 || calls != 1 || e.Phase != "fenced" {
		t.Fatal("retry repeated reconciliation or changed released reservation")
	}
	r.LaunchOperation = "different"
	if w := reconcileCall(h, r); w.Code != 409 {
		t.Fatal("reused operation id with different launch")
	}
}

func TestPrebootRefusalNeverInferredFromMissingFilesOrPID(t *testing.T) {
	for _, mode := range []string{"no exit", "other exit", "signal", "wrong pid", "wrong incarnation", "before birth", "future exit", "unknown birth", "workspace", "symlink", "container", "corrupt status", "foreign legacy status", "config store"} {
		t.Run(mode, func(t *testing.T) {
			h, e, r := prebootFixture(t)
			switch mode {
			case "no exit":
				e.HelperExit = nil
			case "other exit":
				e.HelperExit.ExitCode = 1
			case "signal":
				e.HelperExit.Signal = 9
			case "wrong pid":
				e.HelperExit.PID = 43
			case "wrong incarnation":
				e.HelperExit.Incarnation = "old"
			case "before birth":
				e.HelperExit.ObservedAt = "2026-10-03T01:00:00Z"
			case "future exit":
				e.HelperExit.ObservedAt = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			case "unknown birth":
				e.Process.Start = ""
			case "workspace":
				os.WriteFile(e.Spec.Rootfs, []byte("retained"), 0600)
			case "symlink":
				os.Symlink(filepath.Join(e.Spec.Directory, "missing"), e.Spec.Rootfs)
			case "container":
				os.MkdirAll(filepath.Join(e.Spec.Store, "containers", "agent-env-actor"), 0700)
			case "corrupt status":
				os.WriteFile(filepath.Join(e.Spec.Directory, "vm-status-actor.json"), []byte("{"), 0600)
			case "foreign legacy status":
				os.WriteFile(filepath.Join(e.Spec.Directory, "vm-status.json"), []byte(`{"environment":"env","session":"session","incarnation":"other","vm_stopped":"true"}`), 0600)
			case "config store":
				e.Spec.Store = filepath.Join(e.Spec.Directory, "substituted")
			}
			h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
				t.Fatal("uncertain launch reached nonstart probe")
				return FenceProbeEvidence{}, nil
			}
			if w := reconcileCall(h, r); w.Code == 200 || e.Phase == "fenced" {
				t.Fatal("uncertain launch released")
			}
			if err := h.start(e); err == nil {
				t.Fatal("uncertain launch became eligible")
			}
		})
	}
}

func TestPrebootMachineUncertaintyKeepsHold(t *testing.T) {
	for _, mode := range []string{"probe denied", "possible VM", "wrong coverage", "missing workspace coverage", "missing container coverage", "workspace appears", "container appears", "status appears", "evidence write failure"} {
		t.Run(mode, func(t *testing.T) {
			h, e, r := prebootFixture(t)
			original := h.probe
			h.probe = func(ctx context.Context, p FenceProbeRequest) (FenceProbeEvidence, error) {
				v, err := original(ctx, p)
				switch mode {
				case "probe denied":
					return v, errors.New("owned descriptor access denied")
				case "possible VM":
					v.Issues = []string{"live Virtualization executor ownership unresolved"}
				case "wrong coverage":
					v.Coverage = "direct-vz-private-disk/v1"
				case "missing workspace coverage":
					v.WorkspaceAbsent = false
				case "missing container coverage":
					v.ContainerAbsent = false
				case "workspace appears":
					os.WriteFile(e.Spec.Rootfs, []byte("writer"), 0600)
				case "container appears":
					os.MkdirAll(p.Container, 0700)
				case "status appears":
					os.WriteFile(filepath.Join(e.Spec.Directory, "vm-status-actor.json"), []byte("{}"), 0600)
				case "evidence write failure":
					os.Mkdir(filepath.Join(e.Spec.Directory, "reconcile-evidence-reconcile.json"), 0700)
				}
				return v, err
			}
			reconcileCall(h, r)
			a := e.Actions[r.OperationID]
			if e.Phase == "fenced" || a.Reconciliation.ReplacementEligible || (a.Fence != nil && a.Fence.TerminationConfirmed) {
				t.Fatal("incomplete native/write observation cleared hold")
			}
			if err := h.start(e); err == nil {
				t.Fatal("uncertain current scope admitted launch")
			}
		})
	}
}
