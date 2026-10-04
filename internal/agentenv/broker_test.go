package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fixtureActor struct {
	mu            sync.Mutex
	calls         map[string]int
	lost          bool
	authenticated bool
	done          chan struct{}
	answers       int
	started       chan struct{}
	release       chan struct{}
	resumeError   error
}

func actorFixture() *fixtureActor {
	return &fixtureActor{calls: map[string]int{}, authenticated: true, done: make(chan struct{})}
}
func (a *fixtureActor) Call(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	a.mu.Lock()
	a.calls[method]++
	lost := a.lost
	auth := a.authenticated
	started, release := a.started, a.release
	a.mu.Unlock()
	switch method {
	case "account/read":
		if auth {
			return json.RawMessage(`{"account":{"type":"chatgpt"}}`), nil
		}
		return json.RawMessage(`{"account":null}`), nil
	case "thread/start":
		return json.RawMessage(`{"thread":{"id":"thread-one"}}`), nil
	case "thread/resume":
		return nil, a.resumeError
	case "turn/start":
		if started != nil {
			close(started)
			<-release
		}
		if lost {
			return nil, errors.New("response lost after acceptance")
		}
		return json.RawMessage(`{"turn":{"id":"turn-one"}}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}
func (a *fixtureActor) Notify(string, any) error { return nil }
func (a *fixtureActor) Answer(json.RawMessage, any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers++
	return nil
}
func (a *fixtureActor) Done() <-chan struct{} { return a.done }
func (a *fixtureActor) count(method string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[method]
}

func TestLostAcceptanceDoesNotDispatchTwice(t *testing.T) {
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	a := actorFixture()
	a.lost = true
	b.Actor = a
	task := Task{OperationID: "task", Incarnation: "actor-one", Text: "edit and test"}
	if op, err := b.Submit(task); err == nil || op.State != "acceptance_unknown" {
		t.Fatalf("lost reply: %+v %v", op, err)
	}
	a.authenticated = false
	if op, err := b.Submit(task); err != nil || op.State != "acceptance_unknown" {
		t.Fatalf("retry did not inspect retained record: %+v %v", op, err)
	}
	if a.count("turn/start") != 1 {
		t.Fatal("uncertain task repeated")
	}
}

func TestControllerDisconnectPreservesActorAndOperation(t *testing.T) {
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	a := actorFixture()
	a.started = make(chan struct{})
	a.release = make(chan struct{})
	b.Actor = a
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	body := `{"operation_id":"task","expected_incarnation":"actor-one","text":"edit and test"}`
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/tasks", strings.NewReader(body))
	finished := make(chan struct{})
	go func() {
		resp, _ := http.DefaultClient.Do(r)
		if resp != nil {
			resp.Body.Close()
		}
		close(finished)
	}()
	<-a.started
	cancel()
	<-finished
	close(a.release)
	// Reconnect with the same operation while the first handler completes.
	resp, err := http.Post(server.URL+"/v1/tasks", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("reconnect: %s", data)
	}
	if a.count("turn/start") != 1 {
		t.Fatal("disconnect repeated actor task")
	}
	state := s.Snapshot()
	if state.SessionID != "session" || state.Incarnation != "actor-one" || state.ThreadID != "thread-one" {
		t.Fatalf("reconnect changed identity: %+v", state)
	}
}

func TestOldIncarnationCannotAnswerOrExecute(t *testing.T) {
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	a := actorFixture()
	b.Actor = a
	id := json.RawMessage(`17`)
	if err := b.Observe(Message{ID: id, Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"command":"go test ./cmd/pomar-shim"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/tasks", "/v1/permissions/" + permissionKey(id), "/v1/interrupt", "/v1/login"} {
		body := `{"expected_incarnation":"old"}`
		if path == "/v1/tasks" {
			body = `{"expected_incarnation":"old","operation_id":"task","text":"edit"}`
		}
		if strings.Contains(path, "permissions") {
			body = `{"expected_incarnation":"old","decision":"accept"}`
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		b.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("%s accepted stale incarnation: %d %s", path, w.Code, w.Body.String())
		}
	}
	if a.count("turn/start") != 0 || a.answers != 0 {
		t.Fatal("stale caller reached actor")
	}
	if err := b.Observe(Message{Method: "account/updated", Params: json.RawMessage(`{"accessToken":"synthetic-secret"}`)}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(s.Snapshot())
	if strings.Contains(string(data), "synthetic-secret") {
		t.Fatal("authentication bytes entered public journal")
	}
}

func TestInspectionBindsOriginalIdentityAndEventCursor(t *testing.T) {
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	for _, tc := range []struct {
		query    string
		code     int
		evidence string
	}{
		{"", 409, "unknown"},
		{"?session_id=session&expected_incarnation=old", 409, "unknown"},
		{"?session_id=wrong&expected_incarnation=actor-one", 409, "unknown"},
		{"?session_id=session&expected_incarnation=actor-one", 404, "durable_non_acceptance"},
	} {
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/operations/absent"+tc.query, nil))
		if w.Code != tc.code || !strings.Contains(w.Body.String(), `"evidence":"`+tc.evidence+`"`) {
			t.Fatalf("inspection: %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/events?after=1", nil))
	if w.Code != 409 {
		t.Fatal("future event cursor silently accepted")
	}
}

func TestResultExportsDiffAndRefusesRedirectedUntrackedFiles(t *testing.T) {
	workspace := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "fixture"}} {
		if data, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", data, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("fixture output"), 0600); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(newStore(t), workspace)
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/result", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fixture output") {
		t.Fatalf("result: %d %s", w.Code, w.Body.String())
	}
	outside := filepath.Join(t.TempDir(), "private")
	os.WriteFile(outside, []byte("synthetic-private"), 0600)
	os.Symlink(outside, filepath.Join(workspace, "redirect"))
	w = httptest.NewRecorder()
	b.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/result", nil))
	if w.Code != 409 || strings.Contains(w.Body.String(), "synthetic-private") {
		t.Fatal("redirected file exported")
	}
}
