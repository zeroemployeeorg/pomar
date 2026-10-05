package agentenv

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestContinuationHTTPPersistsBeforeDispatchAndRetriesOnlyInspect(t *testing.T) {
	s, binding, original := continuationFixture(t)
	defer s.Close()
	workspace := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "fixture"}} {
		if output, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	broker := NewBroker(s, workspace)
	actor := actorFixture()
	broker.Actor = actor
	broker.resumeFailure = &RPCError{Code: -32600}
	inventory, err := broker.inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.s.SourceSHA = inventory.Head
	encoded, _ := json.Marshal(inventory)
	binding.Request.WorkspaceInventorySHA256 = digest(encoded)
	encoded, _ = json.Marshal(binding.Request)
	binding.RequestSHA256 = digest(encoded)
	body, _ := json.Marshal(binding)
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		broker.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/continuations", strings.NewReader(string(body))))
		return w
	}
	if w := call(); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if actor.count("thread/start") != 0 || actor.count("turn/start") != 0 {
		t.Fatal("binding dispatched actor")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored State
	if json.Unmarshal(data, &stored) != nil || stored.Continuations[binding.Request.DispositionID].SuccessorInputSHA256 == "" {
		t.Fatal("successor not durable")
	}
	waiting := httptest.NewRecorder()
	broker.Handler().ServeHTTP(waiting, httptest.NewRequest("GET", "/v1/result", nil))
	if waiting.Code != 409 {
		t.Fatal("bound but unsubmitted successor exported a result", waiting.Code, waiting.Body.String())
	}
	if _, err = broker.Submit(binding.Request.Successor); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(workspace, "retained-result.txt"), []byte("new work"), 0600)
	if w := call(); w.Code != 200 {
		t.Fatal("duplicate consumption reinvestigated changed workspace", w.Body.String())
	}
	if actor.count("turn/start") != 1 || s.Snapshot().Operations["original"] != original {
		t.Fatal("replay or original rewrite")
	}
	{
		w := httptest.NewRecorder()
		broker.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/result", nil))
		if w.Code != 409 {
			t.Fatal("unfinished successor export admitted", w.Code)
		}
	}
	if err = s.Observe("actor-two", "turn/completed", nil, json.RawMessage(`{"threadId":"thread-one","turn":{"id":"turn-one","status":"completed"}}`)); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	broker.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/result", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "new work") {
		t.Fatal("finished successor result blocked by closed original", w.Body.String())
	}
}

func TestContinuationHTTPDuplicateRefusesUncertainJournalAndReopensDisk(t *testing.T) {
	s, binding, original := continuationFixture(t)
	defer s.Close()
	workspace := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "fixture"}} {
		if output, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	b := NewBroker(s, workspace)
	actor := actorFixture()
	b.Actor = actor
	inv, err := b.inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.s.SourceSHA = inv.Head
	if err = s.save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(inv)
	binding.Request.WorkspaceInventorySHA256 = digest(raw)
	raw, _ = json.Marshal(binding.Request)
	binding.RequestSHA256 = digest(raw)
	body, _ := json.Marshal(binding)
	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/continuations", strings.NewReader(string(body))))
		return w
	}
	dir := s.dir
	before, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the original durable journal while forcing CreateTemp to fail.
	s.dir = filepath.Join(dir, "missing-parent")
	if w := call(); w.Code != 409 {
		t.Fatal("initial failed save accepted", w.Code)
	}
	if w := call(); w.Code != 409 || !strings.Contains(w.Body.String(), "journal write uncertain") {
		t.Fatal("duplicate falsely claimed durability", w.Code, w.Body.String())
	}
	if _, err = s.bindContinuation(binding); err == nil || !strings.Contains(err.Error(), "journal write uncertain") {
		t.Fatal("store duplicate falsely claimed durability", err)
	}
	if _, dispatch, err := s.Begin(binding.Request.Successor); err == nil || dispatch {
		t.Fatal("uncertain journal dispatched", err, dispatch)
	}
	if actor.count("thread/start") != 0 || actor.count("turn/start") != 0 {
		t.Fatal("actor invoked")
	}
	result := httptest.NewRecorder()
	b.Handler().ServeHTTP(result, httptest.NewRequest("GET", "/v1/result", nil))
	if result.Code != 409 {
		t.Fatal("uncertain journal exported result", result.Code)
	}
	after, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("failed save changed durable bytes", err)
	}
	s.dir = dir
	s.Close()
	reopened, err := Open(dir, "environment", "session", "actor-three")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.Snapshot()
	if len(state.Continuations) != 0 || state.Operations["original"] != original || state.ThreadID != "original-thread" {
		t.Fatal("reopen invented failed binding", state)
	}
	if _, dispatch, err := reopened.Begin(Task{OperationID: "successor", Incarnation: "actor-three", Text: "same work"}); err != ErrBusy || dispatch {
		t.Fatal("reopened unresolved original lost its hold", err, dispatch)
	}
}

func TestContinuationHostRejectsForgedAgentRouteAndMissingTransition(t *testing.T) {
	h := &Host{envs: map[string]*Environment{"environment": {Spec: VMSpec{Environment: "environment", Session: "session", Incarnation: "actor-two"}, Phase: "running", Actions: map[string]Action{}}}}
	store, binding, _ := continuationFixture(t)
	defer store.Close()
	body, _ := json.Marshal(binding.Request)
	for _, tc := range []struct {
		Path string
		Code int
	}{{"/v1/environments/environment/agent/continuations", 403}, {"/v1/environments/environment/continuations", 409}} {
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, httptest.NewRequest("POST", tc.Path, strings.NewReader(string(body))))
		if w.Code != tc.Code {
			t.Fatal(tc.Path, w.Code, w.Body.String())
		}
	}
}

func continuationFixture(t *testing.T) (*Store, ContinuationBinding, Operation) {
	t.Helper()
	s := newStore(t)
	if err := s.BindSource(strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	task := Task{OperationID: "original", Incarnation: "actor-one", Text: "old intent"}
	s.Begin(task)
	s.Thread("actor-one", "original-thread")
	s.Accepted("actor-one", "original", "original-turn")
	s.Unknown("actor-one", "original")
	original := s.Snapshot().Operations["original"]
	s.Close()
	next, err := Open(s.dir, "environment", "session", "actor-two")
	if err != nil {
		t.Fatal(err)
	}
	original = next.Snapshot().Operations["original"]
	r := ContinuationRequest{DispositionID: "rt-disposition", IntentRef: "rt-intent", AuthorityRef: "council-disposition", ResponsibleOwner: "rt-owner", EnvironmentID: "environment", WorkspaceID: "workspace-environment", SessionID: "session", OriginalOperation: "original", OriginalIncarnation: "actor-one", OriginalInputSHA256: original.InputHash, OriginalThreadID: "original-thread", OriginalTurnID: "original-turn", OriginalFenceOperation: "original-fence", PredecessorIncarnation: "inspection-actor", PredecessorFenceOperation: "inspection-fence", LaunchOperation: "new-launch", RecoveryDescription: "repaired candidate of preserved damaged disk", ExternalActionInventory: "no scoped external action evidenced", Successor: Task{OperationID: "successor", Incarnation: "actor-two", Text: "complete remaining bounded work"}}
	fence := func(inc, op string) Reconciliation {
		return Reconciliation{Request: ReconcileRequest{SessionID: "session", Incarnation: inc, OperationID: op}, ReplacementEligible: true, CurrentScope: "fenced_now", EvidenceSHA256: strings.Repeat("b", 64)}
	}
	encoded, _ := json.Marshal(r)
	b := ContinuationBinding{Request: r, RequestSHA256: digest(encoded), OriginalFence: fence("actor-one", "original-fence"), PredecessorFence: fence("inspection-actor", "inspection-fence"), Inventory: WorkspaceInventory{Head: strings.Repeat("a", 40), DiffSHA256: digest(nil)}}
	return next, b, original
}

func TestContinuationPreservesUnknownAndDispatchesOnlyBoundSuccessorOnce(t *testing.T) {
	s, binding, original := continuationFixture(t)
	defer s.Close()
	bound, err := s.bindContinuation(binding)
	if err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Operations["original"] != original || s.Snapshot().ThreadID != "" || bound.Decision != "closed_unresolved" {
		t.Fatal("original rewritten or thread retained", s.Snapshot())
	}
	actor := actorFixture()
	broker := NewBroker(s, t.TempDir())
	broker.Actor = actor
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := broker.Submit(binding.Request.Successor); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if actor.count("thread/start") != 1 || actor.count("turn/start") != 1 {
		t.Fatal("successor replay", actor.calls)
	}
	if s.Snapshot().Continuations["rt-disposition"].SuccessorThreadID != "thread-one" {
		t.Fatal("successor thread not durably linked")
	}
	if s.Snapshot().Operations["original"] != original {
		t.Fatal("old result changed")
	}
	if _, err = s.bindContinuation(binding); err != nil {
		t.Fatal("duplicate disposition did not inspect existing", err)
	}
	changed := binding
	changed.Request.Successor.Text = "changed input"
	raw, _ := json.Marshal(changed.Request)
	changed.RequestSHA256 = digest(raw)
	if _, err = s.bindContinuation(changed); err != ErrConflict {
		t.Fatal("changed disposition accepted", err)
	}
	if _, _, err = s.Begin(Task{OperationID: "unrelated", Incarnation: "actor-two", Text: "another task"}); err != ErrBusy {
		t.Fatal("unrelated task bypassed unresolved successor", err)
	}
	s.Close()
	next, err := Open(s.dir, "environment", "session", "actor-three")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Snapshot().Continuations["rt-disposition"].Successor.OperationID != "successor" || next.Snapshot().Operations["original"] != original {
		t.Fatal("reopen discarded binding/unknown")
	}
	if _, _, err = next.Begin(Task{OperationID: "other", Incarnation: "actor-three", Text: "retry"}); err != ErrBusy {
		t.Fatal("replacement replay admitted", err)
	}
}

func TestContinuationRejectsWrongScopeInventoryAndDuplicateConsumption(t *testing.T) {
	for _, mode := range []string{"wrong session", "wrong old hash", "wrong turn", "wrong thread", "missing original fence", "wrong inspection fence", "dirty workspace", "wrong head", "new disposition same intent", "reserved successor changed input", "journal write failure"} {
		t.Run(mode, func(t *testing.T) {
			s, b, original := continuationFixture(t)
			defer s.Close()
			switch mode {
			case "wrong session":
				b.Request.SessionID = "other"
			case "wrong old hash":
				b.Request.OriginalInputSHA256 = strings.Repeat("c", 64)
			case "wrong turn":
				b.Request.OriginalTurnID = "other"
			case "wrong thread":
				b.Request.OriginalThreadID = "other"
			case "missing original fence":
				b.OriginalFence.ReplacementEligible = false
			case "wrong inspection fence":
				b.PredecessorFence.Request.Incarnation = "old-inspection"
			case "dirty workspace":
				b.Inventory.Status = " M file.go\n"
			case "wrong head":
				b.Inventory.Head = strings.Repeat("c", 40)
			case "new disposition same intent":
				if _, err := s.bindContinuation(b); err != nil {
					t.Fatal(err)
				}
				b.Request.DispositionID = "other-disposition"
			case "reserved successor changed input":
				if _, err := s.bindContinuation(b); err != nil {
					t.Fatal(err)
				}
				task := b.Request.Successor
				task.Text = "changed"
				if _, _, err := s.Begin(task); err != ErrConflict {
					t.Fatal(err)
				}
				return
			case "journal write failure":
				os.Remove(filepath.Join(s.dir, "session.json"))
				os.Mkdir(filepath.Join(s.dir, "session.json"), 0700)
			}
			if _, err := s.bindContinuation(b); err == nil {
				t.Fatal("invalid continuation accepted")
			}
			if s.Snapshot().Operations["original"] != original {
				t.Fatal("old operation changed")
			}
			if mode == "journal write failure" {
				if _, _, err := s.Begin(b.Request.Successor); err == nil {
					t.Fatal("uncertain journal permitted task")
				}
			}
		})
	}
}
