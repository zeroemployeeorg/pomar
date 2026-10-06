package claudeactor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// fakeClaude is a scripted stand-in for one `claude -p` stream-json process:
// for each user line on stdin, script returns the stdout lines to emit.
type fakeClaude struct {
	args      []string
	stdinR    *io.PipeReader
	stdinW    *io.PipeWriter
	stdoutR   *io.PipeReader
	stdoutW   *io.PipeWriter
	mu        sync.Mutex
	inputs    []string
	interrupt chan struct{}
	exited    chan struct{}
}

type script func(f *fakeClaude, session string, n int, line string) []string

func newFake(args []string, s script) *fakeClaude {
	f := &fakeClaude{args: args, interrupt: make(chan struct{}, 1), exited: make(chan struct{})}
	f.stdinR, f.stdinW = io.Pipe()
	f.stdoutR, f.stdoutW = io.Pipe()
	session := ""
	for i, a := range args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(args) {
			session = args[i+1]
		}
	}
	go func() {
		defer close(f.exited)
		defer f.stdoutW.Close()
		r := bufio.NewScanner(f.stdinR)
		n := 0
		for r.Scan() {
			n++
			f.mu.Lock()
			f.inputs = append(f.inputs, r.Text())
			f.mu.Unlock()
			for _, out := range s(f, session, n, r.Text()) {
				if out == "WAIT-INTERRUPT" {
					<-f.interrupt
					continue
				}
				if out == "EXIT" {
					// As qualified on 2.1.280: print mode exits after an
					// interrupted turn's result.
					return
				}
				fmt.Fprintln(f.stdoutW, out)
			}
		}
	}()
	return f
}

func (f *fakeClaude) Stdin() io.WriteCloser { return f.stdinW }
func (f *fakeClaude) Stdout() io.Reader     { return f.stdoutR }
func (f *fakeClaude) Interrupt() error      { f.interrupt <- struct{}{}; return nil }
func (f *fakeClaude) Wait() error           { <-f.exited; return nil }

func line(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }

// a successful turn: init, one assistant message, one tool result, success.
func happy(_ *fakeClaude, session string, _ int, _ string) []string {
	return []string{
		line(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": "claude-test"}),
		line(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"role": "assistant", "content": []any{map[string]string{"type": "text", "text": "editing"}}}}),
		line(map[string]any{"type": "user", "session_id": session, "message": map[string]any{"role": "user", "content": []any{map[string]string{"type": "tool_result", "content": "ok"}}}}),
		line(map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false}),
	}
}

type harness struct {
	t      *testing.T
	dir    string
	store  *agentenv.Store
	broker *agentenv.Broker
	actor  *Actor
	mu     sync.Mutex
	fakes  []*fakeClaude
}

var counter int

func open(t *testing.T, dir, incarnation string, s script, authenticated bool) *harness {
	t.Helper()
	store, err := agentenv.Open(dir, "env-1", "session-1", incarnation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &harness{t: t, dir: dir, store: store, broker: agentenv.NewBroker(store, "/work")}
	cfg := Config{
		Version:       "2.1.280",
		Args:          []string{"--setting-sources", "user", "--strict-mcp-config"},
		Authenticated: func() bool { return authenticated },
		NewID: func() string {
			h.mu.Lock()
			defer h.mu.Unlock()
			counter++
			return fmt.Sprintf("00000000-0000-4000-8000-%012d", counter)
		},
		Start: func(args []string) (Process, error) {
			f := newFake(args, s)
			h.mu.Lock()
			h.fakes = append(h.fakes, f)
			h.mu.Unlock()
			return f, nil
		},
	}
	h.actor = New(cfg, h.broker.Observe)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.broker.Initialize(ctx, h.actor); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) waitOp(id string, want func(agentenv.Operation) bool) agentenv.Operation {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op := h.store.Snapshot().Operations[id]
		if want(op) {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	op := h.store.Snapshot().Operations[id]
	h.t.Fatalf("operation %s never reached the wanted state: %+v", id, op)
	return op
}

func finished(op agentenv.Operation) bool { return op.State == "finished" }

// A task submitted through the shared broker reaches Claude Code once, is
// acknowledged by real stream output, and completes from the result message.
func TestTaskThroughTheSharedBroker(t *testing.T) {
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", happy, true)
	op, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "fix the port check"})
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "accepted" || op.TurnID == "" {
		t.Fatalf("submitted: %+v", op)
	}
	op = h.waitOp("op-1", finished)
	if op.Completion != "completed" || !op.ActorAcknowledged {
		t.Fatalf("finished: %+v", op)
	}
	s := h.store.Snapshot()
	f := h.fakes[0]
	args := strings.Join(f.args, " ")
	for _, want := range []string{"-p --input-format stream-json --output-format stream-json --verbose", "--setting-sources user --strict-mcp-config", "--session-id " + s.ThreadID} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	f.mu.Lock()
	inputs := append([]string(nil), f.inputs...)
	f.mu.Unlock()
	if len(inputs) != 1 || !strings.Contains(inputs[0], `"type":"user"`) || !strings.Contains(inputs[0], "Pomar operation op-1.\\nfix the port check") {
		t.Fatalf("the task reached Claude Code as %q", inputs)
	}
	var methods []string
	for _, e := range s.Events {
		methods = append(methods, e.Method)
	}
	if got := strings.Join(methods, ","); got != "turn/started,item/completed,item/completed,turn/completed" {
		t.Fatalf("journaled events %s", got)
	}
	// The same operation again is inspection only: Claude Code gets nothing new.
	if op, err = h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "fix the port check"}); err != nil || op.Completion != "completed" {
		t.Fatalf("a repeated operation: %+v, %v", op, err)
	}
	f.mu.Lock()
	n := len(f.inputs)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("a repeated operation reached Claude Code again (%d inputs)", n)
	}
}

// Without a configured credential, nothing is started and nothing is sent.
func TestUnauthenticatedRefusesBeforeAnyProcess(t *testing.T) {
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", happy, false)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("unauthenticated submit: %v", err)
	}
	if len(h.fakes) != 0 {
		t.Fatal("a Claude Code process was started without a credential")
	}
	if _, err := h.actor.Call(context.Background(), "account/login/start", map[string]string{"type": "chatgptDeviceCode"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("login: %v", err)
	}
}

// A failed result is a failed completion, never success.
func TestFailedResultIsNotSuccess(t *testing.T) {
	fail := func(_ *fakeClaude, session string, _ int, _ string) []string {
		return []string{line(map[string]any{"type": "result", "subtype": "error_during_execution", "session_id": session, "is_error": true})}
	}
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", fail, true)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if op := h.waitOp("op-1", finished); op.Completion != "failed" {
		t.Fatalf("completion %q", op.Completion)
	}
}

// The broker's interrupt route reaches the running turn, which ends interrupted.
func TestInterruptThroughTheBroker(t *testing.T) {
	slow := func(_ *fakeClaude, session string, _ int, _ string) []string {
		return []string{
			line(map[string]any{"type": "system", "subtype": "init", "session_id": session}),
			"WAIT-INTERRUPT",
			line(map[string]any{"type": "result", "subtype": "error_during_execution", "session_id": session, "is_error": true}),
		}
	}
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", slow, true)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-1", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	srv := httptest.NewServer(h.broker.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/interrupt", "application/json", bytes.NewBufferString(`{"expected_incarnation":"inc-1","operation_id":"int-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("interrupt: HTTP %d", resp.StatusCode)
	}
	if op := h.waitOp("op-1", finished); op.Completion != "interrupted" {
		t.Fatalf("completion %q", op.Completion)
	}
}

// A second turn while one runs is refused by the actor, not queued.
func TestOneTurnAtATime(t *testing.T) {
	hold := func(_ *fakeClaude, session string, _ int, _ string) []string {
		return []string{line(map[string]any{"type": "system", "subtype": "init", "session_id": session}), "WAIT-INTERRUPT"}
	}
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", hold, true)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	thread := h.store.Snapshot().ThreadID
	if _, err := h.actor.Call(context.Background(), "turn/start", map[string]any{"threadId": thread, "input": []map[string]string{{"type": "text", "text": "y"}}}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second turn: %v", err)
	}
	h.fakes[0].Interrupt()
}

// Output from another Claude Code session ends the adapter connection and
// never completes the operation.
func TestForeignSessionOutputIsRefused(t *testing.T) {
	foreign := func(_ *fakeClaude, _ string, _ int, _ string) []string {
		return []string{line(map[string]any{"type": "result", "subtype": "success", "session_id": "11111111-1111-4111-8111-111111111111"})}
	}
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", foreign, true)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.actor.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the actor did not end on foreign session output")
	}
	if op := h.store.Snapshot().Operations["op-1"]; op.Completion != "" {
		t.Fatalf("foreign output completed the operation: %+v", op)
	}
}

// A replacement incarnation resumes the same Claude Code session, claims no
// earlier turn outcome, and never replays the earlier task.
func TestResumeAfterReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	hold := func(_ *fakeClaude, session string, _ int, _ string) []string {
		return []string{line(map[string]any{"type": "system", "subtype": "init", "session_id": session}), "WAIT-INTERRUPT"}
	}
	first := open(t, dir, "inc-1", hold, true)
	if _, err := first.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	thread := first.store.Snapshot().ThreadID
	first.store.Close()

	second := open(t, dir, "inc-2", happy, true)
	if err := second.broker.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := second.fakes[0]
	if args := strings.Join(f.args, " "); !strings.Contains(args, "--resume "+thread) || strings.Contains(args, "--session-id") {
		t.Fatalf("resume args %q", args)
	}
	if op := second.store.Snapshot().Operations["op-1"]; op.Completion != "" {
		t.Fatalf("resume inferred an outcome for the earlier operation: %+v", op)
	}
	f.mu.Lock()
	n := len(f.inputs)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("resume replayed %d input(s)", n)
	}
	first.fakes[0].Interrupt()
}

// After an interrupted turn, Claude Code's print mode exits (qualified on
// 2.1.280). That is not actor departure: the next task resumes the same
// session in a new process, and the earlier task is never replayed.
func TestNextTaskAfterInterruptResumesTheSession(t *testing.T) {
	calls := 0
	script := func(_ *fakeClaude, session string, _ int, _ string) []string {
		calls++
		if calls == 1 {
			return []string{
				line(map[string]any{"type": "system", "subtype": "init", "session_id": session}),
				"WAIT-INTERRUPT",
				line(map[string]any{"type": "result", "subtype": "error_during_execution", "session_id": session, "is_error": true}),
				"EXIT",
			}
		}
		return happy(nil, session, 0, "")
	}
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", script, true)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "first"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-1", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	thread := h.store.Snapshot().ThreadID
	if _, err := h.actor.Call(context.Background(), "turn/interrupt", map[string]string{"threadId": thread}); err != nil {
		t.Fatal(err)
	}
	if op := h.waitOp("op-1", finished); op.Completion != "interrupted" {
		t.Fatalf("first completion %q", op.Completion)
	}
	<-h.fakes[0].exited
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-2", Incarnation: "inc-1", Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if op := h.waitOp("op-2", finished); op.Completion != "completed" {
		t.Fatalf("second completion %q", op.Completion)
	}
	select {
	case <-h.actor.Done():
		t.Fatal("the actor reported departure after an interrupted turn")
	default:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.fakes) != 2 {
		t.Fatalf("%d processes, want 2", len(h.fakes))
	}
	second := h.fakes[1]
	if args := strings.Join(second.args, " "); !strings.Contains(args, "--resume "+thread) {
		t.Fatalf("the next process args %q", args)
	}
	second.mu.Lock()
	defer second.mu.Unlock()
	if len(second.inputs) != 1 || !strings.Contains(second.inputs[0], "Pomar operation op-2.") {
		t.Fatalf("the next process got %q (the earlier task must not be replayed)", second.inputs)
	}
}
