package agentenv

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelObservationRetainedAndClearedOnReplacement(t *testing.T) {
	s := newStore(t)
	task := Task{OperationID: "task-model", Incarnation: "actor-one", Text: "task"}
	s.Begin(task)
	if s.Snapshot().ModelObservation.Status != "unknown" {
		t.Fatal("model guessed before observation")
	}
	raw := json.RawMessage(`{"thread":{"id":"thread-model"},"model":"fixture-model","modelProvider":"fixture-provider"}`)
	if err := s.ObserveModel("actor-one", task.OperationID, "thread/start", raw); err != nil {
		t.Fatal(err)
	}
	v := s.Snapshot().ModelObservation
	if v.Status != "observed" || v.Model != "fixture-model" || v.IncarnationID != "actor-one" || v.OperationID != task.OperationID || v.SourceMethod != "thread/start" || v.ObservedAt == "" {
		t.Fatalf("%+v", v)
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "session.json"))
	var stored State
	json.Unmarshal(b, &stored)
	if stored.ModelObservation != v {
		t.Fatal("model not durable across controller reconnect")
	}
	s.Close()
	next, err := Open(s.dir, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Snapshot().ModelObservation.Status != "unknown" || next.Snapshot().ModelObservation.Model != "" || next.Snapshot().ThreadID != "thread-model" {
		t.Fatal("replacement silently inherited old observation")
	}
	id := "resume-actor-two"
	next.BeginControl("resume", id, "actor-two", map[string]string{"thread_id": "thread-model"})
	if err = next.ObserveModel("actor-two", id, "thread/resume", raw); err != nil {
		t.Fatal(err)
	}
	if next.Snapshot().ModelObservation.SourceMethod != "thread/resume" || next.Snapshot().ModelObservation.IncarnationID != "actor-two" {
		t.Fatal("resume lost provenance")
	}
}
func TestModelMissingSelectionStaysUnknownAndWrongResumeRefused(t *testing.T) {
	s := newStore(t)
	s.Begin(Task{OperationID: "task", Incarnation: "actor-one", Text: "task"})
	if err := s.ObserveModel("actor-one", "task", "thread/start", json.RawMessage(`{"thread":{"id":"thread"}}`)); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().ModelObservation.Status != "unknown" {
		t.Fatal("missing adapter selection guessed")
	}
	s.BeginControl("resume", "resume", "actor-one", map[string]string{})
	if err := s.ObserveModel("actor-one", "resume", "thread/resume", json.RawMessage(`{"thread":{"id":"wrong"},"model":"fixture","modelProvider":"fixture"}`)); err == nil {
		t.Fatal("wrong thread provenance accepted")
	}
}
func TestUnsupportedPermissionResponseIsExplicit(t *testing.T) {
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	a := actorFixture()
	b.Actor = a
	id := json.RawMessage(`33`)
	b.pending[permissionKey(id)] = Message{ID: id, Method: "item/permissions/requestApproval"}
	body := `{"expected_incarnation":"actor-one","operation_id":"unsupported","decision":"accept"}`
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/permissions/"+permissionKey(id), strings.NewReader(body)))
	if w.Code != 501 || !strings.Contains(w.Body.String(), `"status":"unsupported"`) || a.answers != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}
