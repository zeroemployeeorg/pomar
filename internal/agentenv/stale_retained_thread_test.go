package agentenv

import (
	"encoding/json"
	"io"
	"reflect"
	"testing"
	"time"
)

// Retaining a thread must not let a delayed old turn acknowledge or complete a
// new submission whose turn/start reply has not yet arrived.
func TestOldAdapterCannotReportIntoRetainedThread(t *testing.T) {
	for _, method := range []string{"turn/started", "turn/completed"} {
		t.Run(method, func(t *testing.T) {
			old := newStore(t)
			if _, _, err := old.Begin(Task{OperationID: "previous", Incarnation: "actor-one", Text: "previous work"}); err != nil {
				t.Fatal(err)
			}
			if err := old.ObserveModel("actor-one", "previous", "thread/start", json.RawMessage(`{"thread":{"id":"retained-thread"}}`)); err != nil {
				t.Fatal(err)
			}
			completed := json.RawMessage(`{"threadId":"retained-thread","turn":{"id":"previous-turn","status":"completed"}}`)
			if err := old.Observe("actor-one", "turn/completed", nil, completed); err != nil {
				t.Fatal(err)
			}
			oldBroker := NewBroker(old, t.TempDir())
			reader, provider := io.Pipe()
			defer reader.Close()
			defer provider.Close()
			rpc := NewRPC(reader, io.Discard, oldBroker.Observe)
			old.Close()
			next, err := Open(old.dir, "environment", "session", "actor-two")
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if _, dispatch, err := next.Begin(Task{OperationID: "current", Incarnation: "actor-two", Text: "new work"}); err != nil || !dispatch {
				t.Fatal(err, dispatch)
			}
			before := next.Snapshot()
			if before.ThreadID != "retained-thread" || before.Operations["current"].TurnID != "" {
				t.Fatal("fixture did not hold vulnerable interval")
			}
			oldBroker.Store = next
			if err := json.NewEncoder(provider).Encode(Message{Method: method, Params: completed}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-rpc.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("old reader was not refused")
			}
			rpc.mu.Lock()
			refusal := rpc.err
			rpc.mu.Unlock()
			if refusal != ErrStale {
				t.Fatal(refusal)
			}
			after := next.Snapshot()
			if len(after.RejectedEvents) != 1 || after.RejectedEvents[0].OperationID != "previous" {
				t.Fatal("missing old operation provenance")
			}
			t.Logf("same retained thread; new turn still unassigned; old %s rejected with %+v", method, after.RejectedEvents[0])
			after.RejectedEvents = nil
			if !reflect.DeepEqual(before, after) {
				t.Fatal("old event manufactured acknowledgement or completion")
			}
		})
	}
}
