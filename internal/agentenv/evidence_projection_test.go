package agentenv

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecycleEvidenceExcludesRawProviderDetails(t *testing.T) {
	const canary = "SYNTHETIC-PROVIDER-DETAIL-CANARY"
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	b.Actor = actorFixture()
	if _, err := b.Submit(Task{OperationID: "projection-task", Incarnation: "actor-one", Text: "bounded test"}); err != nil {
		t.Fatal(err)
	}
	raw := []json.RawMessage{
		json.RawMessage(`{"threadId":"thread-one","turnId":"turn-one","error":{"code":401,"message":"` + canary + `","data":{"token":"` + canary + `"}},"willRetry":false,"metadata":{"accessToken":"` + canary + `"}}`),
		json.RawMessage(`{"threadId":"thread-one","turn":{"id":"turn-one","status":"failed","error":{"message":"` + canary + `"},"metadata":{"authorization":"` + canary + `"}},"account":{"token":"` + canary + `"}}`),
	}
	for i, method := range []string{"error", "turn/completed"} {
		if err := b.Observe(Message{Method: method, Params: raw[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if op := s.Snapshot().Operations["projection-task"]; op.Completion != "failed" {
		t.Fatalf("terminal identity lost: %+v", op)
	}
	for _, path := range []string{"/v1/events", "/v1/session"} {
		r := httptest.NewRecorder()
		b.Handler().ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 200 || bytes.Contains(r.Body.Bytes(), []byte(canary)) {
			t.Fatalf("%s exports provider details: %s", path, r.Body.String())
		}
	}
	snapshot, e := json.Marshal(s.Snapshot())
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(snapshot, []byte(canary)) {
		t.Fatal("snapshot retained raw provider details")
	}
	disk, e := os.ReadFile(filepath.Join(s.dir, "session.json"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(disk, []byte(canary)) {
		t.Fatal("durable journal retained raw provider details")
	}
	s.Close()
	reopened, e := Open(s.dir, "environment", "session", "actor-two")
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	for i, event := range reopened.Snapshot().Events {
		b, e := json.Marshal(event)
		if e != nil {
			t.Fatal(e)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(b, &fields) != nil {
			t.Fatal("invalid event")
		}
		var hash string
		json.Unmarshal(fields["params_sha256"], &hash)
		if hash != digest(raw[i]) {
			t.Fatal("original opaque evidence digest lost")
		}
	}
}

type privacyActor struct{ *fixtureActor }

func TestErrorProjectionDoesNotInventDiagnosticValues(t *testing.T) {
	for _, raw := range []string{`{"error":{"code":null},"willRetry":null}`, `{"error":{"code":"private diagnostic"},"willRetry":"private diagnostic"}`} {
		projected, _ := projectLifecycleEvidence("error", json.RawMessage(raw))
		if strings.Contains(string(projected), `"code"`) || strings.Contains(string(projected), `"willRetry"`) {
			t.Fatalf("invalid diagnostic became a reported value: %s", projected)
		}
	}
	projected, _ := projectLifecycleEvidence("error", json.RawMessage(`{"error":{"code":0},"willRetry":false}`))
	if !strings.Contains(string(projected), `"code":0`) || !strings.Contains(string(projected), `"willRetry":false`) {
		t.Fatalf("valid zero/false diagnostics lost: %s", projected)
	}
}

func (a privacyActor) Call(ctx context.Context, method string, p any) (json.RawMessage, error) {
	switch method {
	case "account/login/start":
		return json.RawMessage(`{"userCode":"SYNTHETIC-DEVICE-CODE-CANARY","deviceAuthId":"device-one","verificationUri":"https://auth.example/device"}`), nil
	case "account/read":
		return json.RawMessage(`{"account":null,"accessToken":"SYNTHETIC-ACCOUNT-CANARY"}`), nil
	}
	return a.fixtureActor.Call(ctx, method, p)
}

func TestLoginAndAccountDetailsStayOutsideJournal(t *testing.T) {
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	b.Actor = privacyActor{actorFixture()}
	r := httptest.NewRecorder()
	b.Handler().ServeHTTP(r, httptest.NewRequest("POST", "/v1/login", strings.NewReader(`{"expected_incarnation":"actor-one","operation_id":"private-login"}`)))
	if r.Code != 200 || !strings.Contains(r.Body.String(), "SYNTHETIC-DEVICE-CODE-CANARY") {
		t.Fatalf("supported owner login response missing: %s", r.Body.String())
	}
	r = httptest.NewRecorder()
	b.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/v1/session", nil))
	if r.Code != 200 || strings.Contains(r.Body.String(), "SYNTHETIC-") {
		t.Fatal("account details leaked through session")
	}
	raw, e := os.ReadFile(filepath.Join(s.dir, "session.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), "SYNTHETIC-") {
		t.Fatal("authentication response persisted")
	}
}
