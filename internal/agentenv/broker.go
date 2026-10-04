package agentenv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Actor interface {
	Call(context.Context, string, any) (json.RawMessage, error)
	Notify(string, any) error
	Answer(json.RawMessage, any) error
	Done() <-chan struct{}
}

type Broker struct {
	Store         *Store
	Actor         Actor
	Workspace     string
	mu            sync.Mutex
	pending       map[string]Message
	authenticated bool
	loginPending  bool
}

func NewBroker(store *Store, workspace string) *Broker {
	return &Broker{Store: store, Workspace: workspace, pending: map[string]Message{}}
}

func (b *Broker) Initialize(ctx context.Context, actor Actor) error {
	b.Actor = actor
	if _, err := actor.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "pomar", "title": "Pomar", "version": "0.1.0"}}); err != nil {
		return err
	}
	if err := actor.Notify("initialized", map[string]any{}); err != nil {
		return err
	}
	return nil
}

func permissionKey(id json.RawMessage) string {
	sum := sha256.Sum256(id)
	return hex.EncodeToString(sum[:])
}

// Observe never records authentication responses or arbitrary server messages.
// Supported actor notifications become ordered durable evidence.
func (b *Broker) Observe(m Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.Method == "account/login/completed" {
		b.loginPending = false
		return nil
	}
	if !(strings.HasPrefix(m.Method, "item/") || m.Method == "turn/started" || m.Method == "turn/completed" || m.Method == "error") {
		return nil
	}
	if len(m.ID) > 0 {
		switch m.Method {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
			b.pending[permissionKey(m.ID)] = m
		default:
			// Unknown server requests receive no automatic approval.
			return fmt.Errorf("unsupported actor request %q", m.Method)
		}
	}
	return b.Store.Observe(b.Store.Snapshot().Incarnation, m.Method, m.ID, m.Params)
}

func (b *Broker) authenticatedAccount(ctx context.Context) (bool, error) {
	raw, err := b.Actor.Call(ctx, "account/read", map[string]bool{"refreshToken": false})
	if err != nil {
		return false, err
	}
	var response struct {
		Account json.RawMessage `json:"account"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return false, err
	}
	ok := len(response.Account) > 0 && string(response.Account) != "null"
	b.mu.Lock()
	b.authenticated = ok
	b.mu.Unlock()
	return ok, nil
}

func (b *Broker) Submit(task Task) (Operation, error) {
	if task.Incarnation != b.Store.Snapshot().Incarnation {
		return Operation{}, ErrStale
	}
	// Repeated accepted/uncertain input can be inspected without contacting
	// the actor or needing a still-valid model credential.
	if _, exists := b.Store.Snapshot().Operations[task.OperationID]; exists {
		op, _, err := b.Store.Begin(task)
		return op, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ok, err := b.authenticatedAccount(ctx)
	if err != nil {
		return Operation{}, err
	}
	if !ok {
		return Operation{}, errors.New("authentication required")
	}
	op, dispatch, err := b.Store.Begin(task)
	if err != nil || !dispatch {
		return op, err
	}
	unknown := func(cause error) (Operation, error) {
		b.Store.Unknown(task.Incarnation, task.OperationID)
		return b.Store.Snapshot().Operations[task.OperationID], fmt.Errorf("acceptance unknown; inspect operation, do not resubmit: %w", cause)
	}
	s := b.Store.Snapshot()
	if s.ThreadID == "" {
		raw, err := b.Actor.Call(ctx, "thread/start", map[string]any{"cwd": b.Workspace, "approvalPolicy": "on-request", "sandbox": "danger-full-access", "ephemeral": false})
		if err != nil {
			return unknown(err)
		}
		if err = b.Store.ObserveModel(task.Incarnation, task.OperationID, "thread/start", raw); err != nil {
			return unknown(err)
		}
		s = b.Store.Snapshot()
	}
	raw, err := b.Actor.Call(ctx, "turn/start", map[string]any{"threadId": s.ThreadID, "input": []map[string]string{{"type": "text", "text": "Pomar operation " + task.OperationID + ".\n" + task.Text}}})
	if err != nil {
		return unknown(err)
	}
	var out struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return unknown(err)
	}
	if err = b.Store.Accepted(task.Incarnation, task.OperationID, out.Turn.ID); err != nil {
		return unknown(err)
	}
	return b.Store.Snapshot().Operations[task.OperationID], nil
}

func (b *Broker) Resume(ctx context.Context) error {
	s := b.Store.Snapshot()
	if s.ThreadID == "" {
		return nil
	}
	id := "resume-" + s.Incarnation
	_, dispatch, err := b.Store.BeginControl("resume", id, s.Incarnation, map[string]string{"thread_id": s.ThreadID})
	if err != nil {
		return err
	}
	if !dispatch {
		return errors.New("resume already dispatched; inspect retained acceptance")
	}
	raw, err := b.Actor.Call(ctx, "thread/resume", map[string]any{"threadId": s.ThreadID, "cwd": b.Workspace, "approvalPolicy": "on-request", "sandbox": "danger-full-access"})
	if err == nil {
		err = b.Store.ObserveModel(s.Incarnation, id, "thread/resume", raw)
	}
	if err == nil {
		err = b.Store.ObserveRecoveredTurns(s.Incarnation, id, raw)
	}
	if finishErr := b.Store.FinishControl(id, err == nil); finishErr != nil {
		return finishErr
	}
	return err
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, err error) {
	code := http.StatusConflict
	if errors.Is(err, ErrStale) {
		code = http.StatusForbidden
	}
	respond(w, code, map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 96<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return false
	}
	return true
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		ok, err := b.authenticatedAccount(ctx)
		status := "alive"
		if err != nil {
			status = "unresponsive"
		}
		respond(w, 200, map[string]any{"session": b.Store.Snapshot(), "actor": status, "environment_alive": err == nil, "authenticated": ok, "adapter": map[string]any{"provider": "openai", "name": "codex-app-server", "version": "0.160.0", "capabilities": map[string]any{"version": "pomar.codex-capabilities/v1", "supported_operations": []string{"session.inspect", "login.device_code", "task.submit", "operation.inspect", "events.read", "permission.respond", "turn.interrupt", "result.export", "thread.resume_after_fence"}, "permission_response_kinds": []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"}, "permission_decisions": []string{"accept", "decline", "cancel"}}}})
	})
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Incarnation string `json:"expected_incarnation"`
			OperationID string `json:"operation_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.Incarnation != b.Store.Snapshot().Incarnation {
			failure(w, ErrStale)
			return
		}
		control, dispatch, err := b.Store.BeginControl("login", req.OperationID, req.Incarnation, map[string]string{"type": "chatgptDeviceCode"})
		if err != nil {
			failure(w, err)
			return
		}
		if !dispatch {
			respond(w, 200, control)
			return
		}
		b.mu.Lock()
		if b.loginPending {
			b.mu.Unlock()
			b.Store.FinishControl(req.OperationID, false)
			failure(w, errors.New("login already pending; do not silently restart it"))
			return
		}
		b.loginPending = true
		b.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		raw, err := b.Actor.Call(ctx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"})
		if journalErr := b.Store.FinishControl(req.OperationID, err == nil); journalErr != nil {
			failure(w, journalErr)
			return
		}
		if err != nil {
			failure(w, errors.New("login acceptance uncertain; inspect account status"))
			return
		}
		respond(w, 200, map[string]any{"operation": b.Store.Snapshot().Controls[req.OperationID], "login": json.RawMessage(raw)})
	})
	mux.HandleFunc("POST /v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req Task
		if !decode(w, r, &req) {
			return
		}
		op, err := b.Submit(req)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, op)
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s := b.Store.Snapshot()
		session, incarnation := r.URL.Query().Get("session_id"), r.URL.Query().Get("expected_incarnation")
		if session != s.SessionID || incarnation == "" {
			respond(w, 409, map[string]string{"error": "inspection requires original session and incarnation", "evidence": "unknown"})
			return
		}
		op, ok := s.Operations[r.PathValue("id")]
		if !ok {
			if control, ok := s.Controls[r.PathValue("id")]; ok {
				if incarnation != control.Incarnation {
					failure(w, ErrConflict)
					return
				}
				respond(w, 200, control)
				return
			}
			if incarnation != s.Incarnation {
				respond(w, 409, map[string]string{"error": "original incarnation has no retained operation evidence", "evidence": "unknown"})
				return
			}
			respond(w, 404, map[string]string{"error": "operation not recorded in current retained journal", "evidence": "durable_non_acceptance", "incarnation": s.Incarnation, "session_id": s.SessionID})
			return
		}
		if incarnation != op.Incarnation {
			failure(w, ErrConflict)
			return
		}
		respond(w, 200, op)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		if err != nil && r.URL.Query().Get("after") != "" {
			respond(w, 400, map[string]string{"error": "invalid event cursor"})
			return
		}
		s := b.Store.Snapshot()
		last := uint64(len(s.Events))
		if after > last {
			respond(w, 409, map[string]string{"error": "cursor beyond retained journal", "evidence": "unknown"})
			return
		}
		events := []Event{}
		for _, e := range s.Events {
			if e.Sequence > after {
				events = append(events, e)
			}
		}
		respond(w, 200, map[string]any{"contract_version": s.ContractVersion, "session_id": s.SessionID, "incarnation": s.Incarnation, "first_sequence": 1, "last_sequence": last, "next_cursor": last, "gap": false, "events": events})
	})
	mux.HandleFunc("POST /v1/permissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Incarnation string `json:"expected_incarnation"`
			Decision    string `json:"decision"`
			OperationID string `json:"operation_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if req.Incarnation != b.Store.Snapshot().Incarnation {
			failure(w, ErrStale)
			return
		}
		control, dispatch, err := b.Store.BeginControl("permission", req.OperationID, req.Incarnation, map[string]string{"permission_id": r.PathValue("id"), "decision": req.Decision})
		if err != nil {
			failure(w, err)
			return
		}
		if !dispatch {
			if control.State == "unsupported" {
				respond(w, http.StatusNotImplemented, map[string]string{"status": "unsupported", "operation_id": control.ID})
				return
			}
			respond(w, 200, control)
			return
		}
		m, ok := b.pending[r.PathValue("id")]
		if !ok {
			b.Store.FinishControl(req.OperationID, false)
			failure(w, errors.New("unknown or already answered permission request"))
			return
		}
		// Only per-request decisions. No blanket session/execpolicy approval.
		if (m.Method != "item/commandExecution/requestApproval" && m.Method != "item/fileChange/requestApproval") || (req.Decision != "accept" && req.Decision != "decline" && req.Decision != "cancel") {
			if err := b.Store.UnsupportedControl(req.OperationID); err != nil {
				failure(w, err)
				return
			}
			respond(w, http.StatusNotImplemented, map[string]string{"status": "unsupported", "kind": m.Method, "error": "unsupported permission response kind or decision"})
			return
		}
		params, _ := json.Marshal(map[string]string{"request": r.PathValue("id"), "decision": req.Decision})
		if err := b.Store.Observe(req.Incarnation, "permission/decision", m.ID, params); err != nil {
			failure(w, err)
			return
		}
		delete(b.pending, r.PathValue("id"))
		answerErr := b.Actor.Answer(m.ID, map[string]string{"decision": req.Decision})
		if err := b.Store.FinishControl(req.OperationID, answerErr == nil); err != nil {
			failure(w, err)
			return
		}
		if answerErr != nil {
			failure(w, errors.New("permission response acceptance unknown; not replayed"))
			return
		}
		respond(w, 200, b.Store.Snapshot().Controls[req.OperationID])
	})
	mux.HandleFunc("POST /v1/interrupt", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Incarnation string `json:"expected_incarnation"`
			OperationID string `json:"operation_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		s := b.Store.Snapshot()
		if req.Incarnation != s.Incarnation {
			failure(w, ErrStale)
			return
		}
		control, dispatch, err := b.Store.BeginControl("interrupt", req.OperationID, req.Incarnation, map[string]string{"thread_id": s.ThreadID})
		if err != nil {
			failure(w, err)
			return
		}
		if !dispatch {
			respond(w, 200, control)
			return
		}
		for _, op := range s.Operations {
			if op.Incarnation == s.Incarnation && op.TurnID != "" && op.Completion == "" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, callErr := b.Actor.Call(ctx, "turn/interrupt", map[string]string{"threadId": s.ThreadID, "turnId": op.TurnID})
				if err := b.Store.FinishControl(req.OperationID, callErr == nil); err != nil {
					failure(w, err)
					return
				}
				if err := callErr; err != nil {
					failure(w, err)
					return
				}
				respond(w, 200, b.Store.Snapshot().Controls[req.OperationID])
				return
			}
		}
		b.Store.FinishControl(req.OperationID, false)
		failure(w, errors.New("no known active turn; uncertain acceptance is not resubmitted"))
	})
	mux.HandleFunc("GET /v1/result", func(w http.ResponseWriter, r *http.Request) {
		s := b.Store.Snapshot()
		for _, op := range s.Operations {
			if op.Completion == "" {
				failure(w, ErrBusy)
				return
			}
		}
		root, err := os.OpenRoot(b.Workspace)
		if err != nil {
			failure(w, err)
			return
		}
		defer root.Close()
		diff, err := b.gitOutput(r.Context(), "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD")
		if err != nil {
			failure(w, errors.New("diff unavailable or exceeds limit"))
			return
		}
		paths, err := b.gitOutput(r.Context(), "ls-files", "--others", "--exclude-standard", "-z")
		if err != nil {
			failure(w, err)
			return
		}
		untracked := map[string]string{}
		remaining := 4 << 20
		for _, p := range strings.Split(string(paths), "\x00") {
			if p == "" {
				continue
			}
			if len(untracked) >= 32 || filepath.IsAbs(p) || strings.HasPrefix(p, "../") {
				failure(w, errors.New("untracked output limit or path refusal"))
				return
			}
			fi, err := root.Lstat(p)
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > int64(remaining) {
				failure(w, errors.New("untracked output is not a bounded regular file"))
				return
			}
			f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
			if err != nil {
				failure(w, err)
				return
			}
			opened, statErr := f.Stat()
			if statErr != nil || !opened.Mode().IsRegular() || opened.Size() > int64(remaining) {
				f.Close()
				failure(w, errors.New("untracked output changed or is not a bounded regular file"))
				return
			}
			data, err := io.ReadAll(io.LimitReader(f, int64(remaining)+1))
			f.Close()
			if len(data) > remaining {
				failure(w, errors.New("untracked output exceeds limit"))
				return
			}
			if err != nil {
				failure(w, err)
				return
			}
			remaining -= len(data)
			untracked[p] = string(data)
		}
		respond(w, 200, map[string]any{"session": s, "diff": string(diff), "untracked": untracked})
	})
	return mux
}

// Git configuration belongs to the coding user. Never execute it as the root
// broker. Bound output before allocation; cancellation kills only this owned
// export command, not the actor or its descendants.
func (b *Broker) gitOutput(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", b.Workspace}, args...)...)
	cmd.Env = []string{"PATH=/usr/local/go/bin:/usr/bin:/bin", "HOME=/pomar/job", "GIT_CONFIG_NOSYSTEM=1"}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
	}
	var out cappedOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("bounded workspace export failed")
	}
	return out.Bytes(), nil
}

type cappedOutput struct{ bytes.Buffer }

func (w *cappedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 4<<20 {
		return 0, errors.New("workspace output exceeds limit")
	}
	return w.Buffer.Write(p)
}
