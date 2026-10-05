package agentenv

import (
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This is a controlled adapter fixture, not a live provider/VM fault campaign.
// Queue an otherwise valid old notification before its reader can consume it;
// after the fixture's confirmed scope transition, route it to the current
// retained journal through the production RPC reader and Broker.Observe.
func TestDelayedOldAdapterNotificationCannotChangeReplacement(t *testing.T) {
	for _, method := range []string{"turn/started", "turn/completed", "item/commandExecution/requestApproval"} {
		t.Run(method, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "journal")
			old, err := Open(dir, "environment", "session", "actor-one")
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			if err = old.BindSource(strings.Repeat("a", 40)); err != nil {
				t.Fatal(err)
			}
			if _, _, err = old.Begin(Task{OperationID: "original", Incarnation: "actor-one", Text: "old intent"}); err != nil {
				t.Fatal(err)
			}
			if err = old.ObserveModel("actor-one", "original", "thread/start", json.RawMessage(`{"thread":{"id":"original-thread"}}`)); err != nil {
				t.Fatal(err)
			}
			if err = old.Accepted("actor-one", "original", "original-turn"); err != nil {
				t.Fatal(err)
			}
			oldBroker := NewBroker(old, t.TempDir())
			reader, provider := io.Pipe()
			defer reader.Close()
			defer provider.Close()
			rpc := NewRPC(reader, io.Discard, oldBroker.Observe)
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			message := Message{Method: method, Params: json.RawMessage(`{"threadId":"original-thread","turnId":"original-turn","turn":{"id":"original-turn","status":"completed"},"private":"fixture-content-not-to-export"}`)}
			if strings.HasPrefix(method, "item/") {
				message.ID = json.RawMessage(`"old-permission"`)
			}
			written := make(chan error, 1)
			go func() { <-release; written <- json.NewEncoder(provider).Encode(message) }()

			old.Close() // Fixture's original execution scope is released before replacement.
			next, err := Open(dir, "environment", "session", "actor-two")
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			original := next.Snapshot().Operations["original"]
			r := ContinuationRequest{DispositionID: "controller-disposition", IntentRef: "controller-intent", AuthorityRef: "controller-decision", ResponsibleOwner: "controller-owner", EnvironmentID: "environment", WorkspaceID: "workspace-environment", SessionID: "session", OriginalOperation: "original", OriginalIncarnation: "actor-one", OriginalInputSHA256: original.InputHash, OriginalThreadID: "original-thread", OriginalTurnID: "original-turn", OriginalFenceOperation: "original-fence", PredecessorIncarnation: "inspection-actor", PredecessorFenceOperation: "inspection-fence", LaunchOperation: "new-launch", RecoveryDescription: "preserved inspected workspace", ExternalActionInventory: "no scoped external action", Successor: Task{OperationID: "successor", Incarnation: "actor-two", Text: "distinct remaining work"}}
			fence := func(inc, op string) Reconciliation {
				return Reconciliation{Request: ReconcileRequest{SessionID: "session", Incarnation: inc, OperationID: op}, ReplacementEligible: true, CurrentScope: "fenced_now", EvidenceSHA256: strings.Repeat("b", 64)}
			}
			raw, _ := json.Marshal(r)
			binding := ContinuationBinding{Request: r, RequestSHA256: digest(raw), OriginalFence: fence("actor-one", "original-fence"), PredecessorFence: fence("inspection-actor", "inspection-fence"), Inventory: WorkspaceInventory{Head: strings.Repeat("a", 40), DiffSHA256: digest(nil)}}
			if _, err = next.bindContinuation(binding); err != nil {
				t.Fatal(err)
			}
			if _, dispatch, err := next.Begin(r.Successor); err != nil || !dispatch {
				t.Fatal(err, dispatch)
			}
			if err = next.ObserveModel("actor-two", "successor", "thread/start", json.RawMessage(`{"thread":{"id":"successor-thread"}}`)); err != nil {
				t.Fatal(err)
			}
			if err = next.Accepted("actor-two", "successor", "successor-turn"); err != nil {
				t.Fatal(err)
			}
			// Stress the ingestion boundary: a retained old reader reaches the
			// replacement journal. Its source scope must not be relabelled.
			oldBroker.Store = next
			before := next.Snapshot()
			close(release)
			select {
			case err := <-written:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("provider write blocked")
			}
			select {
			case <-rpc.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("stale adapter was not refused")
			}
			rpc.mu.Lock()
			rejection := rpc.err
			rpc.mu.Unlock()
			if rejection != ErrStale {
				t.Fatal("wrong ingestion refusal", rejection)
			}
			after := next.Snapshot()
			if len(after.RejectedEvents) != 1 {
				t.Fatal("rejected provenance absent", after.RejectedEvents)
			}
			provenance := after.RejectedEvents[0]
			if provenance.Incarnation != "actor-one" || provenance.CurrentIncarnation != "actor-two" || provenance.OperationID != "original" || provenance.ThreadID != "original-thread" || provenance.TurnID != "original-turn" || provenance.Method != method || provenance.ParamsSHA256 != digest(message.Params) {
				t.Fatal("wrong provenance", provenance)
			}
			after.RejectedEvents = nil
			if !reflect.DeepEqual(before, after) || len(oldBroker.pending) != 0 {
				t.Fatal("old event changed accepted state, binding or permissions")
			}
			raw, _ = json.Marshal(next.Snapshot())
			if strings.Contains(string(raw), "fixture-content-not-to-export") {
				t.Fatal("raw rejected content exported")
			}
			t.Logf("production RPC ingestion rejected: %+v; original/successor/binding unchanged", provenance)
			next.Close()
			reopened, err := Open(dir, "environment", "session", "actor-three")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if len(reopened.Snapshot().RejectedEvents) != 1 || reopened.Snapshot().RejectedEvents[0] != provenance {
				t.Fatal("rejection diagnosis not durable")
			}
		})
	}
}
