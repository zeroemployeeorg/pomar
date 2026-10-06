package agentenv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type controllerActor struct {
	*fixtureActor
	beforeAnswer func()
	writeError   error
	version      string
	threadParams any
}

func (a *controllerActor) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "initialize" {
		return json.Marshal(map[string]string{"userAgent": "pomar/" + a.version + " (test)"})
	}
	if method == "thread/start" {
		a.threadParams = params
	}
	return a.fixtureActor.Call(ctx, method, params)
}
func (a *controllerActor) Answer(id json.RawMessage, result any) error {
	if a.beforeAnswer != nil {
		a.beforeAnswer()
	}
	a.fixtureActor.Answer(id, result)
	return a.writeError
}

func controllerFixture(t *testing.T) (*Broker, *controllerActor, Message) {
	t.Helper()
	s := newStore(t)
	b := NewBroker(s, t.TempDir())
	a := &controllerActor{fixtureActor: actorFixture(), version: "0.160.0"}
	if err := b.ConfigureController([]string{"inbox", "ack", "answer"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Initialize(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit(Task{OperationID: "task", Incarnation: "actor-one", Text: "test"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a.threadParams)
	if !strings.Contains(string(raw), "dynamicTools") {
		t.Fatal("tools not registered")
	}
	message := Message{ID: json.RawMessage(`42`), Method: "item/tool/call", Params: json.RawMessage(`{"threadId":"thread-one","turnId":"turn-one","callId":"call-one","tool":"pomar_controller_request","arguments":{"capability":"inbox","data":"bounded payload"}}`)}
	return b, a, message
}
func firstController(t *testing.T, b *Broker) ControllerRequest {
	t.Helper()
	for _, r := range b.Store.Snapshot().ControllerRequests {
		return r
	}
	t.Fatal("no request")
	return ControllerRequest{}
}

func TestControllerReplyDurableDuplicateConflictAndDistinctAcknowledgement(t *testing.T) {
	b, a, m := controllerFixture(t)
	if err := b.Observe(m); err != nil {
		t.Fatal(err)
	}
	req := firstController(t, b)
	reply := ControllerReply{ReplyID: "reply-one", Binding: req.Binding, Text: "delivery only", Success: true}
	a.beforeAnswer = func() {
		raw, err := os.ReadFile(filepath.Join(b.Store.dir, "session.json"))
		if err != nil {
			t.Fatal(err)
		}
		var state State
		if json.Unmarshal(raw, &state) != nil {
			t.Fatal("journal invalid")
		}
		durable := state.ControllerRequests[req.ID]
		if durable.Reply == nil || durable.Reply.ReplyID != reply.ReplyID || durable.ReplySHA256 == "" || durable.State != "dispatching" {
			t.Fatal("native write preceded durable reply")
		}
	}
	got, err := b.ReplyController(req.ID, reply)
	if err != nil || got.State != "written" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = b.ReplyController(req.ID, reply); err != nil {
		t.Fatal(err)
	}
	if err = b.Observe(m); err != nil {
		t.Fatal(err)
	}
	changed := reply
	changed.Text = "changed bytes"
	if _, err = b.ReplyController(req.ID, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed reply accepted", err)
	}
	if a.answers != 1 {
		t.Fatal("duplicate native delivery")
	}
	// Ack is a fresh authenticated actor request, not inferred from delivery or completion.
	ack := m
	ack.ID = json.RawMessage(`43`)
	ack.Params = json.RawMessage(`{"threadId":"thread-one","turnId":"turn-one","callId":"call-ack","tool":"pomar_controller_request","arguments":{"capability":"ack","data":"message-one"}}`)
	if err = b.Observe(ack); err != nil {
		t.Fatal(err)
	}
	if len(b.Store.Snapshot().ControllerRequests) != 2 {
		t.Fatal("ack did not remain distinct")
	}
}

func TestControllerConcurrentRepliesAndLostNativeWriteNeverReplay(t *testing.T) {
	b, a, m := controllerFixture(t)
	a.writeError = errors.New("write lost after partial send")
	if err := b.Observe(m); err != nil {
		t.Fatal(err)
	}
	req := firstController(t, b)
	reply := ControllerReply{ReplyID: "reply", Binding: req.Binding, Text: "data", Success: true}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := b.ReplyController(req.ID, reply)
			if err != nil || got.State != "acceptance_unknown" {
				t.Errorf("%+v %v", got, err)
			}
		}()
	}
	wg.Wait()
	if a.answers != 1 {
		t.Fatal("uncertain native reply replayed")
	}
	s := b.Store
	s.Close()
	next, err := Open(s.dir, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	replacement := NewBroker(next, t.TempDir())
	replacement.Actor = actorFixture()
	if _, err = replacement.ReplyController(req.ID, reply); !errors.Is(err, ErrStale) {
		t.Fatal("old incarnation replayed", err)
	}
	if next.Snapshot().ControllerRequests[req.ID].State != "acceptance_unknown" {
		t.Fatal("uncertainty cleared by restart")
	}
}

func TestControllerRejectsUnboundRequestsAndReplies(t *testing.T) {
	mutations := []string{
		`"thread-one"|"other-thread"`, `"turn-one"|"other-turn"`, `"inbox"|"unconfigured"`,
		`"pomar_controller_request"|"arbitrary_tool"`, `"data":"bounded payload"|"data":"bounded payload","socket_path":"/tmp/operator.sock"`,
		`"data":"bounded payload"|"data":null`, `"call-one"|""`,
	}
	for _, mutation := range mutations {
		t.Run(mutation, func(t *testing.T) {
			b, a, m := controllerFixture(t)
			parts := strings.Split(mutation, "|")
			m.Params = json.RawMessage(strings.Replace(string(m.Params), parts[0], parts[1], 1))
			if err := b.Observe(m); err == nil {
				t.Fatal("unbound native call accepted")
			}
			if a.answers != 0 || len(b.Store.Snapshot().ControllerRequests) != 0 {
				t.Fatal("rejected call acted")
			}
		})
	}
	b, a, m := controllerFixture(t)
	if err := b.Observe(m); err != nil {
		t.Fatal(err)
	}
	req := firstController(t, b)
	for _, field := range []string{"environment", "session", "operation", "input", "thread", "turn", "request", "call", "incarnation"} {
		reply := ControllerReply{ReplyID: "reply", Binding: req.Binding, Text: "data", Success: true}
		switch field {
		case "environment":
			reply.Binding.EnvironmentID = "other"
		case "session":
			reply.Binding.SessionID = "other"
		case "operation":
			reply.Binding.OperationID = "other"
		case "input":
			reply.Binding.InputSHA256 = "other"
		case "thread":
			reply.Binding.ThreadID = "other"
		case "turn":
			reply.Binding.TurnID = "other"
		case "request":
			reply.Binding.NativeRequestID = json.RawMessage(`99`)
		case "call":
			reply.Binding.CallID = "other"
		case "incarnation":
			reply.Binding.Incarnation = "old"
		}
		if _, err := b.ReplyController(req.ID, reply); !errors.Is(err, ErrStale) {
			t.Fatal("binding mutation accepted", field, err)
		}
	}
	b.resumeFailure = errors.New("held")
	if _, err := b.ReplyController(req.ID, ControllerReply{ReplyID: "held", Binding: req.Binding, Text: "data"}); err == nil {
		t.Fatal("resume hold bypassed")
	}
	b.resumeFailure = nil
	b.Store.Observe("actor-one", "turn/completed", nil, json.RawMessage(`{"threadId":"thread-one","turn":{"id":"turn-one","status":"completed"}}`))
	if _, err := b.ReplyController(req.ID, ControllerReply{ReplyID: "late", Binding: req.Binding, Text: "data"}); !errors.Is(err, ErrStale) {
		t.Fatal("completed turn received reply", err)
	}
	if a.answers != 0 {
		t.Fatal("rejected binding wrote to actor")
	}
}

func TestControllerAdapterConfigurationAndHTTPBounds(t *testing.T) {
	for _, names := range [][]string{{"inbox", "inbox"}, {"/bin/sh"}, {"Socket"}} {
		if ValidateControllerCapabilities(names) == nil {
			t.Fatal("bad owner configuration accepted")
		}
	}
	b := NewBroker(newStore(t), t.TempDir())
	if err := b.ConfigureController([]string{"inbox"}); err != nil {
		t.Fatal(err)
	}
	if b.Initialize(t.Context(), &controllerActor{fixtureActor: actorFixture(), version: "0.159.0"}) == nil {
		t.Fatal("unsupported adapter enabled")
	}
	b, a, m := controllerFixture(t)
	b.controllerReady = false
	if b.Observe(m) == nil {
		t.Fatal("unnegotiated tool accepted")
	}
	b.controllerReady = true
	b.Observe(m)
	req := firstController(t, b)
	for _, body := range []string{`{}`, `{"reply_id":"x","command":"uname"}`, strings.Repeat("x", 65<<10), `{} {}`} {
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/controller/requests/"+req.ID+"/reply", strings.NewReader(body)))
		if w.Code == 200 {
			t.Fatal("malformed reply accepted")
		}
	}
	if a.answers != 0 {
		t.Fatal("bad HTTP reply delivered")
	}
}

func TestControllerJournalFailurePreventsNativeWrite(t *testing.T) {
	b, a, m := controllerFixture(t)
	if err := b.Observe(m); err != nil {
		t.Fatal(err)
	}
	req := firstController(t, b)
	old := b.Store.dir
	b.Store.dir = filepath.Join(old, "missing")
	reply := ControllerReply{ReplyID: "reply", Binding: req.Binding, Text: "data", Success: true}
	if _, err := b.ReplyController(req.ID, reply); err == nil {
		t.Fatal("journal failure ignored")
	}
	b.Store.dir = old
	if _, err := b.ReplyController(req.ID, reply); err == nil {
		t.Fatal("poisoned journal allowed retry")
	}
	if a.answers != 0 {
		t.Fatal("unsynced native reply")
	}
}

func TestControllerNativeDuplicateAndReplyIDsCannotBeRebound(t *testing.T) {
	b, a, m := controllerFixture(t)
	if err := b.Observe(m); err != nil {
		t.Fatal(err)
	}
	req := firstController(t, b)
	changed := m
	changed.Params = json.RawMessage(strings.Replace(string(m.Params), "bounded payload", "other payload", 1))
	if !errors.Is(b.Observe(changed), ErrConflict) {
		t.Fatal("native id rebound")
	}
	reply := ControllerReply{ReplyID: "single-reply", Binding: req.Binding, Text: "response", Success: true}
	if _, err := b.ReplyController(req.ID, reply); err != nil {
		t.Fatal(err)
	}
	next := m
	next.ID = json.RawMessage(`44`)
	next.Params = json.RawMessage(strings.Replace(string(m.Params), "call-one", "call-next", 1))
	if err := b.Observe(next); err != nil {
		t.Fatal(err)
	}
	for _, request := range b.Store.Snapshot().ControllerRequests {
		if request.ID != req.ID {
			reply.Binding = request.Binding
			if _, err := b.ReplyController(request.ID, reply); !errors.Is(err, ErrConflict) {
				t.Fatal("reply id rebound")
			}
		}
	}
	if a.answers != 1 {
		t.Fatal("rebound reply delivered")
	}
}

func TestControllerRetainedThreadAndInterruptedDispatchStayFenced(t *testing.T) {
	b, a, m := controllerFixture(t)
	if err := b.Observe(m); err != nil {
		t.Fatal(err)
	}
	request := firstController(t, b)
	// Simulate an interrupted process after durable dispatch, before native outcome recording.
	b.Store.mu.Lock()
	request.State = "dispatching"
	request.Reply = &ControllerReply{ReplyID: "reply", Binding: request.Binding, Text: "data", Success: true}
	payload, _ := json.Marshal(nativeReply(*request.Reply))
	request.ReplySHA256 = digest(payload)
	b.Store.s.ControllerRequests[request.ID] = request
	err := b.Store.save()
	b.Store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	b.Store.Close()
	next, err := Open(b.Store.dir, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	recovered := NewBroker(next, t.TempDir())
	if !errors.Is(recovered.ConfigureController([]string{"replacement"}), ErrConflict) {
		t.Fatal("retained thread tool set replaced")
	}
	if err = recovered.ConfigureController([]string{"inbox", "ack", "answer"}); err != nil {
		t.Fatal(err)
	}
	if next.Snapshot().ControllerRequests[request.ID].State != "acceptance_unknown" || a.answers != 0 {
		t.Fatal("restart inferred acceptance")
	}
}

type shortRPCWriter struct{}

func (shortRPCWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestControllerNativeShortWriteIsUncertain(t *testing.T) {
	rpc := NewRPC(strings.NewReader(""), shortRPCWriter{}, nil)
	if err := rpc.Notify("initialized", map[string]any{}); err == nil {
		t.Fatal("short native write accepted")
	}
}
