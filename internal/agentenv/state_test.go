package agentenv

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "journal"), "environment", "session", "actor-one")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAcceptanceCrashAndReplacementNeverReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	s, err := Open(dir, "environment", "session", "actor-one")
	if err != nil {
		t.Fatal(err)
	}
	task := Task{OperationID: "task-one", Incarnation: "actor-one", Text: "edit and test"}
	op, dispatch, err := s.Begin(task)
	if err != nil || !dispatch || op.State != "dispatching" {
		t.Fatalf("begin: %+v %v %v", op, dispatch, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk State
	if json.Unmarshal(data, &disk) != nil || disk.Operations[task.OperationID].State != "dispatching" {
		t.Fatal("dispatch happened before durable acceptance")
	}
	if err := s.Thread("actor-one", "thread-one"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(dir, "environment", "session", "actor-one"); err == nil {
		t.Fatal("new actor reused incarnation")
	}
	recovered, err := Open(dir, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	state := recovered.Snapshot()
	if state.SessionID != "session" || state.ThreadID != "thread-one" || state.Operations[task.OperationID].State != "acceptance_unknown" {
		t.Fatalf("lost recovery state: %+v", state)
	}
	if _, _, err := recovered.Begin(task); !errors.Is(err, ErrStale) {
		t.Fatalf("old execution accepted: %v", err)
	}
	if err := recovered.Observe("actor-one", "turn/completed", nil, json.RawMessage(`{"threadId":"thread-one","turn":{"id":"old-turn","status":"completed"}}`)); !errors.Is(err, ErrStale) {
		t.Fatalf("old answer accepted: %v", err)
	}
	if _, _, err := recovered.Begin(Task{OperationID: "task-two", Incarnation: "actor-two", Text: "replacement"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("uncertain old task silently bypassed: %v", err)
	}
}

func TestRetryConflictAndSeparateAcknowledgementCompletion(t *testing.T) {
	s := newStore(t)
	task := Task{OperationID: "task", Incarnation: "actor-one", Text: "edit"}
	if _, _, err := s.Begin(task); err != nil {
		t.Fatal(err)
	}
	if err := s.Thread("actor-one", "thread"); err != nil {
		t.Fatal(err)
	}
	if err := s.Accepted("actor-one", "task", "turn"); err != nil {
		t.Fatal(err)
	}
	op, dispatch, err := s.Begin(task)
	if err != nil || dispatch || op.ActorAcknowledged || op.Completion != "" {
		t.Fatalf("acceptance conflated: %+v %v %v", op, dispatch, err)
	}
	task.Text = "different"
	if _, _, err := s.Begin(task); !errors.Is(err, ErrConflict) {
		t.Fatal("changed operation input accepted")
	}
	if err := s.Observe("actor-one", "turn/started", nil, json.RawMessage(`{"threadId":"thread","turn":{"id":"turn"}}`)); err != nil {
		t.Fatal(err)
	}
	if !s.Snapshot().Operations["task"].ActorAcknowledged {
		t.Fatal("actor acknowledgement missing")
	}
	if err := s.Observe("actor-one", "turn/completed", nil, json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"interrupted"}}`)); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().Operations["task"].Completion; got != "interrupted" {
		t.Fatalf("interruption became success: %q", got)
	}
}

func TestJournalOwnershipAndWriteFailureHold(t *testing.T) {
	s := newStore(t)
	if _, err := Open(s.dir, "environment", "session", "another"); err == nil {
		t.Fatal("second writer admitted")
	}
	if err := os.Mkdir(filepath.Join(s.dir, "session.json.block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.dir, "session.json"), filepath.Join(s.dir, "previous.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.dir, "session.json.block"), filepath.Join(s.dir, "session.json")); err != nil {
		t.Fatal(err)
	}
	_, dispatch, err := s.Begin(Task{OperationID: "task", Incarnation: "actor-one", Text: "edit"})
	if err == nil || dispatch {
		t.Fatal("failed journal allowed dispatch")
	}
	if _, dispatch, err := s.Begin(Task{OperationID: "other", Incarnation: "actor-one", Text: "edit"}); err == nil || dispatch {
		t.Fatal("failed persistence did not hold")
	}
}

func TestWrongThreadAndMalformedEventsDoNotChangeEvidence(t *testing.T) {
	s := newStore(t)
	s.Begin(Task{OperationID: "task", Incarnation: "actor-one", Text: "edit"})
	s.Thread("actor-one", "thread")
	s.Accepted("actor-one", "task", "turn")
	for _, params := range []string{
		`{"threadId":"foreign","turn":{"id":"turn","status":"completed"}}`,
		`{"turn":{"id":"turn","status":"completed"}}`,
		`{"threadId":"thread","turn":{"id":"turn","status":"imagined"}}`,
		`not-json`,
	} {
		if err := s.Observe("actor-one", "turn/completed", nil, json.RawMessage(params)); err == nil {
			t.Fatal("invalid event accepted")
		}
		if state := s.Snapshot(); state.Operations["task"].Completion != "" || len(state.Events) != 0 {
			t.Fatal("invalid event changed retained evidence")
		}
	}
}
