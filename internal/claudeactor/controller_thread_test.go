package claudeactor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// firstThenHold completes the first turn and holds every later one until
// interrupted.
func firstThenHold(f *fakeClaude, session string, n int, l string) []string {
	if n == 1 {
		return happy(f, session, n, l)
	}
	return holdTurn(f, session, n, l)
}

// A controller request in a thread's second turn binds to that turn's
// operation. Found in cc-4: only the operation that started the thread knew
// its thread, so every later turn's request was refused unrecorded.
func TestControllerRequestInALaterTurn(t *testing.T) {
	h, err := openController(t, filepath.Join(t.TempDir(), "state"), "inc-1", firstThenHold, true, "2.1.280", []string{"inbox"})
	if err != nil {
		t.Fatal(err)
	}
	sock := bridge(t, h.actor)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-1", finished)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-2", Incarnation: "inc-1", Text: "y"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-2", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	defer func() { h.fakes[0].Interrupt(); h.waitOp("op-2", finished) }()
	if op := h.store.Snapshot().Operations["op-2"]; op.ThreadID != h.store.Snapshot().ThreadID {
		t.Fatalf("a later operation is not bound to the thread: %+v", op)
	}
	args := `{"capability":"inbox","data":"next"}`
	emitToolUse(t, h, "toolu_c2", ControllerToolName, args)
	ch := forwardAsync(sock, ControllerTool, args)
	id, r := waitControllerRequest(t, h.broker)
	if r.Binding.OperationID != "op-2" || r.Binding.CallID != "toolu_c2" {
		t.Fatalf("bound to %+v", r.Binding)
	}
	replyController(t, h.broker, id, r, "reply-1", "post 7", true)
	if got := waitForward(t, ch); got.IsError || got.Text != "post 7" {
		t.Fatalf("tool result %+v", got)
	}
}

// After a replacement resumes the thread, the next operation's controller
// request binds to it in the new incarnation.
func TestControllerRequestAfterResume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := openController(t, dir, "inc-1", happy, true, "2.1.280", []string{"inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	first.waitOp("op-1", finished)
	first.store.Close()

	second, err := openController(t, dir, "inc-2", holdTurn, true, "2.1.280", []string{"inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.broker.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := bridge(t, second.actor)
	if _, err := second.broker.Submit(agentenv.Task{OperationID: "op-2", Incarnation: "inc-2", Text: "y"}); err != nil {
		t.Fatal(err)
	}
	second.waitOp("op-2", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	defer func() { second.fakes[0].Interrupt(); second.waitOp("op-2", finished) }()
	args := `{"capability":"inbox","data":"next"}`
	emitToolUse(t, second, "toolu_r1", ControllerToolName, args)
	ch := forwardAsync(sock, ControllerTool, args)
	id, r := waitControllerRequest(t, second.broker)
	if r.Binding.OperationID != "op-2" || r.Binding.Incarnation != "inc-2" {
		t.Fatalf("bound to %+v", r.Binding)
	}
	replyController(t, second.broker, id, r, "reply-1", "post 8", true)
	if got := waitForward(t, ch); got.IsError || got.Text != "post 8" {
		t.Fatalf("tool result %+v", got)
	}
}
