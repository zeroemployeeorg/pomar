package sessiond

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

// Server is one session service, serving one root.
type Server struct {
	root        string
	serve       string // this service's ID, on every journal line it writes
	started     time.Time
	host        *seatterm.SessionHost
	j           *journal
	lock        *os.File
	unrecovered []Unrecovered
	hostname    string // where, for ls and show
	uid         int
	user        string
	logBound    int64 // a session's durable log bound; a test shrinks it on its own instance

	mu        sync.Mutex
	named     map[string]*seatterm.TermSession // each name's latest generation
	reserved  map[string]bool                  // names a create is starting
	creates   map[string]string                // principal and operation, to the name it created
	ops       map[string]*opResult             // send and stop results, by principal and operation
	opOrder   []string
	gen       map[string]int      // each name's generations so far
	gens      map[string]int      // each session's generation of its name, by session ID
	adapters  map[string]*adapter // state channels, by session ID
	readyUsed map[string]uint64   // by session ID: the adapter report a when-ready send last used
}

// opResult is what one send or stop did. A retry with the same operation
// gets it back, and nothing is done twice.
type opResult struct {
	done    bool
	fp      string // what the operation was asked to do, so a retry must ask the same thing
	Session string `json:"session"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	// A send's: how it was written, its input's ID (its operation's), the
	// grant epoch after it, and its acknowledgements, each a later stage:
	// queued in Pomar's input queue, written to the program's terminal, and
	// accepted by the program as its adapter reported (yes, pending, or
	// unsupported without a state channel).
	Mode     string `json:"mode,omitempty"`
	InputID  string `json:"input_id,omitempty"`
	Epoch    uint64 `json:"epoch,omitempty"`
	Queued   bool   `json:"queued,omitempty"`
	Written  bool   `json:"written,omitempty"`
	Accepted string `json:"accepted,omitempty"`
	// A stop's readback.
	Leader      string `json:"leader,omitempty"`
	Group       string `json:"group,omitempty"`
	GroupKilled bool   `json:"group_killed,omitempty"`
}

// opFingerprint binds an operation's dedupe record to exactly what it was
// asked to do: its target and its content. Without this, the same ID reused
// against a different session, or with different text, silently replays
// the first call's result onto the second target instead of being refused
// (the Architect's review of #128's stack, H1).
func opFingerprint(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// Bounds on remembered operations and on ended sessions still listed.
const (
	maxOps      = 512
	maxExited   = 32
	sendConfirm = 10 * time.Second // how long a send waits for its write to reach the program
	// maxSocketPath is the longest Unix socket path every platform binds.
	maxSocketPath = 103
)

var nameWord = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Open prepares root, takes its lock, opens its journal, and reconciles
// what an earlier service held.
func Open(root string) (*Server, error) {
	if err := PrepareRoot(root); err != nil {
		return nil, err
	}
	root = canonical(root)
	lock, err := lockRoot(root)
	if err != nil {
		return nil, err
	}
	s := &Server{root: root, serve: newID(), started: time.Now().UTC(), lock: lock, uid: os.Geteuid(), logBound: defaultSessionLogBytes,
		named: map[string]*seatterm.TermSession{}, reserved: map[string]bool{}, creates: map[string]string{}, ops: map[string]*opResult{},
		gen: map[string]int{}, gens: map[string]int{}, adapters: map[string]*adapter{}, readyUsed: map[string]uint64{}}
	s.hostname, _ = os.Hostname()
	if u, err := user.LookupId(strconv.Itoa(s.uid)); err == nil {
		s.user = u.Username
	}
	if s.j, err = openJournal(root, s.serve); err != nil {
		lock.Close()
		return nil, err
	}
	if err := s.j.write(Record{AuditRecord: seatterm.AuditRecord{Operation: "serve-" + s.serve, Action: "serve", Phase: "outcome", Outcome: "ok"}}); err != nil {
		s.Close()
		return nil, err
	}
	if s.unrecovered, err = s.j.reconcile(root); err != nil {
		s.Close()
		return nil, err
	}
	s.host = seatterm.NewSessionHost(s.j.audit)
	return s, nil
}

// Close releases the journal and the root's lock.
func (s *Server) Close() {
	if s.j != nil {
		s.j.f.Close()
	}
	s.lock.Close()
}

// Socket is the service's socket path.
func (s *Server) Socket() string { return filepath.Join(s.root, SocketName) }

// Serve listens on the root's socket until ctx ends, admitting only this
// user's processes, and calls ready (if set) once it listens. When it ends,
// it stops every session it holds, so each one's end is journaled rather
// than left to its terminal's hangup.
func (s *Server) Serve(ctx context.Context, ready func()) error {
	sock := s.Socket()
	if len(sock) > maxSocketPath {
		return fmt.Errorf("the socket path %s is %d bytes, beyond the %d a Unix socket allows: choose a shorter root", sock, len(sock), maxSocketPath)
	}
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and isn't a socket", sock)
		}
		os.Remove(sock) // stale: the root's lock is ours
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	if ready != nil {
		ready()
	}
	srv := &http.Server{Handler: s.mux(), ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, principalKey{}, c.(*ownerConn).principal)
		}}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err = srv.Serve(&ownerListener{Listener: ln, s: s})
	os.Remove(sock)
	s.stopAll()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) stopAll() {
	for _, in := range s.host.List() {
		if in.Running {
			s.host.Terminate("serve-stop-"+s.serve, "service", in.ID)
		}
	}
}

// ownerListener admits only connections from this user's processes, by the
// uid the kernel recorded for the peer.
type ownerListener struct {
	net.Listener
	s *Server
}

type ownerConn struct {
	net.Conn
	principal string
}

type principalKey struct{}

func (l *ownerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := localclient.PeerUID(c)
		if err != nil || int(uid) != os.Geteuid() {
			reason := "peer uid " + strconv.Itoa(int(uid))
			if err != nil {
				reason = "peer unverified"
			}
			l.s.j.write(Record{AuditRecord: seatterm.AuditRecord{Operation: newID(), Action: "connect", Phase: "outcome", Outcome: "refused", Reason: reason}})
			c.Close()
			continue
		}
		return &ownerConn{Conn: c, principal: "uid:" + strconv.Itoa(int(uid))}, nil
	}
}

func principalOf(r *http.Request) string {
	p, _ := r.Context().Value(principalKey{}).(string)
	return p
}

// Error kinds, which the CLI maps to its fixed exit codes.
const (
	KindUsage      = "usage"
	KindNotFound   = "not_found"
	KindConflict   = "conflict"
	KindExited     = "exited"
	KindUncertain  = "uncertain"
	KindInternal   = "internal"
	KindInProgress = "in_progress"
	// KindStale is a request that named a session generation or grant epoch
	// the session no longer has: read it again, then decide.
	KindStale = "stale"
	// KindUnsupported is a request the session can't honour as asked: a
	// when-ready send to a session without a state channel. It is never
	// carried out some other way.
	KindUnsupported = "unsupported"
	// KindNotReady is a when-ready send whose session's adapter didn't
	// report it ready for input in time: nothing was written.
	KindNotReady = "not_ready"
	// KindTooLarge is a send or paste whose text is over its own bound
	// (maxSendBytes for an ordinary send, maxPasteBytes for a paste):
	// nothing was queued or written.
	KindTooLarge = "too_large"
	// KindHeld is a send refused because the session has an operation whose
	// outcome is acceptance_unknown: a stop mid-effect, never resolved.
	// Pass AckUnknown (-ack-unknown) to send anyway; the hold itself is
	// never cleared by that, only acknowledged.
	KindHeld = "held"
)

// maxSendBytes bounds one send's text. send carries a line, a wake, or a
// short command, never bulk content (paste exists for that, bounded far
// higher at 8 MiB client-side): 64 KiB is generous for anything a line
// of input legitimately needs, while still catching an accidental bulk
// send before it reaches a program expecting a line.
const maxSendBytes = 64 << 10

// APIError is every refusal's body.
type APIError struct {
	Error string `json:"error"`
	Kind  string `json:"kind"`
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, code int, kind, msg string) {
	reply(w, code, APIError{Error: msg, Kind: kind})
}

// hostError maps the session host's refusals to a status and kind.
func hostError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, seatterm.ErrOperationConflict), errors.Is(err, seatterm.ErrControlled), errors.Is(err, seatterm.ErrTooMany), errors.Is(err, seatterm.ErrInputBusy):
		refuse(w, http.StatusConflict, KindConflict, err.Error())
	case errors.Is(err, seatterm.ErrCreating):
		refuse(w, http.StatusConflict, KindInProgress, err.Error())
	case errors.Is(err, seatterm.ErrSessionExited):
		refuse(w, http.StatusConflict, KindExited, err.Error())
	case errors.Is(err, seatterm.ErrFenceUncertain), errors.Is(err, seatterm.ErrTerminateUncertain):
		refuse(w, http.StatusConflict, KindUncertain, err.Error())
	case errors.Is(err, seatterm.ErrEpochStale):
		refuse(w, http.StatusConflict, KindStale, err.Error())
	default:
		refuse(w, http.StatusInternalServerError, KindInternal, err.Error())
	}
}

// CreateRequest is POST /v1/sessions. Path is absolute, and Env is the
// program's whole environment: the service adds nothing.
type CreateRequest struct {
	Operation string   `json:"operation"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Path      string   `json:"path"`
	Args      []string `json:"args"`
	Env       []string `json:"env"`
	Dir       string   `json:"dir"`
	Cols      uint16   `json:"cols"`
	Rows      uint16   `json:"rows"`
	// StateChannel gives the session a socket, named by StateEnv in its
	// environment, on which an adapter reports its agent state.
	StateChannel bool `json:"state_channel,omitempty"`
	// ExpectedSHA256, if set, pins the exact binary at Path: the launch is
	// refused if the file doesn't hash to this, checked once as the file
	// is opened for exec (seatterm.Command.ExpectedSHA256's own doc notes
	// exactly what window this does and doesn't close, by platform).
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	// PreservesInstallPath declares Path resolves some of its own
	// resources relative to its installed location: see
	// seatterm.Command.PreservesInstallPath. Combined with ExpectedSHA256,
	// the launch refuses rather than run a relocated copy.
	PreservesInstallPath bool `json:"preserves_install_path,omitempty"`
}

// SessionView is a session as ls and show give it: the session host's
// state, where and as whom it runs, its name's generation, and its outcome.
type SessionView struct {
	seatterm.SessionInfo
	Host         string `json:"host"`
	UID          int    `json:"uid"`
	User         string `json:"user,omitempty"`
	Generation   int    `json:"generation"` // 1 for a name's first session, then one more for each that took the name
	Outcome      string `json:"outcome"`    // running, or exited (with exit_code)
	StateChannel bool   `json:"state_channel"`
	// HeldOperations are this session's acceptance_unknown operations: an
	// adapter's effect whose outcome was never confirmed. Shown so an
	// attaching client sees them before typing anything.
	HeldOperations []HeldOp `json:"held_operations,omitempty"`
}

// Created is a create's reply.
type Created struct {
	SessionView
	Created bool `json:"created"` // false: this operation had already created it
}

// SendRequest is POST /v1/sessions/{name}/send.
type SendRequest struct {
	Operation string `json:"operation"`
	Text      string `json:"text"`
	Take      bool   `json:"take"`
	// Session and Epoch, when set, are the generation and grant epoch the
	// caller last read: a send to a session that has changed since is
	// refused as stale, never written.
	Session string  `json:"session,omitempty"`
	Epoch   *uint64 `json:"epoch,omitempty"`
	// WhenReady holds the text until the session's adapter reports it
	// waiting for input (a report newer than any a send used), for up to
	// ReadyWaitMS. Without a state channel it is refused as unsupported.
	WhenReady   bool `json:"when_ready,omitempty"`
	ReadyWaitMS int  `json:"ready_wait_ms,omitempty"`
	// AcceptWaitMS waits, after the write, for the adapter to report the
	// input accepted.
	AcceptWaitMS int `json:"accept_wait_ms,omitempty"`
	// Paste marks Text as a bulk payload, never a typed line: the server
	// bounds it separately from an ordinary send (maxPasteBytes, not
	// maxSendBytes) and, unless Raw, refuses any ESC byte in it before
	// wrapping it in bracketed-paste markers itself. The client never
	// wraps or scans; a caller can't construct a request that bypasses
	// this by doing its own wrapping first (the Architect's and
	// zeocouncil's BLOCKER on #128's stack: a paste containing an
	// embedded ESC[201~ ended bracketed-paste mode early, so the rest of
	// the payload arrived as TYPED input, not pasted text).
	Paste bool `json:"paste,omitempty"`
	// Raw, with Paste, sends the payload exactly as given: no ESC scan, no
	// bracketing. The caller is asking for arbitrary bytes and accepts
	// that risk explicitly; this is never the default.
	Raw bool `json:"raw,omitempty"`
	// AckUnknown lets a send proceed despite a held (acceptance_unknown)
	// operation on this session. It never resolves the hold itself — that
	// stays acceptance_unknown forever, per opJournal's own rule — it only
	// says the caller has seen it and chooses to send anyway.
	AckUnknown bool `json:"ack_unknown,omitempty"`
}

// Bounds on a send's waits.
const (
	defaultReadyWait = 30 * time.Second
	maxWait          = 10 * time.Minute
)

// maxPasteBytes bounds a paste's payload; past it, nothing is sent. Separate
// from maxSendBytes: a paste is bulk content by nature, an ordinary send is
// a line or a short command.
const maxPasteBytes = 8 << 20

// errPasteEscape is a paste refused for carrying a byte that could end
// bracketed-paste mode early and have the rest of the payload typed, not
// pasted.
var errPasteEscape = errors.New("the payload contains an ESC byte (0x1b) or a C1 CSI introducer (0x9b); a paste is never bracketed around one, since that is exactly how a payload could end bracketed-paste mode early and have the rest typed. Use -raw if you mean to send it exactly as given")

// pasteText applies Paste/Raw to payload: unless raw, it refuses any ESC
// byte (0x1b, the 7-bit CSI introducer) or 0x9b (the 8-bit C1 CSI
// introducer some terminals honour directly), and otherwise wraps the
// payload in bracketed-paste markers. The scan is a raw byte scan, not a
// rune scan: a lone 0x9b is invalid UTF-8 on its own and a rune-based scan
// (e.g. strings.IndexAny) would silently miss it, decoding it as
// RuneError instead of matching. It also refuses 0xC2 0x9B, the UTF-8
// encoding of U+009B, the shape a JSON \u009b escape actually produces on
// the wire -- confirmed directly that this is the ONLY shape reachable
// over this transport: encoding/json's Marshal replaces any invalid UTF-8
// in a Go string, including a lone 0x9b, with U+FFFD before the request
// ever leaves the client, so the raw-byte check here is defense in depth
// for a caller that reaches this function some other way (in-process, or
// a future non-JSON transport), not something the HTTP/JSON client path
// can actually trigger today. KNOWN GAP: a C1 byte can be spelled other
// ways a conforming UTF-8 decoder would still accept as U+009B's canonical
// encoding is just this one, so this is believed complete for straight
// UTF-8, but an adapter-side encoding quirk upstream of this function is
// not re-verified here. The scan runs on the payload exactly as given,
// before any wrapping of its own, so it can never be fooled by markers
// already present.
func pasteText(payload string, raw bool) (string, error) {
	if raw {
		return payload, nil
	}
	for i := 0; i < len(payload); i++ {
		if c := payload[i]; c == 0x1b || c == 0x9b {
			return "", errPasteEscape
		}
	}
	if strings.Contains(payload, "\xc2\x9b") {
		return "", errPasteEscape
	}
	return "\x1b[200~" + payload + "\x1b[201~", nil
}

// StopRequest is POST /v1/sessions/{name}/stop.
type StopRequest struct {
	Operation string `json:"operation"`
}

// Listing is GET /v1/sessions.
type Listing struct {
	Serve       string        `json:"serve"`
	Started     time.Time     `json:"started"`
	Root        string        `json:"root"`
	Host        string        `json:"host"`
	UID         int           `json:"uid"`
	User        string        `json:"user,omitempty"`
	Sessions    []SessionView `json:"sessions"`
	Unrecovered []Unrecovered `json:"unrecovered"`
	Survives    string        `json:"survives"`
}

func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		l := Listing{Serve: s.serve, Started: s.started, Root: s.root, Host: s.hostname, UID: s.uid, User: s.user,
			Sessions: s.list(), Unrecovered: s.unrecovered,
			Survives: "clients and dropped connections; not a restart of this service, and not a reboot"}
		if l.Unrecovered == nil {
			l.Unrecovered = []Unrecovered{}
		}
		reply(w, http.StatusOK, l)
	})
	mux.HandleFunc("POST /v1/sessions", s.create)
	mux.HandleFunc("GET /v1/sessions/{name}", func(w http.ResponseWriter, r *http.Request) {
		if t := s.lookup(w, r); t != nil {
			reply(w, http.StatusOK, s.view(t))
		}
	})
	mux.HandleFunc("GET /v1/sessions/{name}/screen", func(w http.ResponseWriter, r *http.Request) {
		if t := s.lookup(w, r); t != nil {
			reply(w, http.StatusOK, t.Screen())
		}
	})
	mux.HandleFunc("GET /v1/sessions/{name}/scrollback", func(w http.ResponseWriter, r *http.Request) {
		t := s.lookup(w, r)
		if t == nil {
			return
		}
		lines := 0
		if v := r.URL.Query().Get("lines"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				refuse(w, http.StatusBadRequest, KindUsage, "lines must be a positive integer")
				return
			}
			lines = n
		}
		reply(w, http.StatusOK, t.Scrollback(lines))
	})
	mux.HandleFunc("GET /v1/sessions/{name}/health", func(w http.ResponseWriter, r *http.Request) {
		if t := s.lookup(w, r); t != nil {
			reply(w, http.StatusOK, t.Health())
		}
	})
	mux.HandleFunc("GET /v1/sessions/{name}/logs", s.logs)
	mux.HandleFunc("GET /v1/sessions/by-id/{id}/logs", s.logsByID)
	mux.HandleFunc("POST /v1/sessions/{name}/send", s.send)
	mux.HandleFunc("GET /v1/sessions/{name}/input", s.inputStatus)
	mux.HandleFunc("POST /v1/sessions/{name}/stop", s.stop)
	mux.HandleFunc("POST /v1/sessions/{name}/attach", s.attach)
	return mux
}

// list is every named session's latest generation, by name.
func (s *Server) list() []SessionView {
	s.mu.Lock()
	all := make([]*seatterm.TermSession, 0, len(s.named))
	for _, t := range s.named {
		all = append(all, t)
	}
	s.mu.Unlock()
	out := make([]SessionView, 0, len(all))
	for _, t := range all {
		out = append(out, s.view(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// view is a session as ls and show give it.
func (s *Server) view(t *seatterm.TermSession) SessionView {
	v := SessionView{SessionInfo: t.Info(), Host: s.hostname, UID: s.uid, User: s.user, Outcome: "running"}
	if !v.Running {
		v.Outcome = "exited"
	}
	s.mu.Lock()
	v.Generation = s.gens[t.ID]
	a := s.adapters[t.ID]
	s.mu.Unlock()
	if a != nil {
		v.StateChannel = true
		if a.ops != nil {
			v.HeldOperations = a.ops.held()
		}
	}
	return v
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) *seatterm.TermSession {
	name := r.PathValue("name")
	s.mu.Lock()
	t := s.named[name]
	s.mu.Unlock()
	if t == nil {
		refuse(w, http.StatusNotFound, KindNotFound, "no session named "+name)
	}
	return t
}

// decodeBodyLimit bounds an ordinary request body: every route but send
// carries short, fixed-shape JSON, never a bulk payload.
const decodeBodyLimit = 1 << 20

// sendBodyLimit bounds a send's body: it alone can carry a paste, up to
// maxPasteBytes, and JSON can inflate that (each control byte becomes a
// 6-byte \u00XX escape in the worst case), so its own limit must clear that
// with real headroom, not the ordinary bound every other route gets.
const sendBodyLimit = maxPasteBytes * 8

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, decodeBodyLimit)
}

func decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		refuse(w, http.StatusBadRequest, KindUsage, "malformed request: "+err.Error())
		return false
	}
	return true
}

// decodeSend reads a send's body under sendBodyLimit (the paste-sized
// allowance), decodes it, and then -- unless the request says Paste -- also
// holds it to the ordinary decodeBodyLimit by the RAW BYTE COUNT actually
// read, not by anything the decoded fields claim. A plain send can't get
// the paste-sized allowance by leaving Paste unset and hoping Text's own
// length escapes notice: the body itself is bounded before Paste is ever
// consulted for its effect on Text.
func decodeSend(w http.ResponseWriter, r *http.Request, v *SendRequest) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, sendBodyLimit+1))
	if err != nil {
		refuse(w, http.StatusBadRequest, KindUsage, "malformed request: "+err.Error())
		return false
	}
	if int64(len(b)) > sendBodyLimit {
		refuse(w, http.StatusBadRequest, KindTooLarge, fmt.Sprintf("a send's body is bounded at %d bytes", sendBodyLimit))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		refuse(w, http.StatusBadRequest, KindUsage, "malformed request: "+err.Error())
		return false
	}
	if !v.Paste && int64(len(b)) > decodeBodyLimit {
		refuse(w, http.StatusBadRequest, KindTooLarge, fmt.Sprintf("an ordinary send's body is bounded at %d bytes; a paste (Paste: true) gets the larger allowance", decodeBodyLimit))
		return false
	}
	return true
}

// create starts a named session once per operation. A name already running
// is refused, never replaced; a name whose program has ended takes a new
// generation.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalOf(r)
	if req.Operation == "" || !nameWord.MatchString(req.Name) {
		refuse(w, http.StatusBadRequest, KindUsage, "a session needs an operation ID and a name: a lower-case word of up to 32 letters, digits and dashes")
		return
	}
	if req.Kind == "" {
		req.Kind = "program"
	}
	env := append([]string{}, req.Env...)
	for _, kv := range env {
		if strings.HasPrefix(kv, StateEnv+"=") {
			refuse(w, http.StatusBadRequest, KindUsage, StateEnv+" is the service's to set: ask for a state channel instead")
			return
		}
	}
	statePath := statePath(s.root, p, req.Operation)
	if req.StateChannel {
		env = append(env, StateEnv+"="+statePath)
	}
	spec := seatterm.SessionSpec{Name: req.Name, Kind: req.Kind, Cols: req.Cols, Rows: req.Rows,
		Command: seatterm.Command{Path: req.Path, Args: req.Args, Env: env, Dir: req.Dir, UID: -1, GID: -1, ExpectedSHA256: req.ExpectedSHA256, PreservesInstallPath: req.PreservesInstallPath}}
	key := p + "\x00" + req.Operation
	s.mu.Lock()
	name, retry := s.creates[key]
	if retry && name != req.Name {
		s.mu.Unlock()
		refuse(w, http.StatusConflict, KindConflict, "this operation ID already created a session with a different spec")
		return
	} else if !retry {
		if s.reserved[req.Name] || (s.named[req.Name] != nil && s.named[req.Name].Info().Running) {
			s.mu.Unlock()
			refuse(w, http.StatusConflict, KindConflict, "a session named "+req.Name+" is running; stop it, or choose another name")
			return
		}
		s.reserved[req.Name] = true
		s.creates[key] = req.Name
		defer func() {
			s.mu.Lock()
			delete(s.reserved, req.Name)
			s.mu.Unlock()
		}()
	}
	s.mu.Unlock()
	// A state channel listens before the program starts, so its adapter
	// finds it at once; a retry finds the one the first request opened.
	var a *adapter
	if req.StateChannel && !retry {
		var err error
		if a, err = listenState(statePath, s.root, s.j); err != nil {
			s.mu.Lock()
			delete(s.creates, key)
			s.mu.Unlock()
			refuse(w, http.StatusInternalServerError, KindInternal, "the state channel couldn't be opened: "+err.Error())
			return
		}
	}
	t, created, err := s.host.Create(req.Operation, p, spec)
	if err != nil {
		if a != nil {
			a.close()
		}
		if !retry {
			s.mu.Lock()
			delete(s.creates, key) // this request reserved the name, and nothing was made
			s.mu.Unlock()
		}
		if errors.Is(err, seatterm.ErrOperationConflict) || errors.Is(err, seatterm.ErrCreating) || errors.Is(err, seatterm.ErrTooMany) || errors.Is(err, seatterm.ErrAudit) {
			hostError(w, err)
			return
		}
		refuse(w, http.StatusBadRequest, KindUsage, err.Error())
		return
	}
	if created {
		// The host made a fresh program. s.creates is never pruned to match
		// the host's own, independently bounded memory of an operation
		// (internal/seatterm/host.go's byOp), so a retry can reach here
		// believing it's a retry (the running-name check above was
		// skipped) when the host's own record of the original operation
		// has since been evicted and it genuinely started a second one.
		// Caught here, not before: if the name is already held by a
		// DIFFERENT running session, this fresh one can never be named
		// (overwriting s.named would orphan the real holder, running but
		// unreachable by name), so it's stopped and refused instead,
		// exactly as a fresh create is refused when the name is taken
		// (the Architect's review of #128's stack, H2).
		s.mu.Lock()
		held := s.named[req.Name]
		s.mu.Unlock()
		if held != nil && held.ID != t.ID && held.Info().Running {
			s.host.Terminate(req.Operation, p, t.ID)
			refuse(w, http.StatusConflict, KindConflict, "a session named "+req.Name+" is running; stop it, or choose another name")
			return
		}
		// a.start opens a.ops (the op journal) synchronously, and must
		// finish before s.adapters publishes a below: a concurrent send
		// reads s.adapters[t.ID] without s.mu held past the lookup, and a
		// nil a.ops observed only because start hadn't run yet -- not
		// because openOpJournal actually failed -- would be a pure
		// timing race, not a real failure (layer 6's HIGH).
		if a != nil {
			a.start(t)
		}
		s.j.write(Record{AuditRecord: seatterm.AuditRecord{Operation: req.Operation, Action: "name", Phase: "outcome", Principal: p, Session: t.ID, Outcome: "ok"}, Name: req.Name})
		s.mu.Lock()
		if old := s.named[req.Name]; old != nil {
			s.forgetLocked(old.ID) // the ended generation it replaces
		}
		s.named[req.Name] = t
		s.creates[key] = req.Name // refreshed, so a later retry keeps trusting the current holder
		s.gen[req.Name]++
		s.gens[t.ID] = s.gen[req.Name]
		if a != nil {
			s.adapters[t.ID] = a
		}
		s.pruneExitedLocked()
		s.mu.Unlock()
		s.logSession(t, req.Operation)
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	reply(w, code, Created{SessionView: s.view(t), Created: created})
}

// pruneExitedLocked bounds the ended sessions still listed.
func (s *Server) pruneExitedLocked() {
	var ended []string
	for name, t := range s.named {
		if !t.Info().Running {
			ended = append(ended, name)
		}
	}
	sort.Strings(ended)
	for len(ended) > maxExited {
		s.forgetLocked(s.named[ended[0]].ID)
		delete(s.named, ended[0])
		delete(s.gen, ended[0]) // bounded: a name pruned this long ago counts its generations afresh
		ended = ended[1:]
	}
}

// begin claims an operation, or returns what it already did: a retry never
// acts twice.
// begin claims an operation under its fingerprint, or returns what it
// already did: a retry asking the same thing never acts twice. A retry
// asking something else (a different target, text or flag) is refused as
// a conflict, exactly as create already refuses a mismatched spec: the ID
// is never reused against a different operation.
func (s *Server) begin(kind, p, op, fp string) (*opResult, bool, error) {
	key := kind + "\x00" + p + "\x00" + op
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.ops[key]; ok {
		if r.fp != fp {
			return nil, false, errOpConflict
		}
		return r, false, nil
	}
	r := &opResult{fp: fp}
	s.ops[key] = r
	s.opOrder = append(s.opOrder, key)
	for len(s.opOrder) > maxOps {
		if old := s.ops[s.opOrder[0]]; old != nil && !old.done {
			break // an operation still running is never forgotten
		}
		delete(s.ops, s.opOrder[0])
		s.opOrder = s.opOrder[1:]
	}
	return r, true, nil
}

// errOpConflict is begin's refusal when an operation ID is reused against a
// different target, text or flag than its first use.
var errOpConflict = errors.New("this operation ID already did something else; it can't be reused for a different send or stop")

// drop forgets an operation that did nothing, so a retry decides afresh.
func (s *Server) drop(r *opResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.opOrder {
		if s.ops[k] == r {
			delete(s.ops, k)
			s.opOrder = append(s.opOrder[:i], s.opOrder[i+1:]...)
			return
		}
	}
}

func (s *Server) recorded(w http.ResponseWriter, r *opResult) {
	s.mu.Lock()
	done, out := r.done, *r
	s.mu.Unlock()
	if !done {
		refuse(w, http.StatusConflict, KindInProgress, "this operation is still running; ask again with the same ID")
		return
	}
	reply(w, outcomeStatus(out.Outcome), out)
}

// outcomeStatus is the status a settled outcome replays as, on every retry
// of the operation that settled it: 409 for partial or uncertain (the
// caller must not mistake a retry for a clean success it never was), 200
// for everything else (written, accepted, stopped, already-exited).
// Without this, a retry of an operation that originally settled as
// partial or uncertain silently replayed as a plain 200 -- the ONE
// situation where a retry answering differently from the original call
// would matter most (council's P1-2).
func outcomeStatus(outcome string) int {
	switch outcome {
	case "partial", "uncertain":
		return http.StatusConflict
	default:
		return http.StatusOK
	}
}

// settle records what an operation did, for every retry of it.
func (s *Server) settle(r *opResult, v opResult) opResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.done, v.fp = true, r.fp // v is built fresh by the caller; its fingerprint
	// is never part of that, so it's carried over from the reservation begin
	// made, or every later retry would compare against an empty one and be
	// refused as a conflict with its own first call.
	*r = v
	return v
}

// stale says how a session has changed since the generation or grant epoch
// a request names, or nothing when it hasn't.
func stale(t *seatterm.TermSession, req SendRequest) string {
	if req.Session != "" && req.Session != t.ID {
		return fmt.Sprintf("the session named %s is now %s, not %s: read it again", t.Name, t.ID, req.Session)
	}
	if req.Epoch != nil {
		if e := t.Info().Epoch; e != *req.Epoch {
			return fmt.Sprintf("the session's grant epoch is %d, not %d: control changed since you read it", e, *req.Epoch)
		}
	}
	return ""
}

// heldMessage names every acceptance_unknown operation a send is refused
// behind, so the caller sees exactly what it's being asked to acknowledge.
func heldMessage(held []HeldOp) string {
	ids := make([]string, len(held))
	for i, h := range held {
		ids[i] = fmt.Sprintf("%s (%ds old)", h.ID, h.AgeSecs)
	}
	return fmt.Sprintf("this session has %d operation(s) whose outcome is unknown: %s; pass -ack-unknown to send anyway (the hold itself is not cleared)",
		len(held), strings.Join(ids, ", "))
}

// waitMS is a send's wait, bounded, or def when it names none.
func waitMS(ms int, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return min(time.Duration(ms)*time.Millisecond, maxWait)
}

// awaitReady waits for the session's adapter to report it waiting for
// input in a report no send has used, and claims that report, so two sends
// never take one readiness. It gives up at the wait's end, or when the
// program exits. The claimed StateSeq is returned so the caller can give
// it back with unclaimReady if the send goes on to be refused for some
// other reason: the readiness itself was real and still holds, and must
// stay available to whatever send comes next, not be spent on one that
// never actually wrote anything (council P2).
func (s *Server) awaitReady(t *seatterm.TermSession, wait time.Duration) (uint64, bool) {
	timeout := time.After(wait)
	for {
		st, next := t.Agent()
		if st.State == "waiting-input" {
			s.mu.Lock()
			if s.readyUsed[t.ID] < st.StateSeq {
				s.readyUsed[t.ID] = st.StateSeq
				s.mu.Unlock()
				return st.StateSeq, true
			}
			s.mu.Unlock()
		}
		select {
		case <-next:
		case <-t.Done():
			return 0, false
		case <-timeout:
			return 0, false
		}
	}
}

// unclaimReady gives back a readiness claim awaitReady made, but only if
// nothing has claimed past it since: a newer report (a real state change)
// must never be un-claimed by a late give-back racing behind it. t may be
// nil in tests that exercise only the bookkeeping; a live caller always
// has the session at hand to wake anyone else already waiting on it
// (council P3 on #137: this used to update readyUsed without waking a
// different when-ready send blocked in awaitReady's own select, leaving
// it to sit out its whole wait bound instead of re-checking at once).
func (s *Server) unclaimReady(t *seatterm.TermSession, id string, seq uint64) {
	s.mu.Lock()
	gaveBack := s.readyUsed[id] == seq
	if gaveBack {
		delete(s.readyUsed, id)
	}
	s.mu.Unlock()
	if gaveBack && t != nil {
		t.WakeAgentWaiters()
	}
}

// awaitAccepted waits, up to wait, for the adapter to report input id
// accepted.
func awaitAccepted(t *seatterm.TermSession, id string, wait time.Duration) bool {
	timeout := time.After(wait)
	for {
		st, next := t.Agent()
		if st.Accepted == id {
			return true
		}
		select {
		case <-next:
		case <-t.Done():
			return false
		case <-timeout:
			return false
		}
	}
}

// send writes text to a session's program as its controller for the
// moment: it takes control only when nobody holds it (or, with take, from
// whoever does), waits for the write to reach the program, and gives
// control back. A send naming a generation or grant epoch the session no
// longer has is refused as stale. A when-ready send waits for the session's
// adapter to report it waiting for input, and is refused, never written
// raw, without a state channel. The reply tells apart the input queued,
// written to the program's terminal, and accepted by the program, which
// only its adapter can say.
func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if !decodeSend(w, r, &req) {
		return
	}
	if req.Operation == "" || req.Text == "" || req.ReadyWaitMS < 0 || req.AcceptWaitMS < 0 {
		refuse(w, http.StatusBadRequest, KindUsage, "a send needs an operation ID and its text, and waits of 0 ms or more")
		return
	}
	p := principalOf(r)
	if req.Paste {
		// Layer 7's HIGH: this used to be an unconditional maxSendBytes
		// check that ran before Paste was ever consulted, so a paste's
		// own, far larger bound (maxPasteBytes) was unreachable -- any
		// paste over 64 KiB (an ordinary send's bound, not a paste's) was
		// refused before getting anywhere near its own check below.
		if len(req.Text) > maxPasteBytes {
			refuse(w, http.StatusBadRequest, KindTooLarge, fmt.Sprintf("a paste is bounded at %d bytes; this one is %d; nothing was sent", maxPasteBytes, len(req.Text)))
			return
		}
		if req.Raw {
			// -raw sends unscanned, unbracketed bytes: a record that
			// someone knowingly chose that exists regardless of whether
			// the payload actually held an escape, matching the audit
			// every other consequential op already gets (create's name,
			// a terminate, a control grant).
			s.j.write(Record{AuditRecord: seatterm.AuditRecord{Operation: req.Operation, Action: "paste", Phase: "intent", Principal: p, Session: r.PathValue("name"), Reason: "raw paste: no ESC scan or bracketing"}})
		}
		wrapped, err := pasteText(req.Text, req.Raw)
		if err != nil {
			refuse(w, http.StatusBadRequest, KindUsage, err.Error())
			return
		}
		req.Text = wrapped
	} else if len(req.Text) > maxSendBytes {
		refuse(w, http.StatusBadRequest, KindTooLarge, fmt.Sprintf("the text is %d bytes, over the %d-byte bound; nothing was sent (use paste for bulk content)", len(req.Text), maxSendBytes))
		return
	}
	fp := opFingerprint(r.PathValue("name"), req.Text, strconv.FormatBool(req.Take))
	res, fresh, err := s.begin("send", p, req.Operation, fp)
	if err != nil {
		refuse(w, http.StatusConflict, KindConflict, err.Error())
		return
	}
	if !fresh {
		s.recorded(w, res)
		return
	}
	t := s.lookup(w, r)
	if t == nil {
		s.drop(res)
		return
	}
	if !t.Info().Running {
		s.drop(res)
		hostError(w, seatterm.ErrSessionExited)
		return
	}
	if why := stale(t, req); why != "" {
		s.drop(res)
		refuse(w, http.StatusConflict, KindStale, why)
		return
	}
	s.mu.Lock()
	a := s.adapters[t.ID]
	s.mu.Unlock()
	// heldRefusal is the held check, called twice: once here, so the
	// common case (already held) fails fast before a when-ready send's
	// long wait even starts, and again immediately before the write
	// itself (below), since that first call's answer can go stale across
	// the wait -- an adapter can report a DIFFERENT operation's outcome
	// as acceptance_unknown at any moment, including during a when-ready
	// wait of up to 10 minutes, or during the shorter window a plain
	// send's own Attach/Control can take. Only the second call's answer
	// is what actually gates the write (layer 6's TOCTOU HIGH).
	heldRefusal := func() bool {
		if a == nil || req.AckUnknown {
			return false
		}
		if a.ops == nil {
			s.drop(res)
			refuse(w, http.StatusConflict, KindHeld, "this session's operation journal isn't available, so whether an earlier operation's outcome is unknown can't be confirmed. Pass -ack-unknown to send anyway")
			return true
		}
		if held := a.ops.held(); len(held) > 0 {
			s.drop(res)
			refuse(w, http.StatusConflict, KindHeld, heldMessage(held))
			return true
		}
		return false
	}
	if heldRefusal() {
		return
	}
	mode := "raw"
	var readySeq uint64 // 0: nothing claimed, or already given back
	giveBackReady := func() {
		if readySeq != 0 {
			s.unclaimReady(t, t.ID, readySeq)
			readySeq = 0
		}
	}
	if req.WhenReady {
		mode = "when-ready"
		if a == nil {
			s.drop(res)
			refuse(w, http.StatusConflict, KindUnsupported, "this session has no state channel, so nothing can say when it is ready for input; make it with -state, or send without -when-ready to write the text raw")
			return
		}
		seq, ok := s.awaitReady(t, waitMS(req.ReadyWaitMS, defaultReadyWait))
		if !ok {
			s.drop(res) // nothing was written: a retry decides afresh
			refuse(w, http.StatusConflict, KindNotReady, "the session's adapter didn't report it waiting for input in time; nothing was written")
			return
		}
		readySeq = seq
		if why := stale(t, req); why != "" { // it may have changed while it waited
			giveBackReady() // the readiness was real; only this send is refused
			s.drop(res)
			refuse(w, http.StatusConflict, KindStale, why)
			return
		}
	}
	c, _, err := t.Attach(p)
	if err != nil {
		giveBackReady()
		s.drop(res)
		hostError(w, err)
		return
	}
	detach := sync.OnceFunc(c.Detach)
	defer detach()
	if _, err := c.Control(req.Take, "send "+req.Operation, req.Epoch); err != nil {
		giveBackReady()
		s.drop(res)
		hostError(w, err)
		return
	}
	// The authoritative check: nothing from here down may run if an
	// operation is held, so this re-checks right before the write that
	// actually reaches the program, not the stale answer from before
	// Attach/Control (or, for when-ready, before the whole ready wait).
	// detach() is already deferred above, and is itself once-only.
	if heldRefusal() {
		giveBackReady()
		return
	}
	out := opResult{Session: t.ID, Mode: mode, InputID: req.Operation, Accepted: "unsupported"}
	if a != nil {
		out.Accepted = "pending"
	}
	data, queued := []byte(req.Text), 0
	for len(data) > 0 {
		n := min(len(data), seatterm.MaxData)
		if err := c.Input(data[:n]); err != nil {
			detach()
			out.Outcome, out.Detail, out.Epoch = "partial", fmt.Sprintf("%d of %d bytes queued: %v", len(req.Text)-len(data), len(req.Text), err), t.Info().Epoch
			reply(w, http.StatusConflict, s.settle(res, out))
			return
		}
		data, queued = data[n:], queued+1
	}
	out.Queued = true
	deadline := time.Now().Add(sendConfirm)
	for {
		o := c.Outcomes()
		if o.Written+o.Partial+o.Failed+o.Refused >= queued {
			out.Written = o.Written == queued
			if !out.Written {
				out.Outcome, out.Detail = "partial", fmt.Sprintf("%+v", o)
			}
			break
		}
		if time.Now().After(deadline) {
			out.Outcome, out.Detail = "uncertain", "the program didn't read the input in time; what it got is unknown, and nothing is resent"
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	detach()
	out.Epoch = t.Info().Epoch
	if out.Outcome != "" {
		reply(w, http.StatusConflict, s.settle(res, out))
		return
	}
	out.Outcome = "written"
	if a != nil {
		a.tell(InputNotice{InputID: req.Operation, Bytes: len(req.Text), Mode: mode})
		if st, _ := t.Agent(); st.Accepted == req.Operation || (req.AcceptWaitMS > 0 && awaitAccepted(t, req.Operation, waitMS(req.AcceptWaitMS, 0))) {
			out.Accepted, out.Outcome = "yes", "accepted"
		}
	}
	reply(w, http.StatusOK, s.settle(res, out))
}

// InputStatusResult is a read of one send's current acceptance, live, as
// distinct from a retried send's frozen reply (whatever "accepted" the
// original write settled on). It never re-sends anything and never blocks.
type InputStatusResult struct {
	InputID  string `json:"input_id"`
	Accepted string `json:"accepted"` // yes, pending, unsupported or unknown
}

// inputStatus answers "has this input been accepted yet" with the
// session's current adapter state, read fresh at call time. This is
// deliberately a second, independent read from send's own reply: a send
// retried with the same ID keeps returning what it settled on when it
// ran (operation disposition, the dedup record), while this always
// reflects what the adapter says right now (input acceptance). The two
// are different questions about two different things, and are kept that
// way on purpose: begin/finish/status (see opjournal.go) tracks whether
// an agent's own tool-call operation committed; this tracks whether a
// terminal program confirmed receiving one particular write.
func (s *Server) inputStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		refuse(w, http.StatusBadRequest, KindUsage, "input needs ?id=, the input ID a send used")
		return
	}
	t := s.lookup(w, r)
	if t == nil {
		return
	}
	p := principalOf(r)
	s.mu.Lock()
	rec, known := s.ops["send\x00"+p+"\x00"+id]
	a := s.adapters[t.ID]
	s.mu.Unlock()
	out := InputStatusResult{InputID: id, Accepted: "unknown"}
	switch {
	case a == nil:
		// No state channel at all: this session can never report
		// acceptance for any input, known or not, so that's the answer
		// regardless of whether this exact ID was ever sent.
		out.Accepted = "unsupported"
	case known && rec != nil && rec.Session == t.ID:
		// rec is keyed by kind+principal+op alone -- the SAME id, sent by
		// the SAME principal to a DIFFERENT session (or to an earlier
		// generation of this same name), is a different rec.Session than
		// this lookup's t.ID. Without this check, an id sent only to
		// session aa read as pending/yes on session bb too, as long as
		// the same principal had used that id anywhere (layer 7's
		// MEDIUM). An id genuinely unknown to this exact session now
		// falls through to "unknown", never borrowing another session's
		// answer.
		if st, _ := t.Agent(); st.Accepted == id {
			out.Accepted = "yes"
		} else {
			out.Accepted = "pending"
		}
	}
	reply(w, http.StatusOK, out)
}

// stop ends a session's program, with the authority of the service's own
// user (the only one its socket admits), and reads back the leader's reap
// and whether any process remains in its process group. It acts on the
// generation the name had when the operation began, never on one that took
// the name since.
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	var req StopRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Operation == "" {
		refuse(w, http.StatusBadRequest, KindUsage, "a stop needs an operation ID")
		return
	}
	p := principalOf(r)
	fp := opFingerprint(r.PathValue("name"))
	res, fresh, err := s.begin("stop", p, req.Operation, fp)
	if err != nil {
		refuse(w, http.StatusConflict, KindConflict, err.Error())
		return
	}
	if !fresh {
		s.recorded(w, res)
		return
	}
	t := s.lookup(w, r)
	if t == nil {
		s.drop(res)
		return
	}
	// A group left behind is never reported as a clean 200, whether the
	// session had already exited or this stop just ended it: a straggler
	// still holding the PTY is exactly what Terminate's own audit already
	// calls uncertain (internal/seatterm/host.go:306), and the caller must
	// see that, not a false "stopped" (the Architect's review of #128's
	// stack, H3).
	exited := func() {
		out := opResult{Session: t.ID, Outcome: "already-exited", Leader: "reaped", Group: "gone"}
		code := http.StatusOK
		if t.GroupRemains() {
			out.Outcome, out.Group, out.Detail, code = "uncertain", "remaining", "the program had already exited, but a process in its group remains", http.StatusConflict
		}
		out = s.settle(res, out)
		// The full result (session, leader, group), not only an error
		// string, so a caller sees exactly what a retry's 409 replay also
		// gives it (recorded/outcomeStatus): an uncertain outcome is still
		// an operation result, not merely a refusal.
		reply(w, code, out)
	}
	if !t.Info().Running {
		exited()
		return
	}
	rep, err := s.host.Stop(req.Operation, p, t.ID)
	if err != nil {
		if !t.Info().Running {
			exited()
			return
		}
		s.settle(res, opResult{Session: t.ID, Outcome: "uncertain", Detail: err.Error(), Leader: rep.Leader, Group: rep.Group, GroupKilled: rep.GroupKilled})
		hostError(w, err)
		return
	}
	if rep.Leader != "reaped" || rep.Group != "gone" {
		out := s.settle(res, opResult{Session: t.ID, Outcome: "uncertain", Leader: rep.Leader, Group: rep.Group, GroupKilled: rep.GroupKilled,
			Detail: "the leader or a process in its group could not be confirmed gone"})
		// The full result, not only an error string: see exited's own
		// comment above.
		reply(w, http.StatusConflict, out)
		return
	}
	reply(w, http.StatusOK, s.settle(res, opResult{Session: t.ID, Outcome: "stopped", Leader: rep.Leader, Group: rep.Group, GroupKilled: rep.GroupKilled}))
}

// forgetLocked drops what the service keeps for a session no name lists.
func (s *Server) forgetLocked(id string) {
	delete(s.gens, id)
	delete(s.adapters, id)
	delete(s.readyUsed, id)
}
