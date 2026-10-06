package agentenv

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func permissionAnswer(t *testing.T, b *Broker, key, operation, incarnation string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"operation_id": operation, "expected_incarnation": incarnation, "decision": "accept"})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/permissions/"+key, strings.NewReader(string(raw))))
	return w
}

func TestPermissionIdentitySeparatesRepeatedNativeIDsAcrossIncarnations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	s, err := Open(dir, "environment", "session", "actor-one")
	if err != nil {
		t.Fatal(err)
	}
	old := NewBroker(s, t.TempDir())
	id := json.RawMessage(`1`)
	request := Message{ID: id, Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"command":"go test ./..."}`)}
	if err = old.Observe(request); err != nil {
		t.Fatal(err)
	}
	oldKey := s.Snapshot().Events[0].PermissionID
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	current := NewBroker(s, t.TempDir())
	a := actorFixture()
	current.Actor = a
	if err = current.Observe(request); err != nil {
		t.Fatal(err)
	}
	events := s.Snapshot().Events
	newKey := events[1].PermissionID
	if oldKey == newKey || events[0].PermissionID != oldKey || events[0].Incarnation != "actor-one" {
		t.Fatal("permission identity collapsed or retained evidence changed")
	}
	if w := permissionAnswer(t, current, newKey, "stale-answer", "actor-one"); w.Code != 403 {
		t.Fatalf("stale answer: %d %s", w.Code, w.Body.String())
	}
	if w := permissionAnswer(t, current, oldKey, "old-key-answer", "actor-two"); w.Code != 409 || !strings.Contains(w.Body.String(), `"evidence":"durable_non_acceptance"`) {
		t.Fatalf("old key reached current request: %d %s", w.Code, w.Body.String())
	}
	if a.answers != 0 {
		t.Fatal("old permission identity reached the actor")
	}
	if w := permissionAnswer(t, current, newKey, "current-answer", "actor-two"); w.Code != 200 {
		t.Fatalf("current answer: %d %s", w.Code, w.Body.String())
	}
	if a.answers != 1 {
		t.Fatal("current native answer was not sent once")
	}
}

func TestUnknownPermissionRefusalIsDurableAndNeverBecomesADispatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	s, err := Open(dir, "environment", "session", "actor-one")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBroker(s, t.TempDir())
	a := actorFixture()
	b.Actor = a
	id := json.RawMessage(`7`)
	key := permissionKey("actor-one", id)
	first := permissionAnswer(t, b, key, "refused-answer", "actor-one")
	if first.Code != 409 || a.answers != 0 {
		t.Fatalf("unknown request dispatched: %d answers=%d", first.Code, a.answers)
	}
	if err = b.Observe(Message{ID: id, Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"command":"go test ./..."}`)}); err != nil {
		t.Fatal(err)
	}
	retry := permissionAnswer(t, b, key, "refused-answer", "actor-one")
	if retry.Code != 409 || retry.Body.String() != first.Body.String() || a.answers != 0 {
		t.Fatal("a later pending request turned the refused operation into an answer")
	}
	conflict := permissionAnswer(t, b, key+"other", "refused-answer", "actor-one")
	if conflict.Code != 409 || !strings.Contains(conflict.Body.String(), "already binds different input") || a.answers != 0 {
		t.Fatal("refusal released its input binding")
	}
	if w := permissionAnswer(t, b, key, "fresh-answer", "actor-one"); w.Code != 200 || a.answers != 1 {
		t.Fatal("fresh operation could not answer the live request once")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Snapshot().Controls["refused-answer"]
	if c.State != "refused" || c.Evidence != "durable_non_acceptance" || c.Incarnation != "actor-one" {
		t.Fatalf("refusal changed on recovery: %+v", c)
	}
	recovered := NewBroker(s, t.TempDir())
	w := httptest.NewRecorder()
	recovered.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/operations/refused-answer?session_id=session&expected_incarnation=actor-one", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"refused"`) || !strings.Contains(w.Body.String(), `"evidence":"durable_non_acceptance"`) {
		t.Fatalf("original refusal not inspectable: %d %s", w.Code, w.Body.String())
	}
}
