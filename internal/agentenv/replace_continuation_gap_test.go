package agentenv

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

// POMAR-CC-SOW-37: reproduces, end to end, the real gap found exercising a
// live guest -- an operation that becomes acceptance_unknown across a
// `replace` cannot currently have its predecessor incarnation's custody
// independently reconciled afterward, because reconcileHandler requires
// `req.Incarnation == e.Spec.Incarnation`, and `replace` (stop() then
// start()) advances e.Spec.Incarnation before any caller gets a chance to
// call /reconcile against the one that was just stopped.
//
// TestContinuationPreservesUnknownAndDispatchesOnlyBoundSuccessorOnce
// (continuation_test.go) already proves the BROKER's bindContinuation
// works correctly GIVEN a valid OriginalFence Reconciliation receipt --
// this test proves the HOST side of the real flow can never produce one
// for an incarnation `replace` has already superseded, so that valid
// receipt is unobtainable in practice today. The operation is left
// correctly uncertain (acceptance_unknown, new work refused) -- safe --
// but with no supported path back to resolution, which is the gap.
func TestReplaceForecloseReconciliationOfTheIncarnationItSuperseded(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	helper := "/retained/helper"
	spec := VMSpec{HostConfig: HostConfig{Root: filepath.Dir(dir), Helper: helper}, Environment: "env", Session: "session", Incarnation: "actor2", Directory: dir, Rootfs: filepath.Join(dir, "workspace.ext4")}
	e := &Environment{
		Spec:  spec,
		Phase: "execution_unknown",
		// e.Spec.Incarnation and e.Process both already reflect the
		// SUCCESSOR (actor2): exactly what `replace` leaves behind once
		// `start()` has run (it mutates e.Spec.Incarnation to a fresh
		// randomID()) -- the predecessor's own identity is gone from the
		// live environment struct, only its retained Action/disk records
		// remain.
		Process: proc.Process{PID: 99, UID: os.Getuid(), Start: "Sun Oct 4 06:00:00 2026", Args: helper + " agent-environment --config " + filepath.Join(dir, "vm-config-actor2.json")},
		Actions: map[string]Action{
			"launch-actor1": {Kind: "start", Expected: "created", State: "completed", ResultingIncarnation: "actor1"},
			"replace-mid-flight": {Kind: "replace", Expected: "actor1", State: "completed", ResultingIncarnation: "actor2",
				Fence: &Fence{Incarnation: "actor1", ScopeID: "scope-actor1", NetworkRevoked: true, AdapterAccessRevoked: true, WorkspaceWriteRevoked: true, TerminationConfirmed: true, State: "confirmed"}},
		},
	}
	// vm-config/vm-status for actor1 ARE still retained on disk (replace
	// never deletes them) -- the gap is not missing files, it's the live
	// handler's own incarnation gate.
	actor1Config, _ := json.Marshal(VMSpec{HostConfig: HostConfig{Root: filepath.Dir(dir), Helper: helper}, Environment: "env", Session: "session", Incarnation: "actor1", Directory: dir, Rootfs: spec.Rootfs})
	os.WriteFile(filepath.Join(dir, "vm-config-actor1.json"), actor1Config, 0600)
	os.WriteFile(filepath.Join(dir, "vm-status-actor1.json"), []byte(`{"environment":"env","session":"session","incarnation":"actor1","vm_stopped":"true"}`), 0600)
	os.WriteFile(spec.Rootfs, []byte("retained"), 0600)

	h := &Host{config: HostConfig{Helper: "/new/helper"}, envs: map[string]*Environment{"env": e}}
	h.probe = func(_ context.Context, r FenceProbeRequest) (FenceProbeEvidence, error) {
		fi, _ := os.Stat(spec.Rootfs)
		s := fi.Sys().(*syscall.Stat_t)
		return FenceProbeEvidence{Environment: r.Environment, Session: r.Session, Incarnation: r.Incarnation, Operation: r.Operation,
			ObservedAt: time.Now().UTC().Format(time.RFC3339), Coverage: "direct-vz-private-disk/v1", ObserverUID: os.Getuid(),
			ProcessCount: 1, OwnProcessesChecked: 1, RootfsDevice: uint32(s.Dev), RootfsInode: s.Ino, Confirmed: true, Issues: []string{}}, nil
	}

	req := ReconcileRequest{OperationID: "reconcile-actor1-after-replace", SessionID: "session", Incarnation: "actor1", LaunchOperation: "launch-actor1", FenceOperation: "replace-mid-flight"}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/environments/env/reconcile", strings.NewReader(string(body))))

	// Today's real, current behavior: refused, because actor1 is no longer
	// e.Spec.Incarnation (actor2 is). This is the exact "stale incarnation"
	// response observed live. If this assertion ever starts failing because
	// a fix lets reconcile target a superseded incarnation safely (still
	// requiring real identity/birth verification, per the instruction to
	// keep incarnation checks strict), that is this gap being closed --
	// update this test to assert the NEW supported resolution path instead
	// of deleting the coverage.
	if w.Code == 200 {
		t.Fatalf("reconcile unexpectedly succeeded for a superseded incarnation -- if this is the intended fix, replace this assertion with one proving the resulting Reconciliation is still correctly gated on real probe evidence, not merely permitted: %s", w.Body.String())
	}
	var errBody struct {
		Error string `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &errBody)
	if !strings.Contains(strings.ToLower(errBody.Error), "stale") {
		t.Fatalf("expected a stale-incarnation refusal, got: %s", w.Body.String())
	}

	// Confirming the consequence, not just the host-side symptom:
	// TestContinuationPreservesUnknownAndDispatchesOnlyBoundSuccessorOnce
	// proves the broker's bindContinuation requires exactly this kind of
	// Reconciliation receipt (ReplacementEligible, CurrentScope
	// "fenced_now", a 64-char EvidenceSHA256) for OriginalFence. Since this
	// host can never produce one for actor1 once replace has superseded it,
	// no caller can construct a valid ContinuationBinding for an operation
	// stuck under actor1 today -- confirmed here at the source of the
	// unobtainable evidence, not asserted separately against the broker.
}
