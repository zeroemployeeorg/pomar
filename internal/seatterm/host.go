package seatterm

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/zeroemployeeorg/pomar/internal/termstate"
)

// SessionHost holds a seat's terminal sessions in its guest, in place of a
// terminal multiplexer (POMAR-CC SOW 27, made agent-agnostic by SOW 28):
// each session is one program on a PTY the host owns, so the program
// outlives every client. Any number of clients, up to a bound, view a
// session; at most one controls it. Only the controller's input and window
// size reach the PTY, and each is fenced at the write itself by the
// controller's grant epoch, so a revoked controller's queued input never
// lands. The host knows nothing about which program a session runs.
type SessionHost struct {
	mu       sync.Mutex
	sessions map[string]*TermSession
	byOp     map[opKey]created // creations, by principal and operation ID
	opOrder  []opKey
	starting int // sessions reserved and starting, counted against the bound
	audit    func(AuditRecord) error
	now      func() time.Time
	// fenceBound and writeHook are fixed when the host is made; the tests
	// shorten the one and set the other, before any session starts.
	fenceBound  time.Duration
	hangupGrace time.Duration // how long Terminate waits after the hangup before it kills
	writeHook   func()
}

// Bounds on a host, its sessions and their clients.
const (
	MaxSessions = 8
	MaxViewers  = 8       // per session, the controller included
	RetainBytes = 1 << 20 // a session's retained output
	viewerQueue = 256     // output chunks queued for one client
	inputQueue  = 64      // input and resize writes queued for the PTY
	// HistoryLines bounds the lines a session's screen model keeps above
	// its screen.
	HistoryLines = 2000
)

// defaultFenceBound bounds how long a take waits for the old controller's
// write in flight to end.
const defaultFenceBound = 3 * time.Second

// Errors a client is refused with.
var (
	ErrNotController      = errors.New("this client doesn't hold control of the session")
	ErrControlled         = errors.New("another client controls the session; take control explicitly")
	ErrInputBusy          = errors.New("the session's input queue is full; nothing was queued")
	ErrSessionExited      = errors.New("the session's program has exited")
	ErrTooMany            = errors.New("the bound on sessions or clients is reached")
	ErrFenceUncertain     = errors.New("the previous controller's write couldn't be confirmed fenced; control was not granted")
	ErrAudit              = errors.New("the audit record couldn't be written; nothing was granted")
	ErrOperationConflict  = errors.New("this operation ID already created a session with a different spec")
	ErrTerminateUncertain = errors.New("the program could not be confirmed stopped")
	ErrCreating           = errors.New("this operation is still creating its session; ask again")
	ErrEpochStale         = errors.New("control has changed since the epoch you expected; read it again")
)

// AuditRecord is the metadata the host journals for every grant, revocation
// and session change: never keystrokes, screen contents or arguments.
type AuditRecord struct {
	At        time.Time `json:"at"`
	Operation string    `json:"operation_id"`
	Action    string    `json:"action"` // create, attach, detach, grant, take, release, revoke, exit
	Phase     string    `json:"phase"`  // intent or outcome
	Principal string    `json:"principal"`
	Session   string    `json:"session"`
	Epoch     uint64    `json:"epoch,omitempty"`
	Outcome   string    `json:"outcome,omitempty"` // ok, refused, uncertain, error
	Reason    string    `json:"reason,omitempty"`
}

// SessionSpec is what a new session runs. Name is a display label, never
// authority; the session's ID is its identity. The command's Path is
// absolute, never looked up on the host's PATH, and its environment is
// exactly Command.Env: the host adds nothing, not even PATH, TERM or HOME,
// so whoever makes the spec names each one.
type SessionSpec struct {
	Name       string
	Kind       string // agent, shell, or another lower-case word
	Command    Command
	Cols, Rows uint16
}

var sessionWord = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// NewSessionHost returns a host that journals through audit, which must be
// durable: a grant is refused when its intent can't be recorded. The journal
// is never called under a session's lock, but a journal that hangs does hold
// up whatever is journaling: a create, an attach, a grant, and a session's
// exit, whose Done closes only once its exit is journaled.
func NewSessionHost(audit func(AuditRecord) error) *SessionHost {
	return &SessionHost{sessions: map[string]*TermSession{}, byOp: map[opKey]created{}, audit: audit, now: time.Now, fenceBound: defaultFenceBound, hangupGrace: 3 * time.Second}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (h *SessionHost) record(r AuditRecord) error {
	r.At = h.now().UTC()
	if h.audit == nil {
		return errors.New("no audit journal")
	}
	return h.audit(r)
}

// Create starts a session, once per principal and operation ID: a retry
// with the same pair returns the session it made, and never starts a
// second program; a retry asking for a different spec is refused.
func (h *SessionHost) Create(operation, principal string, spec SessionSpec) (*TermSession, bool, error) {
	if operation == "" || principal == "" || !sessionWord.MatchString(spec.Name) || !sessionWord.MatchString(spec.Kind) || !filepath.IsAbs(spec.Command.Path) ||
		spec.Cols < 1 || spec.Cols > MaxCols || spec.Rows < 1 || spec.Rows > MaxRows {
		return nil, false, errors.New("a session needs an operation ID, a principal, a name, a kind, its command's absolute path and a bounded window size")
	}
	key, fp := opKey{principal, operation}, fingerprint(spec)
	// The operation is reserved under the lock, and journaled and started
	// outside it, so a slow journal or a slow start holds nothing else up.
	h.mu.Lock()
	if e, ok := h.byOp[key]; ok {
		h.mu.Unlock()
		switch {
		case e.spec != fp:
			return nil, false, ErrOperationConflict
		case e.s == nil:
			return nil, false, ErrCreating
		}
		return e.s, false, nil
	}
	if len(h.sessions)+h.starting >= MaxSessions { // live sessions only: an exited one leaves h.sessions
		h.mu.Unlock()
		return nil, false, ErrTooMany
	}
	id := newID()
	h.byOp[key] = created{spec: fp}
	h.starting++
	h.mu.Unlock()
	abandon := func() {
		h.mu.Lock()
		delete(h.byOp, key)
		h.starting--
		h.mu.Unlock()
	}
	if err := h.record(AuditRecord{Operation: operation, Action: "create", Phase: "intent", Principal: principal, Session: id, Reason: spec.Kind}); err != nil {
		abandon()
		return nil, false, ErrAudit
	}
	s, err := startSession(h, id, spec)
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	h.record(AuditRecord{Operation: operation, Action: "create", Phase: "outcome", Principal: principal, Session: id, Outcome: outcome})
	if err != nil {
		abandon()
		return nil, false, err
	}
	h.mu.Lock()
	h.starting--
	// A program that has already exited never enters the live set: its own
	// exit's forget may have run before this.
	if s.Info().Running {
		h.sessions[id] = s
	}
	h.byOp[key] = created{spec: fp, s: s}
	h.opOrder = append(h.opOrder, key)
	h.pruneOps()
	h.mu.Unlock()
	return s, true, nil
}

// opKey scopes an operation ID to the principal that issued it, so one
// principal's ID never returns another's session.
type opKey struct{ principal, operation string }

type created struct {
	spec string // the spec's fingerprint: a retry must ask for the same thing
	s    *TermSession
}

// maxOps bounds the remembered creations. A live session's creation is
// always kept; an exited one's is dropped oldest first beyond the bound,
// after which a retry of that operation would start a new session.
const maxOps = 256

func (h *SessionHost) pruneOps() {
	for len(h.opOrder) > maxOps {
		dropped := false
		for i, k := range h.opOrder {
			if e := h.byOp[k]; e.s != nil && !e.s.Info().Running {
				delete(h.byOp, k)
				h.opOrder = append(h.opOrder[:i], h.opOrder[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			return
		}
	}
}

func fingerprint(s SessionSpec) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%q\x00%q\x00%q\x00%q\x00%q\x00%q\x00%d\x00%d\x00%d\x00%d",
		s.Name, s.Kind, s.Command.Path, s.Command.Args, s.Command.Env, s.Command.Dir, s.Command.UID, s.Command.GID, s.Cols, s.Rows)))
	return hex.EncodeToString(sum[:])
}

// forget removes an exited session from the live set. Its creation stays
// remembered, so a retry of the operation returns it, exited.
func (h *SessionHost) forget(id string) {
	h.mu.Lock()
	delete(h.sessions, id)
	h.mu.Unlock()
}

// Terminate stops a session's program, as Stop does, and reports only
// whether its leader was confirmed reaped.
func (h *SessionHost) Terminate(operation, principal, id string) error {
	_, err := h.Stop(operation, principal, id)
	return err
}

// StopReport is what a stop read back: the program's leader, and the rest
// of its process group.
type StopReport struct {
	Leader      string `json:"leader"`       // reaped, or running (the stop is uncertain)
	Group       string `json:"group"`        // gone, or remaining: a process in the leader's group outlived the stop
	GroupKilled bool   `json:"group_killed"` // a group member outlived the hangup and was killed
}

// Stop stops a session's program: a hangup to its process group, then a
// kill if the leader still runs after the grace period. Viewing or
// controlling a session grants no stop: the caller authorises it. It waits
// on the leader's reap itself, so a slow journal never makes it uncertain,
// and it signals the group only while the group is known to exist: a leader
// reaped with its group empty leaves a pid that may be reused. Once the
// leader is reaped, startSession's own goroutine has already killed any
// member that outlived it (so pump sees EIO and the session is noticed to
// have ended even without a Stop: zeocouncil's review of #126, LOW 3); Stop
// only reads back whether that found one to kill, then confirms the group
// is actually gone. A process that left the group (its own setsid or
// setpgid) is outside what this reads.
func (h *SessionHost) Stop(operation, principal, id string) (StopReport, error) {
	s, ok := h.Session(id)
	if !ok {
		return StopReport{}, errors.New("no live session with that ID")
	}
	if err := h.record(AuditRecord{Operation: operation, Action: "terminate", Phase: "intent", Principal: principal, Session: id}); err != nil {
		return StopReport{}, ErrAudit
	}
	rep, err := StopReport{Leader: "reaped", Group: "gone"}, error(nil)
	pgid := s.cmd.Process.Pid
	signal := func(sig syscall.Signal) {
		select {
		case <-s.reaped:
			// The leader's pid is free for reuse. groupExists alone only
			// asks "does any group with this id exist", which a reused pid
			// answers yes to for someone else's unrelated session (the
			// Architect's review of #128's stack, M1): confirm the group
			// this PTY's own slave side currently has is still exactly
			// this one before ever signalling it.
			if pg, err := foregroundPgrp(s.master); err == nil && pg == pgid {
				syscall.Kill(-pgid, sig)
			}
		default:
			syscall.Kill(-pgid, sig)
		}
	}
	signal(syscall.SIGHUP)
	select {
	case <-s.reaped:
	case <-time.After(h.hangupGrace):
		signal(syscall.SIGKILL)
		select {
		case <-s.reaped:
		case <-time.After(h.fenceBound):
			rep.Leader, err = "running", ErrTerminateUncertain
		}
	}
	if err == nil {
		// s.groupSurvived was set, once, before s.reaped closed above: a
		// stop's own grace-and-kill would almost always arrive after
		// startSession's goroutine already did it, and so would almost
		// always under-report; this reads the one place that saw the
		// truth in time.
		rep.GroupKilled = s.groupSurvived
		if !s.groupGone(h.fenceBound) {
			rep.Group = "remaining"
		}
	}
	outcome := "ok"
	if err != nil || rep.Group != "gone" {
		outcome = "uncertain"
	}
	h.record(AuditRecord{Operation: operation, Action: "terminate", Phase: "outcome", Principal: principal, Session: id, Outcome: outcome,
		Reason: fmt.Sprintf("leader %s, group %s", rep.Leader, rep.Group)})
	return rep, err
}

// groupExists reports whether any process is in the process group pgid. A
// member this user can't signal still counts.
func groupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// groupGone waits, up to bound, for the session's process group to empty.
func (s *TermSession) groupGone(bound time.Duration) bool {
	for end := time.Now().Add(bound); ; time.Sleep(20 * time.Millisecond) {
		if !groupExists(s.cmd.Process.Pid) {
			return true
		}
		if !time.Now().Before(end) {
			return false
		}
	}
}

// GroupRemains reports whether a process in the session's process group is
// still there: after the leader exits, a member that ignored the hangup.
func (s *TermSession) GroupRemains() bool { return groupExists(s.cmd.Process.Pid) }

// Session returns a session by its ID.
func (h *SessionHost) Session(id string) (*TermSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	return s, ok
}

// SessionInfo is a session as listed.
type SessionInfo struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	LastOutput string     `json:"last_output,omitempty"`
	Cols       uint16     `json:"cols"`
	Rows       uint16     `json:"rows"`
	Controller string     `json:"controller,omitempty"`
	Epoch      uint64     `json:"epoch"`
	Viewers    []string   `json:"viewers"`
	Program    string     `json:"program"`       // the program's path
	Dir        string     `json:"dir,omitempty"` // its working directory
	Agent      AgentState `json:"agent"`         // adapter-reported; unknown without one
}

// List returns every session, ordered by name then ID.
func (h *SessionHost) List() []SessionInfo {
	h.mu.Lock()
	all := make([]*TermSession, 0, len(h.sessions))
	for _, s := range h.sessions {
		all = append(all, s)
	}
	h.mu.Unlock()
	out := make([]SessionInfo, 0, len(all))
	for _, s := range all {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// TermSession is one program on a PTY the host owns.
type TermSession struct {
	ID, Name, Kind string
	Program, Dir   string // the program's path and working directory: metadata, never its arguments or environment
	host           *SessionHost
	master         *os.File
	tty            string
	cmd            *exec.Cmd
	writes         chan pendingWrite
	done           chan struct{} // closed once the program has exited and its output is drained
	reaped         chan struct{} // closed once the program's leader is reaped: its pid is free from then on
	waitErr        error         // the leader's wait, set before reaped closes
	groupSurvived  bool          // a group member outlived the leader and was killed for it; set before reaped closes

	mu         sync.Mutex
	fence      *sync.Cond          // signalled when an in-flight write ends
	log        ring                // retained output, the bytes [base, end)
	term       *termstate.Terminal // the screen the output so far renders
	base, end  uint64
	exited     bool
	exitCode   int
	lastOutput time.Time
	cols, rows uint16
	viewers    map[*Client]struct{}
	controller *Client
	epoch      uint64
	inflight   uint64        // the epoch of the write now at the PTY, or 0
	agent      AgentState    // what the session's adapter last reported
	agentCh    chan struct{} // closed at the next report
}

type pendingWrite struct {
	from       *Client
	epoch      uint64
	data       []byte
	cols, rows uint16 // a resize when data is nil
}

func startSession(h *SessionHost, id string, spec SessionSpec) (*TermSession, error) {
	master, tty, err := openPTY()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(tty, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err == nil {
		if err = setSize(slave, spec.Cols, spec.Rows); err != nil {
			slave.Close()
		}
	}
	if err != nil {
		master.Close()
		return nil, err
	}
	c := spec.Command
	cmd := exec.Command(c.Path, c.Args...)
	// A nil environment is empty, never the host's own: nothing of the
	// host reaches a session that its spec doesn't name.
	cmd.Env, cmd.Dir = append([]string{}, c.Env...), c.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if c.UID >= 0 && c.GID >= 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.GID)}
	}
	closeAfterStart, err := checkExpectedSHA256(cmd, c.Path, c.ExpectedSHA256, c.PreservesInstallPath)
	if err != nil {
		slave.Close()
		master.Close()
		return nil, err
	}
	err = cmd.Start()
	if closeAfterStart != nil {
		closeAfterStart() // the child has its own copy now; the parent's would otherwise leak one fd per launch
	}
	slave.Close()
	if err != nil {
		master.Close()
		return nil, err
	}
	s := &TermSession{ID: id, Name: spec.Name, Kind: spec.Kind, host: h, master: master, tty: tty, cmd: cmd,
		writes: make(chan pendingWrite, inputQueue), done: make(chan struct{}), reaped: make(chan struct{}),
		cols: spec.Cols, rows: spec.Rows, viewers: map[*Client]struct{}{},
		term:    termstate.New(int(spec.Cols), int(spec.Rows), HistoryLines),
		Program: c.Path, Dir: c.Dir, agent: AgentState{State: "unknown"}, agentCh: make(chan struct{})}
	s.fence = sync.NewCond(&s.mu)
	// The leader is reaped as soon as it exits, not once its output is
	// drained: a background child holding the terminal open never delays it.
	// Reaping frees the leader's pid for reuse, so the group is killed
	// immediately after, in the same goroutine, before anything else runs:
	// a short, accepted hazard (the same the Architect noted on Terminate's
	// own kill), traded for a real one it closes. Without it, a straggler
	// the leader's own exit didn't take with it — a background child that
	// ignores the kernel's automatic SIGHUP to the terminal's foreground
	// group — keeps the PTY's slave side open, so pump's Read never sees
	// EIO and the session is never noticed to have ended (zeocouncil's
	// review of #126, LOW 3).
	go func() {
		s.waitErr = cmd.Wait()
		// Whether anything was actually there to kill is recorded here,
		// once, before reaped closes: the only truthful place to ask, since
		// this kill is what a stop's own after-the-fact check would race
		// against and nearly always lose (the group is already gone by the
		// time Stop gets to look).
		s.groupSurvived = groupExists(cmd.Process.Pid)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(s.reaped)
	}()
	go s.pump()
	go s.writer()
	return s, nil
}

// pump drains the PTY always, whatever the clients do: a terminal whose
// output nobody reads keeps its last process from exiting.
func (s *TermSession) pump() {
	buf := make([]byte, MaxData)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			s.output(buf[:n])
		}
		if err != nil {
			break // EIO once the terminal side has no process left
		}
	}
	<-s.reaped
	waitErr := s.waitErr
	code := 0
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		code = exit.ExitCode()
	} else if waitErr != nil {
		code = -1
	}
	s.mu.Lock()
	s.exited, s.exitCode = true, code
	s.epoch++ // nothing more is written to a terminal without a program
	for c := range s.viewers {
		c.notify(Event{Kind: EventState})
		c.closeEventsLocked() // the output is drained: nothing more will come
	}
	s.mu.Unlock()
	close(s.writes)
	s.master.Close()
	s.host.forget(s.ID) // an exited session no longer counts against the live bound
	s.host.record(AuditRecord{Operation: newID(), Action: "exit", Phase: "outcome", Session: s.ID, Outcome: "ok", Reason: fmt.Sprintf("exit %d", code)})
	close(s.done) // after the exit is journaled, so whoever sees Done finds it
}

// output retains b and hands it to every client in order. A client whose
// queue is full is marked behind and gets nothing more until it asks for a
// new snapshot: no byte is silently lost and nothing waits on a client.
func (s *TermSession) output(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	off := s.end
	s.base += uint64(s.log.write(b))
	s.end += uint64(len(b))
	s.lastOutput = s.host.now()
	// The model only watches: what it forwards and the answers it would give
	// are dropped, so the PTY and every client get exactly the program's
	// bytes, as before. A controller's own terminal answers the program's
	// queries, as it does without a session host.
	s.term.Feed(b)
	s.term.Replies()
	for c := range s.viewers {
		if c.behind {
			continue
		}
		select {
		case c.events <- Event{Kind: EventOutput, Offset: off, Data: append([]byte(nil), b...)}:
		default:
			c.behind, c.gapAt = true, off
			c.notify(Event{Kind: EventGap, Offset: off})
		}
	}
}

// writer is the only place input and window sizes reach the PTY. Each write
// is checked against the current grant epoch at the moment it is made, and
// marked in flight, so a fence can wait for it.
func (s *TermSession) writer() {
	for w := range s.writes {
		s.mu.Lock()
		ok := w.epoch == s.epoch && s.controller != nil && !s.exited
		if ok {
			s.inflight = w.epoch
			// A write waits for the program to read, however long: input
			// is never dropped on a timer. Only a fence cuts it short, and
			// it sets its deadline under this same lock, so the cut can't
			// be lost to this reset.
			s.master.SetWriteDeadline(time.Time{})
			if s.host.writeHook != nil {
				s.mu.Unlock()
				s.host.writeHook()
				s.mu.Lock()
			}
		}
		if !ok {
			w.from.outcomes.Refused++ // a revoked or stale controller's write never lands
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		var n int
		var err error
		if w.data == nil {
			err = resizeTTY(s.tty, w.cols, w.rows)
			s.mu.Lock()
			if err == nil { // only an applied size changes the recorded one, and is announced
				s.cols, s.rows = w.cols, w.rows
				s.term.Resize(int(w.cols), int(w.rows))
				for c := range s.viewers {
					c.notify(Event{Kind: EventState})
				}
			}
			s.mu.Unlock()
		} else {
			n, err = s.master.Write(w.data)
		}
		s.mu.Lock()
		o := &w.from.outcomes
		switch {
		case err == nil:
			o.Written++
		case n > 0:
			o.Partial++ // some bytes reached the program; what became of the rest is unknown, and nothing is resent
		default:
			o.Failed++
		}
		s.inflight = 0
		s.fence.Broadcast()
		s.mu.Unlock()
	}
}

// Outcomes counts what became of a client's queued writes, input and
// resizes: accepted into the queue is not the same as reaching the
// program. A partial write's remainder is unknown, and nothing is resent.
type Outcomes struct {
	Written, Partial, Failed, Refused int
}

// Outcomes returns the client's write outcomes so far.
func (c *Client) Outcomes() Outcomes {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.outcomes
}

// Info is the session's current state.
func (s *TermSession) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := SessionInfo{ID: s.ID, Name: s.Name, Kind: s.Kind, Running: !s.exited, Cols: s.cols, Rows: s.rows, Epoch: s.epoch, Viewers: []string{},
		Program: s.Program, Dir: s.Dir, Agent: s.agent}
	if s.exited {
		code := s.exitCode
		in.ExitCode = &code
	}
	if !s.lastOutput.IsZero() {
		in.LastOutput = s.lastOutput.UTC().Format(time.RFC3339Nano)
	}
	if s.controller != nil {
		in.Controller = s.controller.Principal
	}
	for c := range s.viewers {
		if c.hidden {
			continue
		}
		in.Viewers = append(in.Viewers, c.Principal)
	}
	sort.Strings(in.Viewers)
	return in
}

// Done is closed once the session's program has exited.
func (s *TermSession) Done() <-chan struct{} { return s.done }

// Snapshot is the retained output as one atomic cut: the bytes [From, To),
// after which a client's output events start exactly at To. Complete says
// the retained bytes reach back to the session's start; when they don't, a
// replay alone may not reproduce the screen, and a client must say so
// rather than claim an exact resume.
type Snapshot struct {
	Kind       string `json:"kind"` // "replay": the retained bytes, to be fed through a terminal
	From       uint64 `json:"from"`
	To         uint64 `json:"to"`
	Complete   bool   `json:"complete"`
	Cols       uint16 `json:"cols"`
	Rows       uint16 `json:"rows"`
	Data       []byte `json:"-"`
	Generation string `json:"session"`
	// Screen is the same cut as the screen model renders it (termstate's
	// ANSI snapshot, then any bytes it holds incomplete): fed to a terminal
	// of Cols by Rows, then followed by the output from To, it shows the
	// program's screen, without replaying its history. ScreenExact is false
	// when the model can't say the cut is clean; a client says so.
	Screen        []byte `json:"-"`
	ScreenFormat  string `json:"screen_format"`
	ScreenExact   bool   `json:"screen_exact"`
	ScreenLimited bool   `json:"screen_limited"` // something on the screen is outside what the model reconstructs: see termstate.Limits
}

func (s *TermSession) snapshotLocked() Snapshot {
	retained := s.log.bytes()
	screen := s.term.Snapshot()
	held, ok := s.term.Held()
	if held > len(retained) {
		held, ok = 0, false
	}
	screen = append(screen, retained[len(retained)-held:]...)
	return Snapshot{Kind: "replay", From: s.base, To: s.end, Complete: s.base == 0, Cols: s.cols, Rows: s.rows,
		Data: retained, Generation: s.ID,
		Screen: screen, ScreenFormat: termstate.SnapshotVersion, ScreenExact: ok, ScreenLimited: !s.term.Exact()}
}

// ScreenText is a session's current screen as text, for programs that read
// it rather than render it.
type ScreenText struct {
	Cols    int      `json:"cols"`
	Rows    int      `json:"rows"`
	Cursor  [2]int   `json:"cursor"` // column and row, from 0
	Title   string   `json:"title,omitempty"`
	Alt     bool     `json:"alternate_screen"`
	Lines   []string `json:"lines"`  // each row, trailing blanks trimmed
	Offset  uint64   `json:"offset"` // the output offset the screen reflects
	Running bool     `json:"running"`
}

// Screen returns the session's current screen as text.
func (s *TermSession) Screen() ScreenText {
	s.mu.Lock()
	defer s.mu.Unlock()
	cols, rows := s.term.Size()
	x, y := s.term.Cursor()
	out := ScreenText{Cols: cols, Rows: rows, Cursor: [2]int{x, y}, Title: s.term.Title(), Alt: s.term.AltScreen(),
		Lines: make([]string, rows), Offset: s.end, Running: !s.exited}
	for i := range out.Lines {
		out.Lines[i] = s.term.Row(i)
	}
	return out
}

// ScrollbackText is the lines scrolled off a session's screen, readable
// without attaching, up to HistoryLines, oldest first.
type ScrollbackText struct {
	Lines []string `json:"lines"`
	// Truncated is true when the session's own history (bounded at
	// HistoryLines) holds more than Lines returns: lines was asked for
	// fewer than the history kept.
	Truncated bool `json:"truncated"`
}

// Scrollback returns up to lines of a session's history, the most recent
// ones, oldest first; lines <= 0 or over HistoryLines returns all of it.
func (s *TermSession) Scrollback(lines int) ScrollbackText {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.term.HistoryText()
	if lines <= 0 || lines >= len(all) {
		return ScrollbackText{Lines: all}
	}
	return ScrollbackText{Lines: all[len(all)-lines:], Truncated: true}
}

// Attach adds a viewing client and returns it with its snapshot. Viewing
// grants nothing: no input, no resize.
func (s *TermSession) Attach(principal string) (*Client, Snapshot, error) {
	return s.attach(principal, false)
}

// AttachHidden is Attach for the host's own internal plumbing (a durable
// log, say): the same non-blocking fan-out as any viewer, but uncounted
// against MaxViewers, never listed in Viewers or Presence, and never
// journaled. It grants nothing a visible viewer doesn't either.
func (s *TermSession) AttachHidden(principal string) (*Client, Snapshot, error) {
	return s.attach(principal, true)
}

func (s *TermSession) attach(principal string, hidden bool) (*Client, Snapshot, error) {
	op := newID()
	if !hidden {
		if err := s.host.record(AuditRecord{Operation: op, Action: "attach", Phase: "intent", Principal: principal, Session: s.ID}); err != nil {
			return nil, Snapshot{}, ErrAudit
		}
	}
	// The outcome is journaled after the lock is released: a slow journal
	// never holds up the PTY's reader, a revocation or another client.
	s.mu.Lock()
	if !hidden {
		visible := 0
		for v := range s.viewers {
			if !v.hidden {
				visible++
			}
		}
		if visible >= MaxViewers {
			s.mu.Unlock()
			s.host.record(AuditRecord{Operation: op, Action: "attach", Phase: "outcome", Principal: principal, Session: s.ID, Outcome: "refused", Reason: "viewer bound"})
			return nil, Snapshot{}, ErrTooMany
		}
	}
	c := &Client{Principal: principal, s: s, hidden: hidden, events: make(chan Event, viewerQueue), signal: make(chan struct{}, 1)}
	if !hidden {
		for v := range s.viewers {
			v.notify(Event{Kind: EventState}) // presence: one more client
		}
	}
	s.viewers[c] = struct{}{}
	if s.exited {
		c.closeEventsLocked() // nothing will come from a program that has exited
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()
	if !hidden {
		s.host.record(AuditRecord{Operation: op, Action: "attach", Phase: "outcome", Principal: principal, Session: s.ID, Outcome: "ok"})
	}
	return c, snap, nil
}

// EventKind names a client event.
type EventKind int

const (
	EventOutput EventKind = iota // Data at Offset
	EventGap                     // the client fell behind at Offset; ask for a new snapshot
	EventState                   // the session's state, control or size changed; read Info
)

// Event is one thing a client is told, in order.
type Event struct {
	Kind   EventKind
	Offset uint64
	Data   []byte
}

// Client is one viewer of a session, and possibly its controller.
type Client struct {
	Principal string
	s         *TermSession
	hidden    bool // the host's own internal plumbing: never listed, never journaled
	events    chan Event
	signal    chan struct{} // gap and state notices, which never queue behind output
	behind    bool
	gapAt     uint64 // the offset of the first output the client missed
	closed    bool   // its Events channel is closed
	gone      bool
	epoch     uint64 // the grant epoch while this client controls
	outcomes  Outcomes
}

// Events is the client's ordered output; Signals says a gap or a state
// change is waiting (read Info, or Resnapshot after a gap).
func (c *Client) Events() <-chan Event     { return c.events }
func (c *Client) Signals() <-chan struct{} { return c.signal }

// Behind reports whether the client missed output and needs a new snapshot.
func (c *Client) Behind() bool {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.behind
}

func (c *Client) notify(Event) {
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// Resnapshot gives a client that fell behind a fresh cut, and resumes its
// output after it. Output queued before the cut is discarded, never mixed.
func (c *Client) Resnapshot() Snapshot {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
drain:
	for {
		select {
		case _, ok := <-c.events:
			if !ok {
				break drain // closed: the session ended or the client left
			}
		default:
			break drain
		}
	}
	c.behind = false
	return s.snapshotLocked()
}

// Control asks for control. With take false it is granted only when nobody
// controls the session. With take true the current controller is revoked
// first, and control is granted only once that controller's write in
// flight, if any, is confirmed ended; its queued writes never land. The
// grant's intent is journaled before anything changes, and its outcome after
// the session's lock is released. A write the take cuts short may already
// have been partly delivered: Outcomes counts it as partial, and nothing of
// it is resent. expectEpoch, if given, is compared against the session's
// current epoch under its own lock, not as a separate check beforehand: a
// caller that read the epoch, then called Control, must be refused if
// control changed in between, not only if it had already changed before the
// read (the Architect's review of #128's stack, M5).
func (c *Client) Control(take bool, reason string, expectEpoch *uint64) (uint64, error) {
	s := c.s
	action := "grant"
	if take {
		action = "take"
	}
	reason = boundReason(reason)
	op := newID()
	if err := s.host.record(AuditRecord{Operation: op, Action: action, Phase: "intent", Principal: c.Principal, Session: s.ID, Reason: reason}); err != nil {
		return 0, ErrAudit
	}
	epoch, result, why, err := c.control(take, expectEpoch)
	// journaled after the lock is released, as for Attach
	s.host.record(AuditRecord{Operation: op, Action: action, Phase: "outcome", Principal: c.Principal, Session: s.ID, Epoch: epoch, Outcome: result, Reason: why})
	return epoch, err
}

func (c *Client) control(take bool, expectEpoch *uint64) (uint64, string, string, error) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case c.gone:
		return 0, "refused", "detached", ErrNotController
	case s.exited:
		return 0, "refused", "exited", ErrSessionExited
	case expectEpoch != nil && s.epoch != *expectEpoch:
		return 0, "refused", "stale epoch", ErrEpochStale
	case s.controller == c:
		return s.epoch, "ok", "already controls", nil
	case s.controller != nil && !take:
		return 0, "refused", "controlled", ErrControlled
	}
	old := s.epoch
	s.epoch++ // from here, nothing queued under the old epoch reaches the PTY
	if prev := s.controller; prev != nil {
		prev.epoch = 0
		s.controller = nil
		prev.notify(Event{Kind: EventState})
	}
	if !s.waitFenceLocked(old) {
		return 0, "uncertain", "previous write not confirmed ended", ErrFenceUncertain
	}
	// The wait released the lock: anyone who took control meanwhile, or an
	// exit, wins, and this request is refused rather than overriding it.
	if s.controller != nil || s.exited || c.gone {
		return 0, "refused", "changed while fencing", ErrControlled
	}
	s.epoch++
	s.controller, c.epoch = c, s.epoch
	for v := range s.viewers {
		v.notify(Event{Kind: EventState})
	}
	return s.epoch, "ok", "", nil
}

// boundReason keeps a journaled reason short and printable.
func boundReason(r string) string {
	out := make([]rune, 0, len(r))
	for _, c := range r {
		if len(out) == 200 {
			break
		}
		if unicode.IsPrint(c) {
			out = append(out, c)
		}
	}
	return string(out)
}

// waitFenceLocked waits, bounded, for a write made under epoch old to end,
// cutting it short through the PTY's write deadline.
func (s *TermSession) waitFenceLocked(old uint64) bool {
	if s.inflight == 0 || s.inflight > old {
		return true
	}
	s.master.SetWriteDeadline(time.Now())
	deadline := time.Now().Add(s.host.fenceBound)
	timer := time.AfterFunc(s.host.fenceBound, func() {
		s.mu.Lock()
		s.fence.Broadcast()
		s.mu.Unlock()
	})
	defer timer.Stop()
	for s.inflight != 0 && s.inflight <= old {
		if !time.Now().Before(deadline) {
			return false
		}
		s.fence.Wait()
	}
	return true
}

// Input queues keys for the PTY under the client's grant. It is refused,
// never queued, for a client that doesn't control the session, and when the
// queue is full.
func (c *Client) Input(b []byte) error {
	if len(b) == 0 || len(b) > MaxData {
		return errors.New("input is 1 byte to one frame")
	}
	return c.queue(pendingWrite{data: append([]byte(nil), b...)})
}

// Resize sets the session's window size, under the client's grant.
func (c *Client) Resize(cols, rows uint16) error {
	if cols < 1 || cols > MaxCols || rows < 1 || rows > MaxRows {
		return fmt.Errorf("window size %dx%d is out of bounds", cols, rows)
	}
	return c.queue(pendingWrite{cols: cols, rows: rows})
}

func (c *Client) queue(w pendingWrite) error {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.gone || s.controller != c || c.epoch != s.epoch { // a detached client queues nothing, whatever it held
		return ErrNotController
	}
	if s.exited {
		return ErrSessionExited
	}
	w.epoch, w.from = c.epoch, c
	select {
	case s.writes <- w:
		return nil
	default:
		return ErrInputBusy
	}
}

// Release gives up control; the session keeps running.
func (c *Client) Release() {
	s := c.s
	s.mu.Lock()
	held := c.releaseLocked()
	s.mu.Unlock()
	if held {
		// A revocation is never held up by the journal: it is recorded when it can be.
		s.host.record(AuditRecord{Operation: newID(), Action: "release", Phase: "outcome", Principal: c.Principal, Session: s.ID, Outcome: "ok"})
	}
}

// releaseLocked revokes the client's control, if it holds it.
func (c *Client) releaseLocked() bool {
	s := c.s
	if s.controller != c {
		return false
	}
	s.epoch++
	s.controller, c.epoch = nil, 0
	for v := range s.viewers {
		v.notify(Event{Kind: EventState})
	}
	return true
}

// Detach removes the client, releasing any control it holds, in one step:
// no grant can land between the two, so a detached client never controls
// the session. It never stops the session's program.
func (c *Client) Detach() {
	s := c.s
	s.mu.Lock()
	held := c.releaseLocked()
	first := !c.gone
	if first {
		c.gone = true
		delete(s.viewers, c)
		c.closeEventsLocked()
		for v := range s.viewers {
			v.notify(Event{Kind: EventState}) // presence: one client fewer
		}
	}
	s.mu.Unlock()
	if c.hidden {
		return // the host's own internal plumbing: never journaled, like its attach
	}
	if held {
		s.host.record(AuditRecord{Operation: newID(), Action: "release", Phase: "outcome", Principal: c.Principal, Session: s.ID, Outcome: "ok"})
	}
	if first {
		s.host.record(AuditRecord{Operation: newID(), Action: "detach", Phase: "outcome", Principal: c.Principal, Session: s.ID, Outcome: "ok"})
	}
}

// ring is a session's retained output: the last RetainBytes bytes, kept
// without copying the whole buffer on every write.
type ring struct {
	buf      []byte
	start, n int
}

// write appends b and returns how many of the oldest bytes it dropped.
func (r *ring) write(b []byte) int {
	if r.buf == nil {
		r.buf = make([]byte, RetainBytes)
	}
	c := len(r.buf)
	if len(b) >= c {
		dropped := r.n + len(b) - c
		copy(r.buf, b[len(b)-c:])
		r.start, r.n = 0, c
		return dropped
	}
	e := (r.start + r.n) % c
	k := copy(r.buf[e:], b)
	copy(r.buf, b[k:])
	r.n += len(b)
	dropped := 0
	if r.n > c {
		dropped = r.n - c
		r.start, r.n = (r.start+dropped)%c, c
	}
	return dropped
}

// bytes returns the retained bytes, oldest first.
func (r *ring) bytes() []byte {
	out := make([]byte, r.n)
	if r.n == 0 {
		return out
	}
	k := copy(out, r.buf[r.start:min(r.start+r.n, len(r.buf))])
	copy(out[k:], r.buf[:r.n-k])
	return out
}

// Gap reports whether the client fell behind, and the offset of the first
// output it missed.
func (c *Client) Gap() (bool, uint64) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.behind, c.gapAt
}

// closeEventsLocked closes the client's Events channel, once: on detach,
// and when the session's program has exited and its output is drained.
func (c *Client) closeEventsLocked() {
	if !c.closed {
		c.closed = true
		close(c.events)
	}
}

// Controls reports whether this client holds the session's control now.
// Every client of one principal shares its name, so only this says which
// of them it is.
func (c *Client) Controls() bool {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.s.controller == c && c.epoch == c.s.epoch
}

// Session returns the session the client views.
func (c *Client) Session() *TermSession { return c.s }

// AgentState is what a session's adapter last reported about the program
// the session runs: whether it is working, waiting for input, or waiting on
// a permission prompt, and the last input it confirmed it accepted. It is
// only ever reported by an adapter, never read off the screen, and is
// unknown until one reports.
type AgentState struct {
	State    string `json:"state"` // unknown, running, waiting-input, permission or idle
	Detail   string `json:"detail,omitempty"`
	Seen     string `json:"seen,omitempty"` // when the adapter reported it
	Seq      uint64 `json:"seq"`            // counts reports: 0 until the first
	StateSeq uint64 `json:"state_seq"`      // the report that set State: a report of only an acceptance leaves it
	Accepted string `json:"accepted_input,omitempty"`
}

// AgentStates are the states an adapter may report.
var AgentStates = map[string]bool{"running": true, "waiting-input": true, "permission": true, "idle": true}

// ReportAgent records an adapter's report and tells every client. An empty
// state keeps the last one, so an adapter may report only an acceptance.
// It returns the StateSeq this call left in place (unchanged if state was
// empty), under the same lock that set it: council P3-2 on #137 found that
// a caller re-reading it afterward, via a separate Agent() call outside
// this lock, lets a differently-ordered concurrent report's StateSeq come
// back instead of this call's own -- returning it here, from inside the
// same critical section that assigned it, closes that gap at the source
// rather than needing the caller to re-derive it.
func (s *TermSession) ReportAgent(state, detail, accepted string) (uint64, error) {
	if state != "" && !AgentStates[state] {
		return 0, fmt.Errorf("unknown agent state %q", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if state != "" {
		s.agent.State, s.agent.StateSeq = state, s.agent.Seq+1
	}
	if accepted != "" {
		s.agent.Accepted = boundReason(accepted)
	}
	s.agent.Detail = boundReason(detail)
	s.agent.Seq++
	s.agent.Seen = s.host.now().UTC().Format(time.RFC3339Nano)
	close(s.agentCh)
	s.agentCh = make(chan struct{})
	for c := range s.viewers {
		c.notify(Event{Kind: EventState})
	}
	return s.agent.StateSeq, nil
}

// Agent returns the agent state last reported, and a channel closed at the
// next report.
func (s *TermSession) Agent() (AgentState, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent, s.agentCh
}

// ResetAgentUnknown takes the agent state back to unknown, but only if its
// StateSeq still equals ifStateSeq: a disconnected adapter's last report
// is no longer anyone's word for the program's current state (council P2:
// a stale "waiting-input" must not keep looking ready once whatever
// reported it is gone, and a stale "running"/"permission" must not keep
// looking busy either). The caller decides a reset is due, then calls
// this with the StateSeq that decision was made against; if a fresh
// report landed in between (a different connection, or the same one
// reconnecting), StateSeq has already moved on, and this is a deliberate
// no-op rather than clobbering a report newer than the disconnect that
// prompted it (council P3 on #137 -- the two are decided under different
// locks, by different goroutines, so "newer" can only be judged by
// comparing the sequence number, not by how the caller's own code reads).
// Unlike ReportAgent, this never goes through AgentStates — unknown isn't
// a state an adapter reports, only the one nobody has reported since.
func (s *TermSession) ResetAgentUnknown(ifStateSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent.State == "unknown" || s.agent.StateSeq != ifStateSeq {
		return // already unknown, or a fresher report already landed: no-op either way
	}
	// Accepted is a past fact (an input the program already confirmed),
	// not a claim about the program's current state: a disconnected
	// adapter no longer vouches for "running", but it never un-confirms
	// an input it already said was accepted (council P2 on #137 -- this
	// used to rebuild the whole struct and lose it, which could flip an
	// in-flight send's own acceptance read from "yes" back to "pending").
	s.agent.State, s.agent.Detail = "unknown", ""
	s.agent.StateSeq, s.agent.Seq = s.agent.Seq+1, s.agent.Seq+1
	s.agent.Seen = s.host.now().UTC().Format(time.RFC3339Nano)
	close(s.agentCh)
	s.agentCh = make(chan struct{})
	for c := range s.viewers {
		c.notify(Event{Kind: EventState})
	}
}

// WakeAgentWaiters wakes anyone blocked in Agent()'s channel without
// reporting anything new: a when-ready send waits there for the next
// agent report, but a readiness it claimed being given back (council P3
// on #137: a refused send's give-back didn't wake a DIFFERENT send
// already waiting) isn't a report at all, just bookkeeping that makes
// the same still-true readiness available again. Closing and remaking
// the channel, with nothing else touched, lets that waiter re-check
// immediately instead of sitting out the rest of its own wait bound.
func (s *TermSession) WakeAgentWaiters() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.agentCh)
	s.agentCh = make(chan struct{})
}

// Presence is the session's clients and controller as this client sees
// them, for its Presence frame.
func (c *Client) Presence() PresenceMsg {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	p := PresenceMsg{Session: s.ID, Viewers: []string{}, Epoch: s.epoch, You: "viewer"}
	for v := range s.viewers {
		if v.hidden {
			continue
		}
		p.Viewers = append(p.Viewers, v.Principal)
	}
	sort.Strings(p.Viewers)
	if s.controller != nil {
		p.Controller = s.controller.Principal
	}
	if s.controller == c && c.epoch == s.epoch {
		p.You = "controller"
	}
	return p
}
