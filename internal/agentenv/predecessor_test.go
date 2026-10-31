package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPredecessorCapturePersistsBeforeAdvanceAndSurvivesReload(t *testing.T) {
	h, e, _, original := reconciliationFixture(t)
	e.Phase = "stopped"
	id, err := h.ensurePredecessor(e)
	if err != nil {
		t.Fatal(err)
	}
	if e.Spec.Incarnation != "actor" {
		t.Fatal("capture advanced incarnation")
	}
	b, err := os.ReadFile(filepath.Join(e.Spec.Directory, "environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained Environment
	if err := json.Unmarshal(b, &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Actions[id].Reconciliation == nil || retained.Actions[id].Reconciliation.HistoricalExecution != "unknown" {
		t.Fatal("lost evidence or fabricated historical completion")
	}
	h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
		t.Fatal("repeated probe after reload")
		return FenceProbeEvidence{}, nil
	}
	if again, err := h.ensurePredecessor(&retained); err != nil || again != id {
		t.Fatalf("reload: %s %v", again, err)
	}
	got, _ := os.ReadFile(filepath.Join(e.Spec.Directory, "vm-status.json"))
	if string(got) != string(original) {
		t.Fatal("rewrote historical receipt")
	}
	// Advancing the actor must not make old public reconciliation valid.
	e.Spec.Incarnation = "successor"
	req := e.Actions[id].Reconciliation.Request
	req.OperationID = "new-stale-probe"
	if w := reconcileCall(h, req); w.Code != 403 {
		t.Fatalf("stale request: %d %s", w.Code, w.Body.String())
	}
}

func TestPredecessorCaptureHoldsOnIncompleteProbeOrPersistenceFailure(t *testing.T) {
	for _, mode := range []string{"probe", "write", "lost-evidence"} {
		t.Run(mode, func(t *testing.T) {
			h, e, _, _ := reconciliationFixture(t)
			e.Phase = "stopped"
			if mode == "probe" {
				h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
					return FenceProbeEvidence{}, errors.New("incomplete")
				}
			}
			if mode == "write" {
				id := "predecessor-" + digest([]byte(e.Spec.Session+"\x00"+e.Spec.Incarnation))
				if err := os.Mkdir(filepath.Join(e.Spec.Directory, "reconcile-evidence-"+id+".json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "lost-evidence" {
				id, err := h.ensurePredecessor(e)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(e.Spec.Directory, e.Actions[id].Reconciliation.EvidenceFile)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.ensurePredecessor(e); err == nil {
				t.Fatal("incomplete evidence authorized launch")
			}
			if e.Spec.Incarnation != "actor" {
				t.Fatal("advanced predecessor")
			}
			if mode == "write" && h.fatal == nil {
				t.Fatal("persistence failure not latched")
			}
		})
	}
}

func TestHostJournalFailureRefusesCreationAndLifecycleButAllowsInspection(t *testing.T) {
	h, e, _, _ := reconciliationFixture(t)
	h.config.Root = t.TempDir()
	h.fatal = errors.New("fsync failed")
	for _, path := range []string{"/v1/environments", "/v1/environments/env/stop", "/v1/environments/env/start", "/v1/environments/env/replace"} {
		body := `{"id":"new","operation_id":"new-op","expected_incarnation":"actor"}`
		if path == "/v1/environments" {
			body = `{"id":"new","operation_id":"new-op"}`
		} else {
			body = `{"operation_id":"new-op","expected_incarnation":"actor"}`
		}
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if w.Code < 400 {
			t.Fatalf("mutation accepted: %s %d", path, w.Code)
		}
	}
	if _, ok := e.Actions["new-op"]; ok {
		t.Fatal("recorded a mutation after fatal")
	}
	if _, err := os.Stat(filepath.Join(h.config.Root, "new")); !os.IsNotExist(err) {
		t.Fatal("allocated after fatal")
	}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/environments/env", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestStartCapturesPredecessorThroughProductionPathBeforeLaunch(t *testing.T) {
	h, e, _, _ := reconciliationFixture(t)
	h.config = e.Spec.HostConfig
	h.config.MaxLive = 2
	e.Phase = "stopped"
	// Refuse before opening a listener or invoking a VM, after the production
	// launch journal point. This keeps the test independent of host sockets.
	e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
	if err := os.WriteFile(e.Spec.EgressSocket, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.start(e); err == nil {
		t.Fatal("expected occupied egress refusal")
	}
	if e.Spec.Incarnation == "actor" {
		t.Fatal("did not reach production incarnation advance")
	}
	b, err := os.ReadFile(filepath.Join(e.Spec.Directory, "environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained Environment
	if err := json.Unmarshal(b, &retained); err != nil {
		t.Fatal(err)
	}
	id := predecessorOperation(&retained, "actor")
	if id == "" || retained.Spec.Incarnation == "actor" {
		t.Fatal("launch journal missing predecessor evidence")
	}
	if retained.Phase != "launching" {
		t.Fatal(retained.Phase)
	}
}

func TestStopRefusesReceiptForAnotherEnvironmentOrSession(t *testing.T) {
	for _, field := range []string{"environment", "session"} {
		t.Run(field, func(t *testing.T) {
			h, e, _, _ := reconciliationFixture(t)
			h.stopWait = 1
			h.processes = processFixture{}
			status := map[string]string{"environment": e.Spec.Environment, "session": e.Spec.Session, "incarnation": e.Spec.Incarnation, "vm_stopped": "true"}
			status[field] = "another"
			b, _ := json.Marshal(status)
			if err := os.WriteFile(filepath.Join(e.Spec.Directory, "vm-status.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			err := h.stop(e)
			h.mu.Unlock()
			if err == nil || e.Phase != "execution_unknown" {
				t.Fatal("foreign receipt released execution")
			}
		})
	}
}

func TestPublicReplaceCapturesEvidenceAndRetryDoesNotRepeatProbe(t *testing.T) {
	h, e, _, _ := reconciliationFixture(t)
	h.config = e.Spec.HostConfig
	h.config.MaxLive = 2
	e.Phase = "stopped"
	e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
	if err := os.WriteFile(e.Spec.EgressSocket, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	probe := h.probe
	calls := 0
	h.probe = func(ctx context.Context, r FenceProbeRequest) (FenceProbeEvidence, error) {
		calls++
		return probe(ctx, r)
	}
	w := hostActionCall(h, "env", "replace", "replace-op", "actor")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	} // deterministic pre-helper refusal
	if predecessorOperation(e, "actor") == "" || calls != 1 {
		t.Fatal("replacement lost predecessor")
	}
	inc := e.Spec.Incarnation
	w = hostActionCall(h, "env", "replace", "replace-op", "actor")
	if w.Code != 200 || calls != 1 || e.Spec.Incarnation != inc || e.Actions["replace-op"].State != "acceptance_unknown" {
		t.Fatal("retry repeated transition or hid uncertainty", w.Body.String())
	}
}

func TestStartIncompletePredecessorDoesNotAdvanceIncarnation(t *testing.T) {
	h, e, _, _ := reconciliationFixture(t)
	h.config = e.Spec.HostConfig
	h.config.MaxLive = 2
	e.Phase = "stopped"
	h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
		return FenceProbeEvidence{}, errors.New("coverage unavailable")
	}
	if err := h.start(e); err == nil {
		t.Fatal("launched without predecessor evidence")
	}
	if e.Spec.Incarnation != "actor" || e.Phase != "execution_unknown" {
		t.Fatal("advanced while predecessor unknown")
	}
}

func TestStartAfterPrebootReconciliationAllowsFirstDiskCreation(t *testing.T) {
	h, e, request := prebootFixture(t)
	h.config = e.Spec.HostConfig
	h.config.MaxLive = 2
	if w := reconcileCall(h, request); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// Stop at the production launch journal, before any native helper runs.
	e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
	if err := os.WriteFile(e.Spec.EgressSocket, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.start(e); err == nil {
		t.Fatal("expected occupied egress refusal")
	}
	if e.Phase != "launching" || e.Spec.RequireRetainedDisk {
		t.Fatal("first disk creation was forbidden", e.Phase, e.Spec.RequireRetainedDisk)
	}
	if _, err := os.Lstat(e.Spec.Rootfs); !os.IsNotExist(err) {
		t.Fatal("unexpected disk mutation", err)
	}
}

func TestStartPreservesDiskRequirementAfterPrebootRefusal(t *testing.T) {
	h, e, request := prebootFixture(t)
	e.Spec.RequireRetainedDisk = true // A prior launch already required this disk.
	h.config = e.Spec.HostConfig
	h.config.MaxLive = 2
	if w := reconcileCall(h, request); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
	if err := os.WriteFile(e.Spec.EgressSocket, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.start(e); err == nil {
		t.Fatal("expected occupied egress refusal")
	}
	if e.Phase != "launching" || !e.Spec.RequireRetainedDisk {
		t.Fatal("lost the existing disk retention requirement")
	}
}

func TestStartRequiresPreviouslyObservedDiskEvenWhenRemoved(t *testing.T) {
	h, e, _, _ := reconciliationFixture(t)
	h.config = e.Spec.HostConfig
	h.config.MaxLive = 2
	e.Phase = "stopped"
	if _, err := h.ensurePredecessor(e); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.Spec.Rootfs); err != nil {
		t.Fatal(err)
	}
	e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
	if err := os.WriteFile(e.Spec.EgressSocket, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.start(e); err == nil {
		t.Fatal("expected occupied egress refusal")
	}
	if e.Phase != "launching" || !e.Spec.RequireRetainedDisk {
		t.Fatal("missing used disk allowed fresh cloning")
	}
}

// A vm_stopped receipt plus helper departure does not resolve the independent
// native access uncertainty from a later reconciliation. Exercise both public
// replacement and start, persisted reload, replay and explicit recovery.
func TestLaterUnknownReconciliationSupersedesPositivePredecessor(t *testing.T) {
	for _, automaticA := range []bool{false, true} {
		for _, reload := range []bool{false, true} {
			for _, transition := range []string{"start", "replace"} {
				t.Run(fmt.Sprintf("automaticA=%t/reload=%t/%s", automaticA, reload, transition), func(t *testing.T) {
					h, e, req, _ := reconciliationFixture(t)
					h.config = e.Spec.HostConfig
					h.config.MaxLive = 2
					h.processes = processFixture{}
					h.stopWait = time.Second
					positiveProbe := h.probe
					calls := 0
					h.probe = func(ctx context.Context, r FenceProbeRequest) (FenceProbeEvidence, error) {
						calls++
						return positiveProbe(ctx, r)
					}
					var positiveID string
					if automaticA {
						e.Phase = "stopped"
						var err error
						positiveID, err = h.ensurePredecessor(e)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						req.OperationID = "aa-positive"
						if w := reconcileCall(h, req); w.Code != 200 {
							t.Fatal(w.Body.String())
						}
						positiveID = req.OperationID
					}
					positive := e.Actions[positiveID]
					if predecessorOperation(e, "actor") != positiveID {
						t.Fatal("positive A not eligible")
					}
					h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
						calls++
						return FenceProbeEvidence{}, errors.New("native access coverage unavailable")
					}
					req.OperationID = "zz-negative"
					if w := reconcileCall(h, req); w.Code != 200 {
						t.Fatal(w.Body.String())
					}
					if e.Phase != "execution_unknown" {
						t.Fatal(e.Phase)
					}
					status := []byte(`{"environment":"env","session":"session","incarnation":"actor","vm_stopped":"true"}`)
					if err := os.WriteFile(filepath.Join(e.Spec.Directory, "vm-status.json"), status, 0600); err != nil {
						t.Fatal(err)
					}
					h.mu.Lock()
					err := h.stop(e)
					h.mu.Unlock()
					if err != nil || e.Phase != "stopped" {
						t.Fatal("ordinary stop", err, e.Phase)
					}
					load := func() {
						b, err := os.ReadFile(filepath.Join(e.Spec.Directory, "environment.json"))
						if err != nil {
							t.Fatal(err)
						}
						var retained Environment
						if err := json.Unmarshal(b, &retained); err != nil {
							t.Fatal(err)
						}
						e = &retained
						h.envs["env"] = e
					}
					if reload {
						load()
					}
					if predecessorOperation(e, "actor") != "" {
						t.Fatal("old positive A revived after B and ordinary stop")
					}
					// Retrying A returns its original observation without promoting it.
					if w := reconcileCall(h, positive.Reconciliation.Request); w.Code != 200 {
						t.Fatal(w.Body.String())
					}
					if predecessorOperation(e, "actor") != "" {
						t.Fatal("old positive replay restored eligibility")
					}
					callsBefore := calls
					// Catch advancement before any socket or real helper can run.
					e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
					if err := os.WriteFile(e.Spec.EgressSocket, []byte("occupied"), 0600); err != nil {
						t.Fatal(err)
					}
					if transition == "start" {
						if err := h.start(e); err == nil {
							t.Fatal("unexpected start")
						}
					} else {
						if w := hostActionCall(h, "env", "replace", "blocked-replacement", "actor"); w.Code != 409 {
							t.Fatal(w.Code, w.Body.String())
						}
					}
					if e.Spec.Incarnation != "actor" || calls != callsBefore {
						t.Fatal("advanced or replayed automatic probe over unresolved B")
					}
					// A distinct positive C may restore current eligibility even when
					// its ID sorts before A and B. Reload must retain that authority.
					h.probe = positiveProbe
					req.OperationID = "00-recovery"
					if w := reconcileCall(h, req); w.Code != 200 {
						t.Fatal(w.Body.String())
					}
					if reload {
						load()
						e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
					}
					if predecessorOperation(e, "actor") != "00-recovery" {
						t.Fatal("fresh positive C unavailable")
					}
					h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
						t.Fatal("repeated C probe")
						return FenceProbeEvidence{}, nil
					}
					if err := h.start(e); err == nil || e.Spec.Incarnation == "actor" || e.Phase != "launching" {
						t.Fatal("fresh C did not permit journaled advance", err, e.Phase)
					}
					if e.Actions[positiveID].Reconciliation.CurrentScope != positive.Reconciliation.CurrentScope {
						t.Fatal("historical A rewritten")
					}
				})
			}
		}
	}
}

func TestUnmarkedLegacyReconciliationRequiresFreshExplicitObservation(t *testing.T) {
	h, e, req, _ := reconciliationFixture(t)
	if w := reconcileCall(h, req); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	e.CurrentReconciliation = "" // A journal written before authoritative tracking.
	if _, err := h.ensurePredecessor(e); err == nil {
		t.Fatal("legacy positive inferred as current")
	}
	req.OperationID = "fresh-legacy-recovery"
	if w := reconcileCall(h, req); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if id, err := h.ensurePredecessor(e); err != nil || id != req.OperationID {
		t.Fatal(id, err)
	}
}

func TestCrashDispatchingReconciliationRemainsIneligibleAfterStopAndReload(t *testing.T) {
	for _, transition := range []string{"start", "replace"} {
		t.Run(transition, func(t *testing.T) {
			h, e, _, _ := reconciliationFixture(t)
			h.config = e.Spec.HostConfig
			h.config.MaxLive = 2
			e.Phase = "stopped"
			positiveProbe := h.probe
			idA, err := h.ensurePredecessor(e)
			if err != nil {
				t.Fatal(err)
			}
			aRequest := e.Actions[idA].Reconciliation.Request
			bRequest := aRequest
			bRequest.OperationID = "00-crashed-observation"
			var crashBytes []byte
			h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
				var err error
				crashBytes, err = os.ReadFile(filepath.Join(e.Spec.Directory, "environment.json"))
				if err != nil {
					t.Fatal(err)
				}
				var saved Environment
				if err := json.Unmarshal(crashBytes, &saved); err != nil {
					t.Fatal(err)
				}
				if saved.CurrentReconciliation != bRequest.OperationID || saved.Phase != "reconciling" || saved.Actions[bRequest.OperationID].State != "dispatching" {
					t.Fatal("current marker not durable before native probe")
				}
				return FenceProbeEvidence{}, errors.New("probe interrupted")
			}
			if w := reconcileCall(h, bRequest); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if len(crashBytes) == 0 {
				t.Fatal("did not observe dispatch journal")
			}
			// Recover exactly the disk snapshot from before B completed; no
			// completed negative result or in-memory state is carried over.
			if err := os.WriteFile(filepath.Join(e.Spec.Directory, "environment.json"), crashBytes, 0600); err != nil {
				t.Fatal(err)
			}
			var recovered Environment
			if err := json.Unmarshal(crashBytes, &recovered); err != nil {
				t.Fatal(err)
			}
			e = &recovered
			h = &Host{config: h.config, envs: map[string]*Environment{"env": e}, processes: processFixture{}, stopWait: time.Second}
			h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
				t.Fatal("cached fallback triggered another probe")
				return FenceProbeEvidence{}, nil
			}
			if _, err := h.ensurePredecessor(e); err == nil {
				t.Fatal("dispatching B reused A or deterministic automatic operation")
			}
			if w := reconcileCall(h, aRequest); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if e.CurrentReconciliation != bRequest.OperationID {
				t.Fatal("historical replay promoted A")
			}
			status := []byte(`{"environment":"env","session":"session","incarnation":"actor","vm_stopped":"true"}`)
			if err := os.WriteFile(filepath.Join(e.Spec.Directory, "vm-status.json"), status, 0600); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			err = h.stop(e)
			h.mu.Unlock()
			if err != nil || e.Phase != "stopped" {
				t.Fatal(err, e.Phase)
			}
			e.Spec.EgressSocket = filepath.Join(e.Spec.Directory, "occupied")
			if err := os.WriteFile(e.Spec.EgressSocket, []byte("occupied"), 0600); err != nil {
				t.Fatal(err)
			}
			if transition == "start" {
				if err := h.start(e); err == nil {
					t.Fatal("started over dispatching B")
				}
			} else if w := hostActionCall(h, "env", "replace", "crash-blocked-replacement", "actor"); w.Code != 409 {
				t.Fatal(w.Code, w.Body.String())
			}
			if e.Spec.Incarnation != "actor" || predecessorOperation(e, "actor") != "" {
				t.Fatal("advanced over dispatching B")
			}
			h.probe = positiveProbe
			cRequest := bRequest
			cRequest.OperationID = "fresh-crash-recovery"
			if w := reconcileCall(h, cRequest); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			data, err := os.ReadFile(filepath.Join(e.Spec.Directory, "environment.json"))
			if err != nil {
				t.Fatal(err)
			}
			var afterC Environment
			if err := json.Unmarshal(data, &afterC); err != nil {
				t.Fatal(err)
			}
			e = &afterC
			h.envs["env"] = e
			if e.Actions[bRequest.OperationID].State != "dispatching" {
				t.Fatal("historical crashed action rewritten")
			}
			if predecessorOperation(e, "actor") != cRequest.OperationID {
				t.Fatal("positive C not retained after reload")
			}
			h.probe = func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error) {
				t.Fatal("positive C reprobed")
				return FenceProbeEvidence{}, nil
			}
			if err := h.start(e); err == nil || e.Spec.Incarnation == "actor" || e.Phase != "launching" {
				t.Fatal("C did not allow journaled advance", err, e.Phase)
			}
		})
	}
}
