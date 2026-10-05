package agentenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func postUnix(ctx context.Context, socket, path string, v any) ([]byte, int, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, 0, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://guest"+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, (256<<10)+1))
	if len(b) > 256<<10 {
		return nil, r.StatusCode, errors.New("continuation response exceeds bound")
	}
	return b, r.StatusCode, err
}

// This is a projection of an external controller's intent disposition, not an approval
// authority. The owner authenticates through the private development socket.
type ContinuationRequest struct {
	DispositionID             string `json:"disposition_id"`
	IntentRef                 string `json:"intent_ref"`
	AuthorityRef              string `json:"authority_ref"`
	ResponsibleOwner          string `json:"responsible_owner"`
	EnvironmentID             string `json:"environment_id"`
	WorkspaceID               string `json:"workspace_id"`
	SessionID                 string `json:"session_id"`
	OriginalOperation         string `json:"original_operation"`
	OriginalIncarnation       string `json:"original_incarnation"`
	OriginalInputSHA256       string `json:"original_input_sha256"`
	OriginalThreadID          string `json:"original_thread_id"`
	OriginalTurnID            string `json:"original_turn_id"`
	OriginalFenceOperation    string `json:"original_fence_operation"`
	PredecessorIncarnation    string `json:"predecessor_incarnation"`
	PredecessorFenceOperation string `json:"predecessor_fence_operation"`
	LaunchOperation           string `json:"launch_operation"`
	WorkspaceInventorySHA256  string `json:"workspace_inventory_sha256"`
	RecoveryDescription       string `json:"recovery_description"`
	ExternalActionInventory   string `json:"external_action_inventory"`
	Successor                 Task   `json:"successor"`
}

type ContinuationBinding struct {
	Request              ContinuationRequest `json:"request"`
	RequestSHA256        string              `json:"request_sha256"`
	Decision             string              `json:"decision"`
	Original             Operation           `json:"original"`
	OriginalFence        Reconciliation      `json:"original_fence"`
	PredecessorFence     Reconciliation      `json:"predecessor_fence"`
	Inventory            WorkspaceInventory  `json:"inventory"`
	Successor            Task                `json:"successor"`
	SuccessorInputSHA256 string              `json:"successor_input_sha256"`
	SuccessorThreadID    string              `json:"successor_thread_id,omitempty"`
	BoundAt              string              `json:"bound_at"`
}

type WorkspaceInventory struct {
	Head       string `json:"head"`
	Status     string `json:"status"`
	DiffSHA256 string `json:"diff_sha256"`
}

func (b *Broker) inventory(ctx context.Context) (WorkspaceInventory, error) {
	var v WorkspaceInventory
	head, err := b.gitOutput(ctx, "rev-parse", "HEAD")
	if err != nil {
		return v, err
	}
	status, err := b.gitOutput(ctx, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return v, err
	}
	diff, err := b.gitOutput(ctx, "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD")
	if err != nil {
		return v, err
	}
	v = WorkspaceInventory{Head: strings.TrimSpace(string(head)), Status: string(status), DiffSHA256: digest(diff)}
	return v, nil
}

func (m *Store) closedUnresolved(id string) bool {
	for _, binding := range m.s.Continuations {
		if binding.Original.ID == id && binding.Decision == "closed_unresolved" {
			return true
		}
	}
	return false
}

func (m *Store) continuedBy(original, successor string) bool {
	for _, binding := range m.s.Continuations {
		if binding.Original.ID == original && binding.Decision == "closed_unresolved" {
			return binding.Successor.OperationID == successor || m.s.Operations[binding.Successor.OperationID].Completion != ""
		}
	}
	return false
}

func (m *Store) bindContinuation(binding ContinuationBinding) (ContinuationBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := binding.Request
	if old, ok, err := m.continuationLocked(r.DispositionID, binding.RequestSHA256); err != nil || ok {
		return old, err
	}
	if err := m.check(r.Successor.Incarnation); err != nil {
		return binding, err
	}
	if !validID.MatchString(r.DispositionID) || !validID.MatchString(r.Successor.OperationID) || r.IntentRef == "" || r.AuthorityRef == "" || r.ResponsibleOwner == "" || len(r.Successor.Text) == 0 || len(r.Successor.Text) > 64<<10 || r.RecoveryDescription == "" || r.ExternalActionInventory == "" {
		return binding, errors.New("continuation lacks intent/disposition binding")
	}
	if r.EnvironmentID != m.s.EnvironmentID || r.WorkspaceID != m.s.WorkspaceID || r.SessionID != m.s.SessionID || r.Successor.OperationID == r.OriginalOperation || r.Successor.Incarnation == r.OriginalIncarnation || r.Successor.Incarnation == r.PredecessorIncarnation {
		return binding, ErrConflict
	}
	original, ok := m.s.Operations[r.OriginalOperation]
	if !ok || original.Incarnation != r.OriginalIncarnation || original.InputHash != r.OriginalInputSHA256 || original.TurnID != r.OriginalTurnID || original.Completion != "" || original.State != "acceptance_unknown" || r.OriginalThreadID != m.s.ThreadID || (original.ThreadID != "" && original.ThreadID != r.OriginalThreadID) {
		return binding, errors.New("original unresolved identity differs")
	}
	for _, old := range m.s.Continuations {
		if old.Original.ID == original.ID || old.Successor.OperationID == r.Successor.OperationID {
			return binding, ErrConflict
		}
	}
	for _, op := range m.s.Operations {
		if op.ID == r.Successor.OperationID || (op.ID != original.ID && op.Completion == "" && !m.closedUnresolved(op.ID)) {
			return binding, ErrBusy
		}
	}
	if _, ok := m.s.Controls[r.Successor.OperationID]; ok {
		return binding, ErrConflict
	}
	for _, f := range []struct {
		Receipt                Reconciliation
		Incarnation, Operation string
	}{{binding.OriginalFence, r.OriginalIncarnation, r.OriginalFenceOperation}, {binding.PredecessorFence, r.PredecessorIncarnation, r.PredecessorFenceOperation}} {
		if f.Receipt.Request.Incarnation != f.Incarnation || f.Receipt.Request.OperationID != f.Operation || f.Receipt.Request.SessionID != m.s.SessionID || !f.Receipt.ReplacementEligible || f.Receipt.CurrentScope != "fenced_now" || len(f.Receipt.EvidenceSHA256) != 64 {
			return binding, errors.New("continuation lacks confirmed scope evidence")
		}
	}
	if binding.Inventory.Head != m.s.SourceSHA || binding.Inventory.Status != "" || binding.Inventory.DiffSHA256 != digest(nil) {
		return binding, errors.New("this bounded continuation requires inspected unchanged baseline; retain unexpected work")
	}
	binding.Original = original
	binding.Decision = "closed_unresolved"
	binding.BoundAt = time.Now().UTC().Format(time.RFC3339Nano)
	binding.Successor = r.Successor
	task, _ := json.Marshal(r.Successor)
	binding.SuccessorInputSHA256 = digest(task)
	if m.s.Continuations == nil {
		m.s.Continuations = map[string]ContinuationBinding{}
	}
	m.s.Continuations[r.DispositionID] = binding
	// Append preserves the original thread in the request/original binding.
	// Clearing only the selected active thread permits one explicitly new thread.
	m.s.ThreadID = ""
	m.s.ModelObservation = ModelObservation{Status: "unknown"}
	return binding, m.save()
}

// Lookup and journal health share the store lock. A failed write may leave an
// in-memory binding, which must never become a successful durability receipt.
func (m *Store) continuation(disposition, hash string) (ContinuationBinding, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.continuationLocked(disposition, hash)
}

func (m *Store) continuationLocked(disposition, hash string) (ContinuationBinding, bool, error) {
	if err := m.check(m.s.Incarnation); err != nil {
		return ContinuationBinding{}, false, err
	}
	old, ok := m.s.Continuations[disposition]
	if ok && old.RequestSHA256 != hash {
		return old, true, ErrConflict
	}
	return old, ok, nil
}

// ResultSnapshot also checks reserved successors that do not yet have an
// Operations entry. Inspection remains available independently of results.
func (m *Store) ResultSnapshot() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(m.s.Incarnation); err != nil {
		return State{}, err
	}
	for _, binding := range m.s.Continuations {
		op, ok := m.s.Operations[binding.Successor.OperationID]
		if !ok || op.Completion == "" {
			return State{}, ErrBusy
		}
		if op.ID != binding.Successor.OperationID || op.Incarnation != binding.Successor.Incarnation || op.InputHash != binding.SuccessorInputSHA256 {
			return State{}, ErrConflict
		}
	}
	for _, op := range m.s.Operations {
		if op.Completion == "" && !m.closedUnresolved(op.ID) {
			return State{}, ErrBusy
		}
	}
	return m.snapshotLocked(), nil
}

func (b *Broker) continuationHandler(w http.ResponseWriter, r *http.Request) {
	var binding ContinuationBinding
	if !decode(w, r, &binding) {
		return
	}
	encoded, _ := json.Marshal(binding.Request)
	if digest(encoded) != binding.RequestSHA256 {
		failure(w, ErrConflict)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if old, ok, err := b.Store.continuation(binding.Request.DispositionID, binding.RequestSHA256); err != nil {
		failure(w, err)
		return
	} else if ok {
		respond(w, 200, old)
		return
	}
	inventory, err := b.inventory(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	encoded, _ = json.Marshal(inventory)
	if digest(encoded) != binding.Request.WorkspaceInventorySHA256 {
		failure(w, errors.New("workspace inventory changed"))
		return
	}
	binding.Inventory = inventory
	bound, err := b.Store.bindContinuation(binding)
	if err != nil {
		failure(w, err)
		return
	}
	b.resumeFailure = nil
	respond(w, 200, bound)
}

func (h *Host) continuationHandler(w http.ResponseWriter, r *http.Request) {
	var req ContinuationRequest
	if !decode(w, r, &req) {
		return
	}
	h.mu.Lock()
	e := h.envs[r.PathValue("id")]
	h.mu.Unlock()
	if e == nil {
		respond(w, 404, map[string]string{"error": "unknown environment"})
		return
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	h.mu.Lock()
	if h.fatal != nil {
		h.mu.Unlock()
		failure(w, errors.New("owner journal durability uncertain"))
		return
	}
	if e.Phase != "running" || req.Successor.Incarnation != e.Spec.Incarnation || req.EnvironmentID != e.Spec.Environment || req.SessionID != e.Spec.Session {
		h.mu.Unlock()
		failure(w, ErrStale)
		return
	}
	original, originalOK := e.Actions[req.OriginalFenceOperation]
	predecessor, predecessorOK := e.Actions[req.PredecessorFenceOperation]
	launch, launchOK := e.Actions[req.LaunchOperation]
	if !originalOK || !predecessorOK || !launchOK || original.Reconciliation == nil || predecessor.Reconciliation == nil || original.State != "completed" || predecessor.State != "completed" || launch.State != "completed" || launch.Expected != req.PredecessorIncarnation || launch.ResultingIncarnation != e.Spec.Incarnation || (launch.Kind != "start" && launch.Kind != "replace") {
		h.mu.Unlock()
		failure(w, errors.New("continuation execution transition does not bind"))
		return
	}
	binding := ContinuationBinding{Request: req, OriginalFence: *original.Reconciliation, PredecessorFence: *predecessor.Reconciliation}
	encoded, _ := json.Marshal(req)
	binding.RequestSHA256 = digest(encoded)
	socket := e.Spec.ControlSocket
	h.mu.Unlock()
	// Only the host can supply the machine-owned fencing records; this bounded
	// route persists the guest binding and never submits inference itself.
	result, status, err := postUnix(r.Context(), socket, "/v1/continuations", binding)
	if err != nil {
		failure(w, fmt.Errorf("binding response unavailable; inspect the same disposition: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(result)
}
