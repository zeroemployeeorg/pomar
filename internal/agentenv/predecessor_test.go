package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
