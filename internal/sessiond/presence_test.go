package sessiond

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

// watcher records what a pump hands its hooks.
type watcher struct {
	mu       sync.Mutex
	presence []seatterm.PresenceMsg
	states   []seatterm.AgentState
	controls []seatterm.ControlMsg
}

func (w *watcher) hooks(req <-chan seatterm.ControlMsg) Hooks {
	return Hooks{
		Presence: func(p seatterm.PresenceMsg) { w.mu.Lock(); w.presence = append(w.presence, p); w.mu.Unlock() },
		State:    func(s seatterm.AgentState) { w.mu.Lock(); w.states = append(w.states, s); w.mu.Unlock() },
		Control:  func(c seatterm.ControlMsg) { w.mu.Lock(); w.controls = append(w.controls, c); w.mu.Unlock() },
		Requests: req,
	}
}

// until waits for ok to hold over what w recorded.
func (w *watcher) until(t *testing.T, what string, ok func(w *watcher) bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		w.mu.Lock()
		done := ok(w)
		w.mu.Unlock()
		if done {
			return
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	t.Fatalf("never saw %s: presence %+v, states %+v, controls %+v", what, w.presence, w.states, w.controls)
}

func (w *watcher) last() seatterm.PresenceMsg {
	if len(w.presence) == 0 {
		return seatterm.PresenceMsg{}
	}
	return w.presence[len(w.presence)-1]
}

func (w *watcher) saw(event string, epoch uint64) bool {
	for _, c := range w.controls {
		if c.Event == event && (epoch == 0 || c.Epoch == epoch) {
			return true
		}
	}
	return false
}

// attachWith attaches a client with hooks; detach ends it.
func attachWith(t *testing.T, c *Client, name string, view, take bool, w *watcher, req chan seatterm.ControlMsg) (detach func()) {
	t.Helper()
	st, err := c.Attach(name, view, take)
	if err != nil {
		t.Fatal(err)
	}
	in, inW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		PumpWith(st, in, io.Discard, 60, 8, nil, w.hooks(req))
	}()
	return func() {
		inW.Write([]byte{DetachKey})
		<-done
	}
}

// Every client is told who is attached and who controls, on attach and on
// each change: another client's attach, its take, and its detach. A take
// is answered with its epoch, and the client it was taken from is told.
func TestPresenceAndControlReachEveryClient(t *testing.T) {
	_, c := serve(t, root(t))
	if err := c.Do("POST", "/v1/sessions", shell("op-p", "work", "sleep 60"), nil); err != nil {
		t.Fatal(err)
	}
	a := &watcher{}
	detachA := attachWith(t, c, "work", false, false, a, nil)
	defer detachA()
	a.until(t, "A controlling alone", func(w *watcher) bool {
		p := w.last()
		return p.You == "controller" && len(p.Viewers) == 1 && w.saw("granted", 0)
	})
	b, breq := &watcher{}, make(chan seatterm.ControlMsg, 1)
	detachB := attachWith(t, c, "work", false, false, b, breq)
	b.until(t, "B viewing, refused control", func(w *watcher) bool {
		return w.last().You == "viewer" && len(w.last().Viewers) == 2 && w.saw("refused", 0)
	})
	a.until(t, "A told of B", func(w *watcher) bool { return len(w.last().Viewers) == 2 })

	var before SessionView
	c.Do("GET", "/v1/sessions/work", nil, &before)
	breq <- seatterm.ControlMsg{Op: "take"}
	var granted uint64
	b.until(t, "B granted with an epoch", func(w *watcher) bool {
		for _, m := range w.controls {
			if m.Event == "granted" && m.Epoch > before.Epoch {
				granted = m.Epoch
				return true
			}
		}
		return false
	})
	a.until(t, "A told it was taken", func(w *watcher) bool { return w.saw("taken", granted) && w.last().You == "viewer" })
	b.until(t, "B controlling", func(w *watcher) bool { return w.last().You == "controller" })
	var after SessionView
	if c.Do("GET", "/v1/sessions/work", nil, &after); after.Epoch != granted {
		t.Fatalf("the session's epoch is %d, the grant's %d", after.Epoch, granted)
	}

	breq <- seatterm.ControlMsg{Op: "release"}
	b.until(t, "B released", func(w *watcher) bool {
		return w.saw("released", 0) && w.last().You == "viewer" && w.last().Controller == ""
	})
	b.mu.Lock()
	if b.saw("taken", 0) {
		t.Fatal("a release was reported as taken")
	}
	b.mu.Unlock()
	detachB()
	a.until(t, "A told B left", func(w *watcher) bool { return len(w.last().Viewers) == 1 })
}

// A send naming a generation or epoch the session no longer has is refused
// as stale, and writes nothing. A send's acknowledgements say how far it
// got: queued, written, and, with no adapter, acceptance unsupported. A
// when-ready send without a state channel is refused, never written raw.
func TestSendsAreFencedAndAcknowledged(t *testing.T) {
	_, c := serve(t, root(t))
	if err := c.Do("POST", "/v1/sessions", shell("op-s", "work", `while read l; do printf 'got:%s\n' "$l"; done`), nil); err != nil {
		t.Fatal(err)
	}
	var v SessionView
	if err := c.Do("GET", "/v1/sessions/work", nil, &v); err != nil {
		t.Fatal(err)
	}
	wrong := v.Epoch + 5
	if err := c.Do("POST", "/v1/sessions/work/send", SendRequest{Operation: "s-1", Text: "no\r", Epoch: &wrong}, nil); kindOf(err) != KindStale {
		t.Fatalf("a stale epoch: %v", err)
	}
	if err := c.Do("POST", "/v1/sessions/work/send", SendRequest{Operation: "s-2", Text: "no\r", Session: "an-older-one"}, nil); kindOf(err) != KindStale {
		t.Fatalf("a stale generation: %v", err)
	}
	if err := c.Do("POST", "/v1/sessions/work/send", SendRequest{Operation: "s-3", Text: "no\r", WhenReady: true}, nil); kindOf(err) != KindUnsupported {
		t.Fatalf("when-ready without a state channel: %v", err)
	}
	var out OpResult
	if err := c.Do("POST", "/v1/sessions/work/send", SendRequest{Operation: "s-4", Text: "yes\r", Epoch: &v.Epoch, Session: v.ID}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "written" || out.Mode != "raw" || !out.Queued || !out.Written || out.Accepted != "unsupported" || out.InputID != "s-4" || out.Epoch <= v.Epoch {
		t.Fatalf("acks %+v", out)
	}
	scr := strings.Join(waitScreen(t, c, "work", "got:yes").Lines, "\n")
	if strings.Contains(scr, "got:no") {
		t.Fatalf("a refused send was written: %q", scr)
	}
	// The refused operations did nothing, so their IDs decide afresh.
	if err := c.Do("POST", "/v1/sessions/work/send", SendRequest{Operation: "s-1", Text: "again\r"}, nil); err != nil {
		t.Fatalf("a refused operation's retry: %v", err)
	}
}

// fakeAdapter is an adapter on a session's state channel.
type fakeAdapter struct {
	conn net.Conn
	rd   *bufio.Reader
}

func (f *fakeAdapter) report(t *testing.T, r AdapterReport) {
	t.Helper()
	b, _ := json.Marshal(r)
	if _, err := f.conn.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeAdapter) notice(t *testing.T) InputNotice {
	t.Helper()
	f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := f.rd.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var n InputNotice
	if json.Unmarshal(line, &n) != nil || n.InputID == "" {
		t.Fatalf("not an input notice: %s", line)
	}
	return n
}

// A session with a state channel takes its adapter's reports: the state is
// shown to clients and in show, never read off the screen. A when-ready
// send is written only once the adapter reports the program waiting for
// input, uses that report once, and is accepted when the adapter says so.
func TestAnAdaptersStateGatesWhenReadySends(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-a", "agent", `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
	req.Cols, req.StateChannel = 120, true
	var made Created
	if err := c.Do("POST", "/v1/sessions", req, &made); err != nil || !made.StateChannel || made.Agent.State != "unknown" {
		t.Fatalf("create: %v %+v", err, made)
	}
	path := strings.TrimSpace(waitScreen(t, c, "agent", ".sock").Lines[0])
	if fi, err := os.Lstat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the state channel %s: %v", path, err)
	}
	w := &watcher{}
	detach := attachWith(t, c, "agent", true, false, w, nil)
	defer detach()

	type result struct {
		out OpResult
		err error
	}
	sent := make(chan result, 1)
	go func() {
		var out OpResult
		err := c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "w-1", Text: "first\r", WhenReady: true, ReadyWaitMS: 8000}, &out)
		sent <- result{out, err}
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ad := &fakeAdapter{conn: conn, rd: bufio.NewReader(conn)}
	ad.report(t, AdapterReport{State: "running"})
	w.until(t, "the running state", func(w *watcher) bool { return len(w.states) > 0 && w.states[len(w.states)-1].State == "running" })
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(strings.Join(screenOf(t, c, "agent").Lines, "\n"), "got:first") {
		t.Fatal("a when-ready send was written while the program was running")
	}
	select {
	case r := <-sent:
		t.Fatalf("the send ended before the program was ready: %+v %v", r.out, r.err)
	default:
	}
	ad.report(t, AdapterReport{State: "waiting-input", Detail: "prompt"})
	r := <-sent
	if r.err != nil || r.out.Outcome != "written" || r.out.Mode != "when-ready" || r.out.Accepted != "pending" {
		t.Fatalf("when-ready: %v %+v", r.err, r.out)
	}
	waitScreen(t, c, "agent", "got:first")
	if n := ad.notice(t); n.InputID != "w-1" || n.Mode != "when-ready" || n.Bytes != len("first\r") {
		t.Fatalf("notice %+v", n)
	}
	ad.report(t, AdapterReport{Accepted: "w-1"})
	var v SessionView
	// Detail is `json:",omitempty"`: once the server-side value genuinely
	// clears to "", the response omits the key entirely, and decoding into
	// an already-populated v would silently leave its PRIOR non-empty
	// value in place (encoding/json only sets fields present in the
	// payload) -- exactly the stale "Detail:prompt" false failure this
	// loop produced intermittently. Each poll must decode into a fresh,
	// zero-valued struct so a field that legitimately went empty is seen
	// as empty, not as whatever it last was.
	for end := time.Now().Add(5 * time.Second); v.Agent.Accepted != "w-1" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		v = SessionView{}
		c.Do("GET", "/v1/sessions/agent", nil, &v)
	}
	if v.Agent.Accepted != "w-1" || v.Agent.State != "waiting-input" || v.Agent.Detail != "" || v.Agent.Seq != 3 {
		t.Fatalf("agent %+v", v.Agent)
	}

	// The readiness was used: another when-ready send waits for a new one.
	if err := c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "w-2", Text: "second\r", WhenReady: true, ReadyWaitMS: 300}, nil); kindOf(err) != KindNotReady {
		t.Fatalf("a used readiness: %v", err)
	}
	// With an acceptance wait, the adapter's confirmation completes the send.
	go func() {
		ad.report(t, AdapterReport{State: "waiting-input"})
		n := ad.notice(t)
		ad.report(t, AdapterReport{State: "running", Accepted: n.InputID})
	}()
	var out OpResult
	if err := c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "w-3", Text: "third\r", WhenReady: true, ReadyWaitMS: 5000, AcceptWaitMS: 5000}, &out); err != nil || out.Outcome != "accepted" || out.Accepted != "yes" {
		t.Fatalf("accepted: %v %+v", err, out)
	}
	// A malformed report is answered, and ends the adapter's connection.
	ad.report(t, AdapterReport{State: "dancing"})
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := ad.rd.ReadBytes('\n')
	if !strings.Contains(string(line), "unknown agent state") {
		t.Fatalf("a bad state was taken: %s", line)
	}
}

// A stop reads back the leader's reap and the rest of its process group: a
// member that ignored the hangup is killed after the grace, and said so.
func TestAStopReadsBackTheProcessGroup(t *testing.T) {
	_, c := serve(t, root(t))
	if err := c.Do("POST", "/v1/sessions", shell("op-g", "plain", "sleep 60"), nil); err != nil {
		t.Fatal(err)
	}
	var out OpResult
	if err := c.Do("POST", "/v1/sessions/plain/stop", StopRequest{Operation: "k-1"}, &out); err != nil || out.Leader != "reaped" || out.Group != "gone" || out.GroupKilled {
		t.Fatalf("a plain stop: %v %+v", err, out)
	}
	if err := c.Do("POST", "/v1/sessions", shell("op-h", "stubborn", `(trap '' HUP; exec sleep 60) & sleep 60`), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the child has set its trap
	if err := c.Do("POST", "/v1/sessions/stubborn/stop", StopRequest{Operation: "k-2"}, &out); err != nil || out.Outcome != "stopped" || out.Leader != "reaped" || out.Group != "gone" || !out.GroupKilled {
		t.Fatalf("a stop past a member that ignores the hangup: %v %+v", err, out)
	}
	var v SessionView
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if c.Do("GET", "/v1/sessions/stubborn", nil, &v); !v.Running {
			break
		}
	}
	if v.Running || v.Outcome != "exited" {
		t.Fatalf("after the stop: %+v", v)
	}
}

// ls and show say where and as whom a session runs, its generation and
// outcome, its program and directory, its epoch, viewers and agent state,
// and never its environment.
func TestListingsCarryIdentityAndOutcome(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-l", "work", "sleep 60")
	req.Env = append(req.Env, "GREETING=a-value-never-shown")
	req.Dir = "/tmp"
	if err := c.Do("POST", "/v1/sessions", req, nil); err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err := c.Do("GET", "/v1/sessions/work", nil, &raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "a-value-never-shown") || strings.Contains(string(raw), "GREETING") {
		t.Fatalf("show carries the environment: %s", raw)
	}
	var v map[string]any
	json.Unmarshal(raw, &v)
	host, _ := os.Hostname()
	for k, want := range map[string]any{"host": host, "uid": float64(os.Geteuid()), "generation": float64(1), "outcome": "running",
		"program": "/bin/sh", "dir": "/tmp", "name": "work", "state_channel": false} {
		if v[k] != want {
			t.Fatalf("show's %s is %v, want %v: %s", k, v[k], want, raw)
		}
	}
	for _, k := range []string{"id", "epoch", "viewers", "agent", "user"} {
		if _, ok := v[k]; !ok {
			t.Fatalf("show has no %s: %s", k, raw)
		}
	}
	if v["agent"].(map[string]any)["state"] != "unknown" {
		t.Fatalf("an agent state without an adapter: %v", v["agent"])
	}
	if err := c.Do("POST", "/v1/sessions/work/stop", StopRequest{Operation: "k-l"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do("POST", "/v1/sessions", shell("op-l2", "work", "sleep 60"), nil); err != nil {
		t.Fatal(err)
	}
	var l Listing
	if err := c.Do("GET", "/v1/sessions", nil, &l); err != nil {
		t.Fatal(err)
	}
	if l.Host != host || l.UID != os.Geteuid() || len(l.Sessions) != 1 || l.Sessions[0].Generation != 2 || l.Sessions[0].Outcome != "running" {
		t.Fatalf("listing %+v", l)
	}
	c.Do("POST", "/v1/sessions/work/stop", StopRequest{Operation: "k-l2"}, nil)
	var ended SessionView
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if c.Do("GET", "/v1/sessions/work", nil, &ended); !ended.Running {
			break
		}
	}
	if ended.Outcome != "exited" || ended.ExitCode == nil {
		t.Fatalf("an ended session: %+v", ended)
	}
}
