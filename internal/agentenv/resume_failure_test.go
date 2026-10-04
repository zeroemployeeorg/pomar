package agentenv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFailedResumeKeepsEvidenceVisibleAndExecutionHeld(t *testing.T) {
	s := newStore(t)
	actor := actorFixture()
	original := NewBroker(s, t.TempDir())
	original.Actor = actor
	if _, err := original.Submit(Task{OperationID: "original", Incarnation: "actor-one", Text: "bounded edit"}); err != nil {
		t.Fatal(err)
	}
	directory := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(directory, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	b := NewBroker(next, t.TempDir())
	actor.resumeError = &RPCError{Code: -32602}
	b.Actor = actor
	if err := b.Resume(context.Background()); err == nil {
		t.Fatal("resume error suppressed")
	}
	if _, err := b.Submit(Task{OperationID: "new", Incarnation: "actor-two", Text: "must not run"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("new execution not held: %v", err)
	}
	if actor.count("turn/start") != 1 {
		t.Fatal("new task dispatched")
	}
	h := b.Handler()
	for _, path := range []string{"/v1/permissions/permission", "/v1/interrupt"} {
		body := `{"operation_id":"held-control","expected_incarnation":"actor-two"}`
		if path == "/v1/permissions/permission" {
			body = `{"operation_id":"held-control","expected_incarnation":"actor-two","decision":"accept"}`
		}
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("control not held: %s %d", path, w.Code)
		}
	}
	if _, ok := next.Snapshot().Controls["held-control"]; ok {
		t.Fatal("held control changed journal")
	}
	if actor.answers != 0 || actor.count("turn/interrupt") != 0 {
		t.Fatal("held control reached actor")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/operations/original?session_id=session&expected_incarnation=actor-one", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"turn_id":"turn-one"`) || !strings.Contains(w.Body.String(), `"state":"acceptance_unknown"`) {
		t.Fatalf("original evidence unavailable: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/session", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"held"`) || !strings.Contains(w.Body.String(), `"rpc_error_code":-32602`) || !strings.Contains(w.Body.String(), `"authenticated":true`) {
		t.Fatalf("recovery disposition missing: %s", w.Body.String())
	}
	if _, err := b.Submit(Task{OperationID: "stale", Incarnation: "actor-one", Text: "must not run"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale scope not refused: %v", err)
	}
}
