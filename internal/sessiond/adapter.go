package sessiond

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

// StateEnv names, in a session's environment, the socket its adapter
// reports agent state on, when the session was made with a state channel.
const StateEnv = "POMAR_SESSION_STATE"

// maxStateLine bounds one line an adapter writes.
const maxStateLine = 4096

// AdapterReport is one line an adapter writes on the state channel: a
// state, an input it accepted, or both. An adapter is any program the
// session runs, or that its program starts; Pomar knows nothing of which
// agent it speaks for.
type AdapterReport struct {
	State    string `json:"state,omitempty"`    // running, waiting-input, permission or idle
	Detail   string `json:"detail,omitempty"`   // a short label, shown beside the state
	Accepted string `json:"accepted,omitempty"` // an input_id the program confirmed it accepted
}

// InputNotice is the line the service writes to the adapter once an input
// reached the program, so it can confirm it by input_id.
type InputNotice struct {
	InputID string `json:"input_id"`
	Bytes   int    `json:"bytes"`
	Mode    string `json:"mode"` // raw or when-ready
}

// adapter is one session's state channel: a private socket in the root,
// served for as long as the session runs. The latest connection is the
// adapter's; each line it writes is a report.
type adapter struct {
	path string
	ln   net.Listener
	t    *seatterm.TermSession
	j    *journal
	root string
	ops  *opJournal // opened once the session's ID is known, in start

	mu        sync.Mutex
	conn      net.Conn
	stateConn net.Conn // the connection whose report last set the current agent state, if any
	stateSeq  uint64   // the StateSeq that report produced; the fence passed to ResetAgentUnknown
}

// statePath is a create's state socket: the same for every retry of one
// operation, so a retry asks for the same session.
func statePath(root, principal, op string) string {
	h := sha256.Sum256([]byte(principal + "\x00" + op))
	return filepath.Join(root, "a-"+hex.EncodeToString(h[:8])+".sock")
}

// listenState opens a state channel at path, admitting only this user.
// root names where its op journal is opened, once start knows the session's
// ID.
func listenState(path, root string, j *journal) (*adapter, error) {
	if len(path) > maxSocketPath {
		return nil, errors.New("the state socket's path is too long for a Unix socket: choose a shorter root")
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, errors.New(path + " exists and isn't a socket")
		}
		os.Remove(path) // a stale channel: the root's lock is ours
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &adapter{path: path, ln: ln, j: j, root: root}, nil
}

// start binds the channel to its session, then serves it: a connection made
// before, while the program starts, waits in the backlog. It closes when
// the session's program has exited. Its op journal opens here too, keyed
// by the session's NAME, not its (random, per-generation) ID: a name is
// what a caller reconnects to, and a held (acceptance_unknown) operation
// has to survive the program it was begun against dying and the name
// being taken up by a freshly re-created session, or the hold is silently
// forgotten the moment that happens (layer 6's HIGH). A failure to open
// the journal never blocks the session itself, only the begin/finish/
// status calls an adapter makes, which then answer a plain error.
func (a *adapter) start(t *seatterm.TermSession) {
	a.t = t
	a.ops, _ = openOpJournal(a.root, t.Name)
	go a.serve()
	go func() {
		<-t.Done()
		a.close()
	}()
}

func (a *adapter) close() {
	a.ln.Close()
	os.Remove(a.path)
	if a.ops != nil {
		// The program just exited: nothing will ever call finish for a
		// still-pending operation now, since only the adapter it ran
		// could have, and it's gone. Converting here, not only on a
		// later reopen, is what makes this show up as held immediately,
		// including for a session that stays live and is simply
		// re-created under the same name right after (layer 6's HIGH:
		// "pending never becomes acceptance_unknown").
		a.ops.convertPending()
		a.ops.close()
	}
	a.mu.Lock()
	if a.conn != nil {
		a.conn.Close()
	}
	a.mu.Unlock()
}

func (a *adapter) serve() {
	for {
		c, err := a.ln.Accept()
		if err != nil {
			return
		}
		if uid, err := localclient.PeerUID(c); err != nil || int(uid) != os.Geteuid() {
			a.j.write(Record{AuditRecord: seatterm.AuditRecord{Operation: newID(), Action: "adapter", Phase: "outcome", Session: a.t.ID, Outcome: "refused", Reason: "peer not this user"}})
			c.Close()
			continue
		}
		a.mu.Lock()
		if a.conn != nil {
			a.conn.Close() // the latest connection is the adapter's
		}
		a.conn = c
		a.mu.Unlock()
		go a.read(c)
	}
}

// disconnected takes the agent state back to unknown when the exact
// connection whose report set the CURRENT state goes away ON ITS OWN --
// never merely because some connection closed, and never merely because
// it was superseded. Two council findings on #137, both against the
// first version of this function:
//
// P2 (round one): an op-only connection, such as one of the hook scripts
// in docs/sessions.md that only ever calls begin/finish and never sends
// an AdapterReport, must leave the state alone when it disconnects; it
// was never speaking for state. Fixed by comparing c against stateConn,
// not conn.
//
// P2-1 (round two): that alone still reset state on every new
// connection's ARRIVAL, not just a reporter's own exit. serve() closes
// whatever was the single latest connection the moment any new one is
// accepted -- including a short-lived op-only hook call -- so a
// long-lived real reporter got forcibly evicted by each such call, its
// read loop ended here, and this function saw c still equal to stateConn
// and reset anyway. Only the moment moved (exit -> arrival); every hook
// call still wiped real state. Fixed: a reset now also requires c to
// still be a.conn -- i.e. that this disconnect is this connection's own
// genuine end (error, EOF, or the program closing it), not merely having
// been superseded by a newer connection, which serve's own Close already
// reassigned a.conn away from before this call could observe it.
//
// The compare-and-clear of conn and stateConn happens under the same
// lock acquisition that decides whether to reset, but the reset itself
// still runs after releasing it (ResetAgentUnknown takes the session's
// own lock, a different one) -- a fresh report could land in that exact
// gap. ResetAgentUnknown is fenced against stateSeq for exactly that
// reason (council P3): a report that landed after this decision was made
// has already moved StateSeq past what's passed here, so it's a no-op,
// never a clobber.
//
// A known limit of this same design (council P3-1 on #137, round three):
// once a reporter has been superseded, c can never again equal a.conn for
// IT -- the eviction already moved a.conn elsewhere -- so its own later
// exit can no longer trigger a reset by this path either. If it never
// reconnects and reports again (it dies, or its next connection only
// sends an acceptance), the state it last reported keeps showing, stale,
// until a fresh report or the session's own end. docs/sessions.md's
// contract section says so plainly, so an adapter author knows to
// reconnect and re-report after being superseded, rather than assume an
// earlier connection still speaks for anything.
func (a *adapter) disconnected(c net.Conn) {
	a.mu.Lock()
	stillLatest := a.conn == c // false if serve() already superseded c with a newer connection
	if stillLatest {
		a.conn = nil
	}
	wasStateConn, seq := a.stateConn == c, a.stateSeq
	resetDue := stillLatest && wasStateConn
	if resetDue {
		a.stateConn = nil
	}
	a.mu.Unlock()
	if resetDue {
		a.t.ResetAgentUnknown(seq)
	}
}

// opRequest is an adapter's begin, finish or status call, on the same
// socket and line protocol as AdapterReport, distinguished by carrying
// "op". Everything else on the channel is a plain state report.
type opRequest struct {
	Op     string `json:"op"`
	ID     string `json:"id"`
	Digest string `json:"digest,omitempty"`
	Known  *bool  `json:"known,omitempty"`
}

var opIDWord = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`) // one printable-ASCII token, bounded

// read takes an adapter's reports and op calls, one JSON line each. A
// malformed or over-long line is answered with an error line and ends the
// connection.
func (a *adapter) read(c net.Conn) {
	defer c.Close()
	defer a.disconnected(c)
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 512), maxStateLine)
	for sc.Scan() {
		var peek struct {
			Op string `json:"op"`
		}
		if json.Unmarshal(sc.Bytes(), &peek) == nil && peek.Op != "" {
			if err := a.op(c, sc.Bytes()); err != nil {
				a.reply(c, map[string]string{"error": err.Error()})
				return
			}
			continue
		}
		var r AdapterReport
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		err := dec.Decode(&r)
		if err == nil && r.State == "" && r.Accepted == "" {
			err = errors.New("a report carries a state, an accepted input_id, or both")
		}
		var seq uint64
		if err == nil {
			seq, err = a.t.ReportAgent(r.State, r.Detail, r.Accepted)
		}
		if err != nil {
			a.reply(c, map[string]string{"error": err.Error()})
			return
		}
		if r.State != "" {
			// This connection set the current state: record it, and the
			// exact StateSeq that report produced, so a later disconnect
			// resets only if this is still the freshest word. An
			// accepted-only report does NOT move stateConn: it never
			// claimed to speak for state, so it must not take over
			// "owning" a reset from whichever connection actually set
			// the state that's still current (op-only connections, which
			// never reach here at all, likewise never touch it).
			//
			// seq is ReportAgent's OWN return value, taken from inside the
			// same lock that assigned it, not a separate a.t.Agent() read
			// afterward. Council P3-2 (the P3-A remainder): a separate
			// read could still lose a tie -- a superseded connection R and
			// its replacement R' both reporting around the same time could
			// each read back R''s seq (whichever of R/R' took a.t's lock
			// last), letting R win stateConn while actually holding R''s
			// seq, so R''s later genuine exit would wrongly look stale and
			// never reset. Using ReportAgent's own return value ties each
			// connection to the exact seq ITS OWN call produced, so this
			// can't happen regardless of read ordering afterward.
			a.mu.Lock()
			if seq > a.stateSeq {
				a.stateConn, a.stateSeq = c, seq
			}
			a.mu.Unlock()
		}
	}
	if sc.Err() != nil {
		a.reply(c, map[string]string{"error": "line too long"})
	}
}

// op answers a begin, finish or status call, replying on success itself;
// an error it returns is what the caller reports and ends the connection
// on, matching how a malformed AdapterReport is handled.
func (a *adapter) op(c net.Conn, line []byte) error {
	var req opRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return err
	}
	if !opIDWord.MatchString(req.ID) {
		return errors.New("op needs an id: one printable token, at most 128 bytes")
	}
	if a.ops == nil {
		return errors.New("this session's operation journal couldn't be opened; begin/finish/status aren't available")
	}
	switch req.Op {
	case "begin":
		first, err := a.ops.begin(req.ID, req.Digest)
		if err != nil {
			return err
		}
		a.reply(c, map[string]bool{"first": first})
	case "finish":
		if req.Known == nil {
			return errors.New("finish needs known: true or false")
		}
		if err := a.ops.finish(req.ID, *req.Known); err != nil {
			return err
		}
		a.reply(c, map[string]bool{"ok": true})
	case "status":
		a.reply(c, map[string]string{"disposition": a.ops.status(req.ID)})
	default:
		return errors.New("op must be begin, finish or status")
	}
	return nil
}

func (a *adapter) reply(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	c.SetWriteDeadline(time.Now().Add(time.Second))
	c.Write(append(b, '\n'))
}

// tell writes an input notice to the adapter, if one is connected. It
// never waits on an adapter that doesn't read.
func (a *adapter) tell(n InputNotice) {
	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	if c != nil {
		a.reply(c, n)
	}
}
