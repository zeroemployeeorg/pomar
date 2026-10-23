package agentenv

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// POMAR-CC-SOW-37: this is the TARGET corrected sequence for
// TestReplaceForecloseReconciliationOfTheIncarnationItSuperseded
// (replace_continuation_gap_test.go, kept unmodified -- it documents
// today's real failure and must keep failing the same way until the
// production fix lands). This file does not implement that fix (host.go's
// stop()/replace are POMAR's shared-lifecycle code to change); it proves,
// using the REAL, unmodified continuation.go code, that the sequence the
// fix is meant to produce -- stop/replace retains a Reconciliation ->
// that retained evidence alone (no later live /reconcile call) is
// sufficient for a real continuation bind -- actually works end to end
// once that one missing connection exists. It is the acceptance target,
// not a demonstration that the connection already exists.
//
// Confirmed directly before writing this: bindContinuation's own
// validation never references ContinuationRequest.LaunchOperation at all
// -- launch completion is checked only by the HOST's continuationHandler
// (continuation.go, Host side), exactly the code path POMAR's fix touches.
// The broker's job, proven here with the same fixture shape as
// continuationFixture (continuation_test.go), is to trust whatever
// Reconciliation receipt it's handed and independently re-verify
// identity/payload/scope -- which it already does correctly; what's
// missing today is only the host ever producing a usable one for a
// superseded incarnation.
func correctedPredecessorReconciliation(incarnation, fenceOperation, sessionID string) Reconciliation {
	return Reconciliation{
		Request:             ReconcileRequest{SessionID: sessionID, Incarnation: incarnation, OperationID: fenceOperation},
		ReplacementEligible: true,
		CurrentScope:        "fenced_now",
		EvidenceSHA256:      strings.Repeat("c", 64),
	}
}

// heldOperationFixture reproduces the shape of the real held operation from
// this session's live exercise (adapter-restart-exercise-1, environment
// cc-pr143-qual): an operation accepted under a predecessor incarnation,
// left acceptance_unknown by a replace, with no real Reconciliation ever
// captured for it -- see Open()'s unconditional "op.Completion == empty ->
// acceptance_unknown" transition on incarnation change (state.go), which is
// what actually produced this state live, not an explicit Unknown() call.
func heldOperationFixture(t *testing.T) (*Store, ContinuationRequest, Operation) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "journal"), "environment", "session", "actor1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindSource(strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	task := Task{OperationID: "adapter-restart-exercise-1", Incarnation: "actor1", Text: "sleep 25; echo DONE_WAITING"}
	if _, _, err := s.Begin(task); err != nil {
		t.Fatal(err)
	}
	if err := s.Thread("actor1", "original-thread"); err != nil {
		t.Fatal(err)
	}
	if err := s.Accepted("actor1", "adapter-restart-exercise-1", "original-turn"); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	next, err := Open(dir, "environment", "session", "actor2")
	if err != nil {
		t.Fatal(err)
	}
	original := next.Snapshot().Operations["adapter-restart-exercise-1"]
	if original.State != "acceptance_unknown" {
		t.Fatalf("fixture must reproduce the real held state: got %q", original.State)
	}
	r := ContinuationRequest{
		DispositionID: "pcc-recovery-target", IntentRef: "restart-leg-recovery", AuthorityRef: "council-disposition",
		ResponsibleOwner: "pomar-cc", EnvironmentID: "environment", WorkspaceID: "workspace-environment", SessionID: "session",
		OriginalOperation: "adapter-restart-exercise-1", OriginalIncarnation: "actor1", OriginalInputSHA256: original.InputHash,
		OriginalThreadID: "original-thread", OriginalTurnID: "original-turn",
		OriginalFenceOperation: "replace-mid-flight-interrupt", PredecessorIncarnation: "actor1", PredecessorFenceOperation: "replace-mid-flight-interrupt",
		LaunchOperation: "start-restart-leg", RecoveryDescription: "resume remaining bounded work after a mid-flight replace",
		ExternalActionInventory: "sleep command's own external effect, if any, not independently observed; recovery is bounded to the conversation/assignment layer only",
		Successor:               Task{OperationID: "adapter-restart-exercise-1-successor", Incarnation: "actor2", Text: "report whatever the interrupted sleep command's retained output shows, if any; otherwise state plainly that none is available"},
	}
	return next, r, original
}

func bindingFor(r ContinuationRequest, fence Reconciliation) ContinuationBinding {
	encoded, _ := json.Marshal(r)
	return ContinuationBinding{
		Request: r, RequestSHA256: digest(encoded),
		OriginalFence: fence, PredecessorFence: fence,
		Inventory: WorkspaceInventory{Head: strings.Repeat("a", 40), DiffSHA256: digest(nil)},
	}
}

// Target: once stop()/replace retain a Reconciliation like the one above
// (synchronously, at the only moment it is trustworthy), the SAME
// operation left acceptance_unknown by TestReplaceForeclose... can reach a
// real, supported resolution through the existing, unmodified broker
// continuation path -- no new continuation mechanism, no relaxed identity
// checks, just the missing evidence connection -- and dispatches the bound
// successor turn exactly once, never a silent replay of the original.
func TestCorrectedSequenceRetainedEvidenceResolvesTheHeldOperation(t *testing.T) {
	s, r, original := heldOperationFixture(t)
	defer s.Close()

	fence := correctedPredecessorReconciliation("actor1", "replace-mid-flight-interrupt", "session")
	binding := bindingFor(r, fence)

	bound, err := s.bindContinuation(binding)
	if err != nil {
		t.Fatalf("the corrected sequence's continuation bind must succeed once retained evidence exists: %v", err)
	}
	if bound.Decision != "closed_unresolved" {
		t.Fatal("expected the original to close as unresolved, not rewritten", bound)
	}
	if s.Snapshot().Operations["adapter-restart-exercise-1"] != original {
		t.Fatal("the original operation's retained history must be byte-for-byte unchanged, never rewritten", s.Snapshot().Operations["adapter-restart-exercise-1"], original)
	}

	actor := actorFixture()
	broker := NewBroker(s, t.TempDir())
	broker.Actor = actor
	if _, err := broker.Submit(r.Successor); err != nil {
		t.Fatal(err)
	}
	if actor.count("turn/start") != 1 {
		t.Fatalf("exactly one new successor turn must dispatch, never a silent replay of the original: turn/start called %d times", actor.count("turn/start"))
	}
}

// Complementary, and just as important: an incomplete probe result -- my
// real "vnode lookup incomplete" / replacement_eligible:false finding from
// the live guest -- must still correctly block the continuation even once
// evidence retention exists. "No running process observed" alone must
// never read as ReplacementEligible, and a Reconciliation that says so
// honestly (CurrentScope "unknown", ReplacementEligible false) must be
// refused by the SAME unchanged broker validation that accepts a clean one
// above. This is the real custody uncertainty the corrected design must
// never paper over.
func TestCorrectedSequenceStillRefusesAnIncompleteProbeResult(t *testing.T) {
	s, r, original := heldOperationFixture(t)
	defer s.Close()

	incomplete := Reconciliation{
		Request:             ReconcileRequest{SessionID: "session", Incarnation: "actor1", OperationID: "replace-mid-flight-interrupt"},
		CurrentScope:        "unknown",
		ReplacementEligible: false,
		Issues:              []string{"pid 3188 fd 4: vnode lookup incomplete"},
	}
	binding := bindingFor(r, incomplete)

	if _, err := s.bindContinuation(binding); err == nil {
		t.Fatal("an incomplete/uncertain probe result must never be accepted as sufficient evidence for continuation")
	}
	if s.Snapshot().Operations["adapter-restart-exercise-1"] != original {
		t.Fatal("a refused continuation attempt must never change the original operation", s.Snapshot().Operations["adapter-restart-exercise-1"], original)
	}
}
