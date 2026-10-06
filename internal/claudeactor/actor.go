// Package claudeactor drives Claude Code as a Pomar agent-environment actor.
// It implements agentenv.Actor by translating the broker's calls (the Codex
// app-server vocabulary the shared broker speaks) onto one persistent
// `claude -p` process per thread, in stream-json on stdin and stdout. The
// shared broker, journal, fencing and controller contract are unchanged.
//
// The stream-json input shapes are not in Claude Code's public documentation:
// they are pinned to one Claude Code version and must be qualified against
// that exact binary, as the Codex adapter is pinned to app-server 0.160.0.
package claudeactor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// Name identifies this adapter in the userAgent it reports.
const Name = "pomar-claude-actor"

// Process is one running Claude Code process: its stdin and stdout, an
// interrupt, and its exit.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Interrupt() error
	Wait() error
}

// Config is what the actor needs. Start launches Claude Code with the given
// arguments (for example an exec in the guest, or a fake in tests).
type Config struct {
	// Version is the pinned Claude Code version this actor was qualified
	// against; it is reported, not verified, like Codex's userAgent check.
	Version string
	// Args are the fixed arguments before the session flags, for example
	// --setting-sources user --strict-mcp-config. Not --bare: in 2.1.280 it
	// never reads OAuth credentials (claude --help).
	Args []string
	// Start launches the process with the full argument list.
	Start func(args []string) (Process, error)
	// Authenticated reports whether a credential is configured for the coding
	// user, without reading or returning its value.
	Authenticated func() bool
	// NewID returns a fresh UUID; nil uses crypto/rand.
	NewID func() string
}

// Actor is one adapter connection, bound to one broker incarnation.
type Actor struct {
	cfg     Config
	observe func(agentenv.Message) error

	mu        sync.Mutex
	proc      Process
	thread    string
	turn      string // the active turn, or ""
	started   bool   // turn/started emitted for the active turn
	interrupt bool   // an interrupt was requested for the active turn
	done      chan struct{}
	closeOnce sync.Once
	err       error
}

// New returns an actor that reports actor messages to observe.
func New(cfg Config, observe func(agentenv.Message) error) *Actor {
	if cfg.NewID == nil {
		cfg.NewID = newUUID
	}
	return &Actor{cfg: cfg, observe: observe, done: make(chan struct{})}
}

var (
	ErrUnsupported = errors.New("claude actor: unsupported method")
	ErrNoThread    = errors.New("claude actor: no running thread")
	ErrBusy        = errors.New("claude actor: a turn is already running")
)

// Call answers the broker's requests. Only the methods the shared broker uses
// are supported; anything else is refused, never approximated.
func (a *Actor) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.Marshal(map[string]string{"userAgent": fmt.Sprintf("%s/%s (claude-code %s)", Name, "0.1.0", a.cfg.Version)})
	case "account/read":
		// The account is "present" when a credential is configured; its value
		// is never read here, returned or journaled.
		if a.cfg.Authenticated != nil && a.cfg.Authenticated() {
			return json.Marshal(map[string]any{"account": map[string]string{"type": "claude-code"}})
		}
		return json.Marshal(map[string]any{"account": nil})
	case "thread/start":
		return a.startThread(ctx)
	case "thread/resume":
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := remarshal(params, &p); err != nil || p.ThreadID == "" {
			return nil, errors.New("claude actor: resume needs a thread id")
		}
		return a.resumeThread(p.ThreadID)
	case "turn/start":
		var p struct {
			ThreadID string `json:"threadId"`
			Input    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		}
		if err := remarshal(params, &p); err != nil {
			return nil, err
		}
		return a.startTurn(p.ThreadID, p.Input)
	case "turn/interrupt":
		return a.interruptTurn()
	}
	// account/login/start has no stream-json equivalent: Claude Code's
	// sign-in is an interactive terminal flow (POMAR-CC SOW 01 §5 P4).
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, method)
}

// Notify accepts the broker's notifications; "initialized" needs no action.
func (a *Actor) Notify(method string, params any) error {
	if method == "initialized" {
		return nil
	}
	return fmt.Errorf("%w: notify %s", ErrUnsupported, method)
}

// Answer would deliver a permission decision or a controller reply. They
// arrive through the MCP bridge, which is not wired in this version: no
// request is ever raised, so any answer is refused rather than dropped.
func (a *Actor) Answer(id json.RawMessage, result any) error {
	return fmt.Errorf("%w: answer %s", ErrUnsupported, string(id))
}

// Done closes when the Claude Code process has exited.
func (a *Actor) Done() <-chan struct{} { return a.done }

func (a *Actor) startThread(ctx context.Context) (json.RawMessage, error) {
	a.mu.Lock()
	if a.proc != nil {
		a.mu.Unlock()
		return nil, errors.New("claude actor: a thread is already running")
	}
	a.mu.Unlock()
	id := a.cfg.NewID()
	if err := a.launch(id, "--session-id", id); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"thread": map[string]string{"id": id}})
}

func (a *Actor) resumeThread(id string) (json.RawMessage, error) {
	a.mu.Lock()
	if a.proc != nil {
		a.mu.Unlock()
		return nil, errors.New("claude actor: a thread is already running")
	}
	a.mu.Unlock()
	if err := a.launch(id, "--resume", id); err != nil {
		return nil, err
	}
	// No retained turn statuses are claimed: Claude Code's transcript is not
	// read here, so recovery leaves earlier operations unknown, never inferred.
	return json.Marshal(map[string]any{"thread": map[string]any{"id": id, "turns": []any{}}})
}

func (a *Actor) launch(thread string, flag, id string) error {
	args := append(append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, a.cfg.Args...), flag, id)
	proc, err := a.cfg.Start(args)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.proc, a.thread = proc, thread
	a.mu.Unlock()
	go a.read(proc)
	return nil
}

func (a *Actor) startTurn(thread string, input []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) (json.RawMessage, error) {
	a.mu.Lock()
	if a.proc == nil && a.thread != "" && thread == a.thread && a.err == nil {
		// The previous turn was interrupted and its process exited: resume
		// the same session in a new process before this turn.
		a.mu.Unlock()
		if err := a.launch(thread, "--resume", thread); err != nil {
			return nil, err
		}
		a.mu.Lock()
	}
	defer a.mu.Unlock()
	if a.proc == nil || thread != a.thread || a.err != nil {
		return nil, ErrNoThread
	}
	if a.turn != "" {
		return nil, ErrBusy
	}
	var content []map[string]string
	for _, in := range input {
		if in.Type != "text" {
			return nil, fmt.Errorf("claude actor: unsupported input type %q", in.Type)
		}
		content = append(content, map[string]string{"type": "text", "text": in.Text})
	}
	if len(content) == 0 {
		return nil, errors.New("claude actor: empty turn input")
	}
	line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	if err != nil {
		return nil, err
	}
	turn := a.cfg.NewID()
	if _, err = a.proc.Stdin().Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("claude actor: turn not delivered: %w", err)
	}
	a.turn, a.started, a.interrupt = turn, false, false
	return json.Marshal(map[string]any{"turn": map[string]string{"id": turn}})
}

func (a *Actor) interruptTurn() (json.RawMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc == nil || a.turn == "" {
		return nil, errors.New("claude actor: no active turn")
	}
	if err := a.proc.Interrupt(); err != nil {
		return nil, err
	}
	a.interrupt = true
	return json.Marshal(map[string]any{})
}

// streamLine is the part of a Claude Code stream-json output line the actor
// reads; anything else in it is carried, not interpreted.
type streamLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	IsError   bool            `json:"is_error"`
	Message   json.RawMessage `json:"message"`
}

func (a *Actor) read(proc Process) {
	s := bufio.NewScanner(proc.Stdout())
	s.Buffer(make([]byte, 4096), 2<<20)
	var err error
	for s.Scan() {
		var l streamLine
		if err = json.Unmarshal(s.Bytes(), &l); err != nil {
			err = fmt.Errorf("claude actor: unreadable stream line: %w", err)
			break
		}
		if err = a.handle(l, s.Bytes()); err != nil {
			break
		}
	}
	if err == nil {
		err = s.Err()
	}
	if err != nil {
		// A protocol violation ends this connection: no more input is
		// written, so the process sees end of input and exits.
		proc.Stdin().Close()
	}
	waitErr := proc.Wait()
	if err == nil {
		err = waitErr
	}
	if err == nil {
		err = io.EOF
	}
	a.finish(proc, err)
}

func (a *Actor) handle(l streamLine, raw []byte) error {
	a.mu.Lock()
	thread, turn := a.thread, a.turn
	if l.SessionID != "" && l.SessionID != thread {
		a.mu.Unlock()
		return fmt.Errorf("claude actor: stream belongs to session %q, not %q", l.SessionID, thread)
	}
	if turn == "" {
		a.mu.Unlock()
		// Output outside a turn is not evidence of any operation.
		return nil
	}
	first := !a.started
	a.started = true
	a.mu.Unlock()
	if first {
		if err := a.emit("turn/started", map[string]any{"threadId": thread, "turn": map[string]string{"id": turn, "status": "inProgress"}}); err != nil {
			return err
		}
	}
	switch l.Type {
	case "assistant", "user":
		return a.emit("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "claude/" + l.Type, "message": l.Message}})
	case "result":
		a.mu.Lock()
		status := "completed"
		switch {
		case a.interrupt:
			status = "interrupted"
		case l.IsError || (l.Subtype != "" && l.Subtype != "success"):
			status = "failed"
		}
		if status == "interrupted" {
			// Print mode exits after an interrupted turn (qualified on
			// 2.1.280): retire this process now, so the next turn resumes
			// the session in a new one, and this one's exit is ignored.
			a.proc = nil
		}
		a.turn, a.started, a.interrupt = "", false, false
		a.mu.Unlock()
		return a.emit("turn/completed", map[string]any{"threadId": thread, "turn": map[string]string{"id": turn, "status": status}})
	}
	// system and other stream events are not operation evidence.
	return nil
}

func (a *Actor) emit(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return a.observe(agentenv.Message{Method: method, Params: raw})
}

func (a *Actor) finish(proc Process, err error) {
	a.mu.Lock()
	if a.proc != proc {
		// A retired process (after an interrupted turn) ending is expected;
		// the session continues in the next process.
		a.mu.Unlock()
		return
	}
	a.err = err
	a.mu.Unlock()
	a.closeOnce.Do(func() { close(a.done) })
}

func remarshal(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
