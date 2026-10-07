// Package agentenv implements the agent environment's public, seat-independent
// session protocol. Durable input acceptance is separate from actor completion.
package agentenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

var (
	ErrStale    = errors.New("stale incarnation")
	ErrConflict = errors.New("operation id already binds different input")
	ErrBusy     = errors.New("a submission is still active or uncertain")
	validID     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

type Task struct {
	OperationID string `json:"operation_id"`
	Incarnation string `json:"expected_incarnation"`
	Text        string `json:"text"`
}

type Operation struct {
	ID                    string                 `json:"operation_id"`
	Incarnation           string                 `json:"incarnation"`
	InputHash             string                 `json:"input_sha256"`
	State                 string                 `json:"state"`
	TurnID                string                 `json:"turn_id,omitempty"`
	ThreadID              string                 `json:"thread_id,omitempty"`
	ActorAcknowledged     bool                   `json:"actor_acknowledged"`
	Completion            string                 `json:"completion,omitempty"`
	CompletionObservation *CompletionObservation `json:"completion_observation,omitempty"`
}

type CompletionObservation struct {
	Incarnation  string `json:"observer_incarnation"`
	OperationID  string `json:"operation_id"`
	SourceMethod string `json:"source_method"`
	ObservedAt   string `json:"observed_at"`
}

// ObserveRecoveredTurns accepts only a new authorized adapter's retained
// terminal turn observation. Absence/inProgress is UNKNOWN, never evidence
// of non-acceptance. No input or turn/start is replayed by this recovery.
func (m *Store) ObserveRecoveredTurns(incarnation, operation string, raw json.RawMessage) error {
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(incarnation); err != nil {
		return err
	}
	c, ok := m.s.Controls[operation]
	if !ok || c.Kind != "resume" || c.Incarnation != incarnation || response.Thread.ID != m.s.ThreadID || response.Thread.ID == "" {
		return errors.New("recovery observation is not bound to the resumed thread/control")
	}
	turns := map[string]string{}
	for _, turn := range response.Thread.Turns {
		if turn.ID == "" {
			return errors.New("retained turn lacks identity")
		}
		if _, exists := turns[turn.ID]; exists {
			return errors.New("duplicate retained turn identity")
		}
		turns[turn.ID] = turn.Status
	}
	for id, op := range m.s.Operations {
		if op.Incarnation == incarnation || op.Completion != "" || op.TurnID == "" || m.closedUnresolved(id) || (op.ThreadID != "" && op.ThreadID != response.Thread.ID) {
			continue
		}
		status, found := turns[op.TurnID]
		if !found {
			continue
		}
		if status != "completed" && status != "failed" && status != "interrupted" {
			continue
		}
		op.State = "finished"
		op.Completion = status
		op.CompletionObservation = &CompletionObservation{Incarnation: incarnation, OperationID: operation, SourceMethod: "thread/resume", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		m.s.Operations[id] = op
	}
	return m.save()
}

type Event struct {
	Sequence     uint64          `json:"sequence"`
	Incarnation  string          `json:"incarnation"`
	Method       string          `json:"method"`
	RequestID    json.RawMessage `json:"request_id,omitempty"`
	Params       json.RawMessage `json:"params"`
	ParamsSHA256 string          `json:"params_sha256,omitempty"`
	PermissionID string          `json:"permission_id,omitempty"`
}

// RejectedEvent retains routing provenance only, never arbitrary provider
// content. It is diagnostic evidence, not an accepted event or actor outcome.
type RejectedEvent struct {
	Incarnation        string `json:"source_incarnation"`
	CurrentIncarnation string `json:"current_incarnation"`
	Method             string `json:"method"`
	ThreadID           string `json:"thread_id,omitempty"`
	TurnID             string `json:"turn_id,omitempty"`
	OperationID        string `json:"operation_id,omitempty"`
	ParamsSHA256       string `json:"params_sha256"`
	Reason             string `json:"reason"`
}

type Control struct {
	ID          string `json:"operation_id"`
	Kind        string `json:"kind"`
	Incarnation string `json:"incarnation"`
	InputHash   string `json:"input_sha256"`
	State       string `json:"state"`
	Evidence    string `json:"evidence,omitempty"`
	Refusal     string `json:"refusal,omitempty"`
}

type State struct {
	AdapterSelection       *AdapterSelection              `json:"adapter_selection,omitempty"`
	ControllerCapabilities []string                       `json:"controller_capabilities,omitempty"`
	ControllerRequests     map[string]ControllerRequest   `json:"controller_requests,omitempty"`
	EnvironmentID          string                         `json:"environment_id"`
	SessionID              string                         `json:"session_id"`
	Incarnation            string                         `json:"incarnation"`
	ThreadID               string                         `json:"thread_id,omitempty"`
	Operations             map[string]Operation           `json:"operations"`
	Events                 []Event                        `json:"events"`
	RejectedEvents         []RejectedEvent                `json:"rejected_events,omitempty"`
	Controls               map[string]Control             `json:"controls"`
	Continuations          map[string]ContinuationBinding `json:"continuations,omitempty"`
	ContractVersion        string                         `json:"contract_version"`
	WorkspaceID            string                         `json:"workspace_id"`
	SourceSHA              string                         `json:"source_sha,omitempty"`
	ScopeID                string                         `json:"scope_id"`
	RequestedModel         string                         `json:"requested_model"`
	RequestedProvider      string                         `json:"requested_provider"`
	ModelObservation       ModelObservation               `json:"model_observation"`
	ModelObservations      []ModelObservation             `json:"model_observations,omitempty"`
}

// ModelObservation is adapter-reported selection, not backend routing attestation.
type ModelObservation struct {
	Status        string `json:"status"`
	Model         string `json:"model,omitempty"`
	Provider      string `json:"provider,omitempty"`
	ThreadID      string `json:"thread_id,omitempty"`
	IncarnationID string `json:"incarnation_id,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
	ObservedAt    string `json:"observed_at,omitempty"`
	SourceMethod  string `json:"source_method,omitempty"`
}

type Store struct {
	mu    sync.Mutex
	dir   string
	lock  *os.File
	s     State
	fatal error
}

// Open requires an explicit incarnation. It never automatically replays an
// operation after restart. The caller must fence old execution before replacing
// an incarnation; the store enforces the resulting reporting/input boundary.
func Open(dir, environment, session, incarnation string) (*Store, error) {
	for _, id := range []string{environment, session, incarnation} {
		if !validID.MatchString(id) {
			return nil, errors.New("invalid environment, session or incarnation id")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("agent state directory must be private and not redirected")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("agent store already owned: %w", err)
	}
	m := &Store{dir: dir, lock: lock, s: State{EnvironmentID: environment, SessionID: session, Incarnation: incarnation, Operations: map[string]Operation{}, Controls: map[string]Control{}, Events: []Event{}}}
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err == nil {
		if len(data) > 16<<20 {
			m.Close()
			return nil, errors.New("agent journal exceeds limit")
		}
		if err = json.Unmarshal(data, &m.s); err != nil {
			m.Close()
			return nil, err
		}
		if m.s.EnvironmentID != environment || m.s.SessionID != session || m.s.Operations == nil {
			m.Close()
			return nil, errors.New("stored identity mismatch")
		}
		if m.s.Incarnation == incarnation {
			m.Close()
			return nil, errors.New("broker restart requires a new fenced actor incarnation; surviving broker reconnect uses its existing socket")
		}
		m.s.Incarnation = incarnation
		for id, op := range m.s.Operations {
			if op.Completion == "" {
				op.State = "acceptance_unknown"
				m.s.Operations[id] = op
			}
		}
		for id, control := range m.s.Controls {
			if control.State == "dispatching" {
				control.State = "acceptance_unknown"
				m.s.Controls[id] = control
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		m.Close()
		return nil, err
	}
	if errors.Is(err, os.ErrNotExist) {
		if _, markerErr := os.Lstat(filepath.Join(dir, "identity.json")); markerErr == nil {
			m.Close()
			return nil, errors.New("retained journal missing; acceptance evidence unknown")
		}
	}
	if m.s.Controls == nil {
		m.s.Controls = map[string]Control{}
	}
	if m.s.ControllerRequests == nil {
		m.s.ControllerRequests = map[string]ControllerRequest{}
	}
	for id, request := range m.s.ControllerRequests {
		if request.State == "dispatching" {
			request.State = "acceptance_unknown"
			m.s.ControllerRequests[id] = request
		}
		if request.State == "pending" && request.Binding.Incarnation != m.s.Incarnation {
			request.State = "fenced"
			m.s.ControllerRequests[id] = request
		}
	}
	m.s.ContractVersion = "pomar.agent/v1"
	m.s.ModelObservation = ModelObservation{Status: "unknown"}
	m.s.WorkspaceID = "workspace-" + environment
	m.s.ScopeID = "scope-" + incarnation
	if err = m.persist(); err != nil {
		m.Close()
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, "identity.json")); errors.Is(err, os.ErrNotExist) {
		identity, _ := json.Marshal(map[string]string{"environment_id": environment, "session_id": session})
		if err := os.WriteFile(filepath.Join(dir, "identity.json"), identity, 0o600); err != nil {
			m.Close()
			return nil, err
		}
	}
	return m, nil
}

func (m *Store) Close() error { return m.lock.Close() }

func (m *Store) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *Store) snapshotLocked() State {
	b, _ := json.Marshal(m.s)
	var out State
	json.Unmarshal(b, &out)
	return out
}

func (m *Store) check(incarnation string) error {
	if m.fatal != nil {
		return fmt.Errorf("journal write uncertain: %w", m.fatal)
	}
	if incarnation != m.s.Incarnation {
		return ErrStale
	}
	return nil
}

func (m *Store) persist() error {
	b, err := json.Marshal(m.s)
	if err != nil {
		return err
	}
	if len(b) > 16<<20 {
		return errors.New("agent journal exceeds limit")
	}
	f, err := os.CreateTemp(m.dir, ".session-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(m.dir, "session.json")); err != nil {
		return err
	}
	d, err := os.Open(m.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (m *Store) save() error {
	if err := m.persist(); err != nil {
		m.fatal = err
		return err
	}
	return nil
}

// Begin returns dispatch=true exactly once, only after the acceptance journal
// was synced. Retrying an uncertain operation never calls the actor again.
func (m *Store) Begin(task Task) (op Operation, dispatch bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err = m.check(task.Incarnation); err != nil {
		return
	}
	if !validID.MatchString(task.OperationID) || len(task.Text) == 0 || len(task.Text) > 64<<10 {
		err = errors.New("invalid operation id or task size")
		return
	}
	b, _ := json.Marshal(task)
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	if _, ok := m.s.Controls[task.OperationID]; ok {
		return Operation{}, false, ErrConflict
	}
	if existing, ok := m.s.Operations[task.OperationID]; ok {
		if existing.InputHash != hash {
			err = ErrConflict
			return
		}
		return existing, false, nil
	}
	for _, existing := range m.s.Operations {
		if existing.Completion == "" && !m.continuedBy(existing.ID, task.OperationID) {
			err = ErrBusy
			return
		}
	}
	for _, binding := range m.s.Continuations {
		if binding.Successor.OperationID == task.OperationID && binding.SuccessorInputSHA256 != hash {
			return Operation{}, false, ErrConflict
		}
	}
	op = Operation{ID: task.OperationID, Incarnation: task.Incarnation, InputHash: hash, State: "dispatching"}
	m.s.Operations[op.ID] = op
	err = m.save()
	return op, err == nil, err
}

func (m *Store) Thread(incarnation, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(incarnation); err != nil {
		return err
	}
	if id == "" {
		return errors.New("empty thread id")
	}
	m.s.ThreadID = id
	return m.save()
}

func (m *Store) ObserveModel(incarnation, operation, method string, raw json.RawMessage) error {
	var out struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model    string `json:"model"`
		Provider string `json:"modelProvider"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if out.Thread.ID == "" {
		return errors.New("thread response lacks identity")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(incarnation); err != nil {
		return err
	}
	if method != "thread/start" && method != "thread/resume" {
		return errors.New("unsupported observation source")
	}
	if !validID.MatchString(operation) {
		return errors.New("invalid observation operation")
	}
	if op, ok := m.s.Operations[operation]; ok {
		if op.Incarnation != incarnation {
			return ErrStale
		}
	} else {
		c, found := m.s.Controls[operation]
		if !found || c.Incarnation != incarnation {
			return errors.New("model observation requires retained operation")
		}
	}
	if method == "thread/resume" && out.Thread.ID != m.s.ThreadID {
		return errors.New("resumed thread identity mismatch")
	}
	if op, ok := m.s.Operations[operation]; ok {
		if method == "thread/start" && op.ThreadID != "" && op.ThreadID != out.Thread.ID {
			return ErrConflict
		}
		op.ThreadID = out.Thread.ID
		m.s.Operations[operation] = op
	}
	m.s.ThreadID = out.Thread.ID
	for id, binding := range m.s.Continuations {
		if binding.Successor.OperationID == operation {
			binding.SuccessorThreadID = out.Thread.ID
			m.s.Continuations[id] = binding
		}
	}
	m.s.ModelObservation = ModelObservation{Status: "unknown"}
	if out.Model != "" && out.Provider != "" {
		m.s.ModelObservation = ModelObservation{Status: "observed", Model: out.Model, Provider: out.Provider, ThreadID: out.Thread.ID, IncarnationID: incarnation, OperationID: operation, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), SourceMethod: method}
		m.s.ModelObservations = append(m.s.ModelObservations, m.s.ModelObservation)
	}
	return m.save()
}

func (m *Store) Accepted(incarnation, operation, turn string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(incarnation); err != nil {
		return err
	}
	op, ok := m.s.Operations[operation]
	if !ok || op.Incarnation != incarnation {
		return ErrStale
	}
	if turn == "" {
		return errors.New("empty turn id")
	}
	// Notifications may reach the journal before the turn/start reply.
	if op.TurnID != "" && op.TurnID != turn {
		return ErrConflict
	}
	op.TurnID = turn
	// A turn is accepted on the session's current thread. Only the operation
	// that starts a thread learns it from thread/start; later operations on
	// the same thread, and every operation after a resume, are bound here, so
	// their turns' controller requests can be bound to them.
	if op.ThreadID == "" {
		op.ThreadID = m.s.ThreadID
	}
	if op.Completion == "" {
		op.State = "accepted"
	}
	m.s.Operations[operation] = op
	return m.save()
}

func (m *Store) Unknown(incarnation, operation string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(incarnation); err != nil {
		return err
	}
	op, ok := m.s.Operations[operation]
	if !ok {
		return errors.New("unknown operation")
	}
	if op.Completion == "" && op.TurnID == "" {
		op.State = "acceptance_unknown"
		m.s.Operations[operation] = op
	}
	return m.save()
}

func (m *Store) Observe(incarnation, method string, requestID, params json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(incarnation); err != nil {
		if errors.Is(err, ErrStale) {
			return m.rejectStaleEvent(incarnation, method, params)
		}
		return err
	}
	if len(m.s.Events) >= 8192 || len(params) > 256<<10 {
		return errors.New("agent event limit reached")
	}
	sequence := uint64(len(m.s.Events) + 1)
	event := Event{Sequence: sequence, Incarnation: incarnation, Method: method, RequestID: requestID, Params: params}
	if len(requestID) > 0 {
		event.PermissionID = permissionKey(incarnation, requestID)
	}
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return err
	}
	if p.ThreadID != "" && p.ThreadID != m.s.ThreadID {
		return errors.New("actor event belongs to another thread")
	}
	if (method == "turn/started" || method == "turn/completed") && (p.ThreadID == "" || p.Turn.ID == "") {
		return errors.New("turn event lacks thread or turn identity")
	}
	if method == "turn/completed" && p.Turn.Status != "completed" && p.Turn.Status != "failed" && p.Turn.Status != "interrupted" {
		return errors.New("unknown completion status")
	}
	event.Params, event.ParamsSHA256 = projectLifecycleEvidence(method, params)
	m.s.Events = append(m.s.Events, event)
	turn := p.TurnID
	if turn == "" {
		turn = p.Turn.ID
	}
	for id, op := range m.s.Operations {
		if op.Incarnation != incarnation || op.Completion != "" {
			continue
		}
		if op.TurnID != "" && op.TurnID != turn {
			continue
		}
		if method == "turn/started" {
			op.TurnID = turn
			op.State = "accepted"
			op.ActorAcknowledged = true
			// turn/started can precede the turn/start reply; its thread is
			// the session's, checked above.
			if op.ThreadID == "" {
				op.ThreadID = p.ThreadID
			}
		}
		if method == "turn/completed" && turn != "" {
			switch p.Turn.Status {
			case "completed", "failed", "interrupted":
				op.TurnID = turn
				op.Completion = p.Turn.Status
				op.State = "finished"
			default:
				return errors.New("unknown completion status")
			}
		}
		m.s.Operations[id] = op
	}
	return m.save()
}

// Caller holds mu. A stale notification cannot enter Events or change tasks,
// bindings or permissions, even if its thread and turn otherwise look valid.
func (m *Store) rejectStaleEvent(incarnation, method string, params json.RawMessage) error {
	if len(m.s.RejectedEvents) >= 1024 || len(params) > 256<<10 {
		return ErrStale
	}
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ErrStale
	}
	turn := p.TurnID
	if turn == "" {
		turn = p.Turn.ID
	}
	rejected := RejectedEvent{Incarnation: incarnation, CurrentIncarnation: m.s.Incarnation, Method: method, ThreadID: p.ThreadID, TurnID: turn, ParamsSHA256: digest(params), Reason: "stale incarnation"}
	for id, op := range m.s.Operations {
		if op.Incarnation == incarnation && op.TurnID != "" && op.TurnID == turn && (op.ThreadID == "" || op.ThreadID == p.ThreadID) {
			rejected.OperationID = id
			break
		}
	}
	m.s.RejectedEvents = append(m.s.RejectedEvents, rejected)
	if err := m.save(); err != nil {
		return err
	}
	return ErrStale
}

func (m *Store) BindSource(sha string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s.SourceSHA != "" && m.s.SourceSHA != sha {
		return errors.New("workspace source baseline changed")
	}
	m.s.SourceSHA = sha
	return m.save()
}

func (m *Store) BeginControl(kind, id, incarnation string, payload any) (Control, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(incarnation); err != nil {
		return Control{}, false, err
	}
	if !validID.MatchString(id) {
		return Control{}, false, errors.New("invalid operation id")
	}
	if _, ok := m.s.Operations[id]; ok {
		return Control{}, false, ErrConflict
	}
	b, err := json.Marshal(map[string]any{"kind": kind, "expected_incarnation": incarnation, "payload": payload})
	if err != nil {
		return Control{}, false, err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	if existing, ok := m.s.Controls[id]; ok {
		if existing.InputHash != hash {
			return Control{}, false, ErrConflict
		}
		return existing, false, nil
	}
	control := Control{ID: id, Kind: kind, Incarnation: incarnation, InputHash: hash, State: "dispatching"}
	m.s.Controls[id] = control
	err = m.save()
	return control, err == nil, err
}

func (m *Store) FinishControl(id string, known bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.s.Controls[id]
	if !ok {
		return errors.New("unknown control operation")
	}
	if err := m.check(c.Incarnation); err != nil {
		return err
	}
	c.State = "sent"
	if !known {
		c.State = "acceptance_unknown"
	}
	m.s.Controls[id] = c
	return m.save()
}

func (m *Store) UnsupportedControl(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.s.Controls[id]
	if !ok {
		return errors.New("unknown control operation")
	}
	if err := m.check(c.Incarnation); err != nil {
		return err
	}
	c.State = "unsupported"
	m.s.Controls[id] = c
	return m.save()
}

// RefusePermission records proven non-dispatch without releasing the operation
// ID. A later request with that permission ID cannot make a retry send an answer.
func (m *Store) RefusePermission(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.s.Controls[id]
	if !ok || c.Kind != "permission" || c.State != "dispatching" {
		return errors.New("permission refusal requires an undispatched control")
	}
	if err := m.check(c.Incarnation); err != nil {
		return err
	}
	c.State = "refused"
	c.Evidence = "durable_non_acceptance"
	c.Refusal = "unknown or already answered permission request"
	m.s.Controls[id] = c
	return m.save()
}
