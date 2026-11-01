package sessiond

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dialOps connects to a session's state channel and gives raw op calls.
type opsConn struct {
	conn net.Conn
	rd   *bufio.Reader
}

func dialOps(t *testing.T, path string) *opsConn {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &opsConn{conn: conn, rd: bufio.NewReader(conn)}
}

func (o *opsConn) call(t *testing.T, v any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	if _, err := o.conn.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	o.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := o.rd.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if json.Unmarshal(line, &out) != nil {
		t.Fatalf("not JSON: %s", line)
	}
	return out
}

// newAgentSession creates a session with a state channel and returns the
// dialed socket, ready for op calls or state reports, and the socket's own
// path, so a test can redial after a call that ends the connection (an
// error does, by design: the adapter reconnects).
func newAgentSession(t *testing.T, c *Client, name string) (*opsConn, string, Created) {
	t.Helper()
	req := shell("op-"+name, name, `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
	req.Cols, req.StateChannel = 120, true
	var made Created
	if err := c.Do("POST", "/v1/sessions", req, &made); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := strings.TrimSpace(waitScreen(t, c, name, ".sock").Lines[0])
	return dialOps(t, path), path, made
}

// A retry with the same digest is a no-op: first is false, and nothing is
// done twice. A different digest for the same id is refused.
func TestBeginDedupesByDigestAndRefusesAConflict(t *testing.T) {
	_, c := serve(t, root(t))
	o, _, _ := newAgentSession(t, c, "agent")

	r := o.call(t, opRequest{Op: "begin", ID: "op-1", Digest: "d1"})
	if first, _ := r["first"].(bool); !first {
		t.Fatalf("first begin: %+v", r)
	}
	r = o.call(t, opRequest{Op: "begin", ID: "op-1", Digest: "d1"})
	if first, ok := r["first"].(bool); !ok || first {
		t.Fatalf("a retry with the same digest: %+v", r)
	}
	r = o.call(t, opRequest{Op: "begin", ID: "op-1", Digest: "d2"})
	if _, ok := r["error"]; !ok {
		t.Fatalf("a conflicting digest should be refused: %+v", r)
	}
}

// finish(known:false) is permanent: a later finish, either way, on the same
// id is refused, never silently overwritten. Finishing an id never begun
// is refused too.
func TestFinishIsPermanentAndNeedsABegin(t *testing.T) {
	_, c := serve(t, root(t))
	o, path, _ := newAgentSession(t, c, "agent")

	// Each call below that expects an error ends its connection, by
	// design (the adapter reconnects): redial after each.
	known := false
	if r := o.call(t, opRequest{Op: "finish", ID: "never-begun", Known: &known}); r["error"] == nil {
		t.Fatalf("finishing an unbegun id should be refused: %+v", r)
	}
	o = dialOps(t, path)
	o.call(t, opRequest{Op: "begin", ID: "op-2", Digest: "d"})
	if r := o.call(t, opRequest{Op: "finish", ID: "op-2", Known: &known}); r["ok"] != true {
		t.Fatalf("first finish: %+v", r)
	}
	if r := o.call(t, opRequest{Op: "status", ID: "op-2"}); r["disposition"] != "acceptance_unknown" {
		t.Fatalf("status after finish(false): %+v", r)
	}
	yes := true
	if r := o.call(t, opRequest{Op: "finish", ID: "op-2", Known: &yes}); r["error"] == nil {
		t.Fatalf("a second finish should be refused, not flip the disposition: %+v", r)
	}
	o = dialOps(t, path)
	if r := o.call(t, opRequest{Op: "status", ID: "op-2"}); r["disposition"] != "acceptance_unknown" {
		t.Fatalf("the disposition must stay acceptance_unknown: %+v", r)
	}
}

// status on an id never begun is not_found; on a begun-but-unfinished id,
// pending; on a finished one, sent.
func TestStatusReflectsEachDisposition(t *testing.T) {
	_, c := serve(t, root(t))
	o, _, _ := newAgentSession(t, c, "agent")

	if r := o.call(t, opRequest{Op: "status", ID: "unknown-id"}); r["disposition"] != "not_found" {
		t.Fatalf("status of an unbegun id: %+v", r)
	}
	o.call(t, opRequest{Op: "begin", ID: "op-3", Digest: "d"})
	if r := o.call(t, opRequest{Op: "status", ID: "op-3"}); r["disposition"] != "pending" {
		t.Fatalf("status pending: %+v", r)
	}
	known := true
	o.call(t, opRequest{Op: "finish", ID: "op-3", Known: &known})
	if r := o.call(t, opRequest{Op: "status", ID: "op-3"}); r["disposition"] != "sent" {
		t.Fatalf("status sent: %+v", r)
	}
}

// Durability: an operation journal reopened (a restart, simulated directly
// at the level that owns reconciliation) finds any still-pending
// operation and marks it acceptance_unknown, permanently — a restart never
// lets a pending operation look like it quietly became success.
func TestAPendingOperationBecomesUnknownAcrossAReopen(t *testing.T) {
	r := root(t)
	if err := os.MkdirAll(r, 0o700); err != nil {
		t.Fatal(err)
	}
	j, err := openOpJournal(r, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if first, err := j.begin("op-a", "d"); err != nil || !first {
		t.Fatalf("begin: %v %v", first, err)
	}
	if first, err := j.begin("op-b", "d"); err != nil || !first {
		t.Fatalf("begin: %v %v", first, err)
	}
	known := true
	if err := j.finish("op-b", known); err != nil {
		t.Fatal(err)
	}
	j.close() // simulates the service ending without op-a ever finishing

	j2, err := openOpJournal(r, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	defer j2.close()
	if got := j2.status("op-a"); got != opAcceptanceUnknown {
		t.Fatalf("a pending operation across a reopen: got %q, want %q", got, opAcceptanceUnknown)
	}
	if got := j2.status("op-b"); got != opSent {
		t.Fatalf("an already-finished operation must not change on reopen: got %q", got)
	}
	// The conversion is itself durable: a third open sees the same thing,
	// not a repeated, different conversion.
	j2.close()
	j3, err := openOpJournal(r, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	defer j3.close()
	if got := j3.status("op-a"); got != opAcceptanceUnknown {
		t.Fatalf("the conversion itself must be durable: got %q", got)
	}
}

// The bound evicts sent entries oldest first, and never touches a pending
// or acceptance_unknown one.
func TestTheBoundEvictsSentEntriesNeverHeldOnes(t *testing.T) {
	r := root(t)
	if err := os.MkdirAll(r, 0o700); err != nil {
		t.Fatal(err)
	}
	const bound = 50 // this instance only: a shared constant would make the test slow
	j, err := openOpJournalBound(r, "sess-2", bound)
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	// A pending and an acceptance_unknown entry, first: these must survive
	// everything that follows.
	j.begin("held-pending", "d")
	j.begin("held-unknown", "d")
	known := false
	j.finish("held-unknown", known)
	// Fill well past the bound with sent entries.
	yes := true
	for i := 0; i < bound+20; i++ {
		id := "sent-" + strconv.Itoa(i)
		j.begin(id, "d")
		j.finish(id, yes)
	}
	if got := j.status("held-pending"); got != opPending {
		t.Fatalf("a pending entry was evicted: %q", got)
	}
	if got := j.status("held-unknown"); got != opAcceptanceUnknown {
		t.Fatalf("an acceptance_unknown entry was evicted: %q", got)
	}
	if got := j.status("sent-0"); got != "not_found" {
		t.Fatalf("the oldest sent entry should have been evicted: %q", got)
	}
	if got := j.status("sent-" + strconv.Itoa(bound+19)); got != opSent {
		t.Fatalf("the newest sent entry should still be there: %q", got)
	}
}

// show lists a session's held (acceptance_unknown) operations with their
// age, so an attaching client sees them before typing anything.
func TestShowListsHeldOperationsWithAge(t *testing.T) {
	_, c := serve(t, root(t))
	o, _, _ := newAgentSession(t, c, "agent")

	o.call(t, opRequest{Op: "begin", ID: "held-1", Digest: "d"})
	known := false
	o.call(t, opRequest{Op: "finish", ID: "held-1", Known: &known})
	o.call(t, opRequest{Op: "begin", ID: "sent-1", Digest: "d"})
	yes := true
	o.call(t, opRequest{Op: "finish", ID: "sent-1", Known: &yes})

	var v SessionView
	if err := c.Do("GET", "/v1/sessions/agent", nil, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.HeldOperations) != 1 || v.HeldOperations[0].ID != "held-1" {
		t.Fatalf("held_operations: %+v", v.HeldOperations)
	}
}

// zeocouncil's B1 fix: while any operation is held, a send (raw or
// when-ready) refuses unless AckUnknown is given, so a wake or a typed
// "continue" can't be the thing that resumes past an unresolved action.
// AckUnknown never clears the hold itself.
func TestSendRefusesWhileHeldUnlessAcknowledged(t *testing.T) {
	_, c := serve(t, root(t))
	o, path, _ := newAgentSession(t, c, "agent")

	o.call(t, opRequest{Op: "begin", ID: "held-2", Digest: "d"})
	known := false
	o.call(t, opRequest{Op: "finish", ID: "held-2", Known: &known})

	if err := c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "s-held-1", Text: "no\r"}, nil); kindOf(err) != KindHeld {
		t.Fatalf("a plain send should refuse while held: %v", err)
	}
	if err := c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "s-held-2", Text: "no\r", WhenReady: true}, nil); kindOf(err) != KindHeld {
		t.Fatalf("a when-ready send should refuse while held too: %v", err)
	}
	var out OpResult
	if err := c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "s-held-3", Text: "yes\r", AckUnknown: true}, &out); err != nil || out.Outcome != "written" {
		t.Fatalf("-ack-unknown should let it through: %v %+v", err, out)
	}
	// A fresh connection for the status check: the one just-written send
	// pushed its own write notification to o, which a status call on the
	// same connection would read instead of its reply.
	o2 := dialOps(t, path)
	if r := o2.call(t, opRequest{Op: "status", ID: "held-2"}); r["disposition"] != "acceptance_unknown" {
		t.Fatalf("ack-unknown must not clear the hold itself: %+v", r)
	}
}

// A session with no held operations is unaffected: plain sends still work
// exactly as before this fix.
func TestSendIsUnaffectedWithoutAnyHeldOperation(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-work", "work", `read l; printf 'got:%s\n' "$l"`)
	req.Cols = 80
	var made Created
	if err := c.Do("POST", "/v1/sessions", req, &made); err != nil {
		t.Fatal(err)
	}
	var out OpResult
	if err := c.Do("POST", "/v1/sessions/work/send", SendRequest{Operation: "s-plain", Text: "hi\r"}, &out); err != nil || out.Outcome != "written" {
		t.Fatalf("a session with no state channel, and so no held operations, should send normally: %v %+v", err, out)
	}
}

// Layer 6's HIGH: a crash mid-write leaves a torn last line (bytes with no
// trailing newline). Left in place, the next append lands directly after
// those torn bytes with no newline between them, so the reader sees one
// garbled line spanning the torn fragment AND the next, otherwise-complete
// record -- which fails to parse and is silently dropped, losing that next
// record on every future replay. A lost begin makes a retry of that same
// operation look like a first attempt: double execution. This reproduces
// exactly that shape: a clean record, a torn (newline-less) fragment
// simulating a crash mid-write, then a fresh begin written right after
// (simulating the same process continuing past the crash without ever
// reopening) -- and confirms that fresh begin survives a SECOND reopen,
// not merged into one garbled, dropped line with the torn bytes before it.
func TestATornLastLineDoesNotSwallowTheNextRecord(t *testing.T) {
	r := root(t)
	if err := os.MkdirAll(opDir(r), 0o700); err != nil {
		t.Fatal(err)
	}
	j, err := openOpJournal(r, "sess-torn")
	if err != nil {
		t.Fatal(err)
	}
	// Finished (sent), not left pending: a pending entry across a reopen
	// triggers its own automatic pending->acceptance_unknown conversion
	// write, which (under the bug) would itself be the thing that
	// collides with the torn bytes below, masking whether a begin called
	// afterward is the one actually swallowed.
	if _, err := j.begin("op-clean", "d"); err != nil {
		t.Fatal(err)
	}
	if err := j.finish("op-clean", true); err != nil {
		t.Fatal(err)
	}
	j.close()

	// Simulate a crash mid-write: append torn bytes (valid JSON prefix,
	// no trailing newline) directly to the file on disk, bypassing the
	// journal's own appendLocked (which always completes its line).
	f, err := os.OpenFile(opPath(r, "sess-torn"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"id":"op-torn","digest":"d","disposition":"pe`)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Reopen (as a restarted service would) and begin a fresh operation
	// right away: under the bug, this fresh record lands right after the
	// torn bytes with no newline before it, merging into one line that a
	// later reopen fails to parse and drops entirely -- exactly the
	// operation's own only record, since it's never finished before the
	// next reopen (a begin mid-flight, same as a real crash victim).
	j2, err := openOpJournal(r, "sess-torn")
	if err != nil {
		t.Fatal(err)
	}
	if got := j2.status("op-clean"); got != opSent {
		t.Fatalf("the clean, already-finished record before the torn line must still replay: got %q", got)
	}
	if _, err := j2.begin("op-after-crash", "d2"); err != nil {
		t.Fatalf("begin right after a torn line: %v", err)
	}
	j2.close()

	j3, err := openOpJournal(r, "sess-torn")
	if err != nil {
		t.Fatal(err)
	}
	defer j3.close()
	if got := j3.status("op-after-crash"); got == "not_found" {
		t.Fatalf("the begin written right after a torn line was swallowed on the next reopen: got %q (this is the double-execution bug: a dropped begin looks like the operation was never attempted, so a retry runs it again)", got)
	}
	if got := j3.status("op-clean"); got != opSent {
		t.Fatalf("the clean record must still be there across the second reopen too: got %q", got)
	}
}

// Layer 6's HIGH: a.ops is nil whenever its journal couldn't be opened
// (adapter.go's start discards that error on purpose, so the session
// itself is never blocked by it), or, racier, if a concurrent send reads
// s.adapters before start has finished setting it up. The send handler
// must refuse cleanly (fail closed) in either case, not call a.ops.held()
// on a nil *opJournal and panic the request.
func TestSendFailsClosedWhenTheOpJournalIsUnavailable(t *testing.T) {
	s, c := serve(t, root(t))
	o, _, made := newAgentSession(t, c, "agent-nil-ops")
	_ = o

	// Simulate the journal having failed to open (or the publish race):
	// same package, so this reaches straight past adapter.go's own
	// plumbing to force exactly the state the HIGH is about.
	s.mu.Lock()
	a := s.adapters[made.ID]
	if a == nil {
		s.mu.Unlock()
		t.Fatal("no adapter published for a state-channel session")
	}
	a.ops = nil
	s.mu.Unlock()

	err := c.Do("POST", "/v1/sessions/agent-nil-ops/send", SendRequest{Operation: "s-nil-ops-1", Text: "hi\r"}, nil)
	if kindOf(err) != KindHeld {
		t.Fatalf("a send against a session with no usable op journal should refuse (fail closed), not panic or succeed: %v", err)
	}
	// -ack-unknown still lets it through, same escape hatch as an actual
	// held operation.
	var out OpResult
	if err := c.Do("POST", "/v1/sessions/agent-nil-ops/send", SendRequest{Operation: "s-nil-ops-2", Text: "hi\r", AckUnknown: true}, &out); err != nil || out.Outcome != "written" {
		t.Fatalf("-ack-unknown should let a send through despite an unavailable op journal: %v %+v", err, out)
	}
}

// Layer 6's TOCTOU HIGH: the held check ran once, before a when-ready
// send's own wait (which can run up to 10 minutes). An operation becoming
// held DURING that wait -- an adapter can report any other operation's
// outcome as acceptance_unknown at any moment -- must still refuse the
// write, not rely on an answer that was true only at the start of the
// call. This creates the hold strictly AFTER the send's first held-check
// has certainly already run (the send is in flight, blocked in its ready
// wait, before the hold is created), and only THEN lets the wait resolve,
// so a pass here can only be explained by a SECOND check done right
// before the write -- not the first one, which saw no hold at all.
func TestSendRechecksHeldRightBeforeTheWriteNotOnlyBeforeTheWait(t *testing.T) {
	_, c := serve(t, root(t))
	o, _, _ := newAgentSession(t, c, "agent-toctou")

	type result struct {
		out OpResult
		err error
	}
	sent := make(chan result, 1)
	go func() {
		var out OpResult
		err := c.Do("POST", "/v1/sessions/agent-toctou/send", SendRequest{Operation: "w-toctou-1", Text: "line\r", WhenReady: true, ReadyWaitMS: 8000}, &out)
		sent <- result{out, err}
	}()
	// Margin for the send's first held-check (instantaneous: no hold
	// exists yet) to have already run and let the request move into its
	// ready wait, which only this test's own next report can resolve.
	time.Sleep(200 * time.Millisecond)
	select {
	case r := <-sent:
		t.Fatalf("the send ended before it was ever made ready: %+v %v", r.out, r.err)
	default:
	}

	o.call(t, opRequest{Op: "begin", ID: "held-toctou", Digest: "d"})
	known := false
	o.call(t, opRequest{Op: "finish", ID: "held-toctou", Known: &known})

	b, _ := json.Marshal(AdapterReport{State: "waiting-input"})
	if _, err := o.conn.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}

	r := <-sent
	if kindOf(r.err) != KindHeld {
		t.Fatalf("a send that became held during its own ready wait wrote anyway: err=%v out=%+v, want a held refusal", r.err, r.out)
	}
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(strings.Join(screenOf(t, c, "agent-toctou").Lines, "\n"), "got:line") {
		t.Fatal("the refused send's text reached the program anyway")
	}
}

// Layer 6's HIGH, the other half: a pending operation whose program exits
// (rather than its adapter calling finish) must become acceptance_unknown
// right then, not stay pending forever (invisible to the held check, which
// only ever lists acceptance_unknown) and not only once some LATER
// sessiond restart happens to reopen the journal. And since a session's
// name, not its random per-generation ID, is what a caller actually comes
// back to, a fresh session re-created under the SAME name must inherit
// that hold: the operation journal is now keyed by name (adapter.go's
// start), so the old generation's still-unresolved hold is exactly what
// the new generation's journal already contains when it opens.
func TestAPendingOperationBecomesHeldOnExitAndSurvivesUnderTheSameName(t *testing.T) {
	r := root(t)
	s, c := serve(t, r)
	mk := func(op string) Created {
		req := shell(op, "exit-held", `printf '%s\n' "$POMAR_SESSION_STATE"; read l; exit 0`)
		req.Cols, req.StateChannel = 80, true
		var made Created
		if err := c.Do("POST", "/v1/sessions", req, &made); err != nil {
			t.Fatalf("create %s: %v", op, err)
		}
		return made
	}
	waitRunning := func(name string, running bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			var v SessionView
			if c.Do("GET", "/v1/sessions/"+name, nil, &v) == nil && v.Running == running {
				return
			}
		}
		t.Fatalf("%s never reached running=%v", name, running)
	}

	gen1 := mk("op-exit-held-1")
	path := strings.TrimSpace(waitScreen(t, c, "exit-held", ".sock").Lines[0])
	o := dialOps(t, path)
	o.call(t, opRequest{Op: "begin", ID: "held-exit-1", Digest: "d"})
	// An ordinary send, before any hold exists yet, just to make the
	// program read its one line and exit on its own.
	if err := c.Do("POST", "/v1/sessions/exit-held/send", SendRequest{Operation: "s-exit-1", Text: "bye\r"}, nil); err != nil {
		t.Fatalf("send to make it exit: %v", err)
	}
	waitRunning("exit-held", false)

	// Running going false and the pending op converting to held are two
	// INDEPENDENT reactions to the same t.Done() close, not one atomic
	// update: adapter.start backgrounds `go func() { <-t.Done();
	// a.close() }()`, and a.close is what calls ops.convertPending. Go
	// gives no ordering guarantee between that goroutine running and
	// whatever else observes t.Info().Running go false, so waitRunning
	// above (which only proves the latter) can return before the former
	// has happened. convertPending's own doc comment says this is meant
	// to "show up as held immediately" -- that intent holds, but nothing
	// in the code makes it synchronous, so the honest client-side check
	// is a short, bounded poll, not a single unretried read with no
	// margin for an inherent (if normally fast) scheduling gap.
	var v SessionView
	for end := time.Now().Add(2 * time.Second); len(v.HeldOperations) != 1 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		v = SessionView{}
		if err := c.Do("GET", "/v1/sessions/exit-held", nil, &v); err != nil {
			t.Fatal(err)
		}
	}
	if len(v.HeldOperations) != 1 || v.HeldOperations[0].ID != "held-exit-1" {
		t.Fatalf("a pending operation whose program exited should show as held at once, no restart needed: %+v", v.HeldOperations)
	}

	// A fresh generation under the SAME name: it must inherit the old
	// generation's unresolved hold.
	gen2 := mk("op-exit-held-2")
	if gen2.ID == gen1.ID {
		t.Fatal("the second create didn't make a new generation")
	}
	if err := c.Do("POST", "/v1/sessions/exit-held/send", SendRequest{Operation: "s-exit-2", Text: "hi\r"}, nil); kindOf(err) != KindHeld {
		t.Fatalf("a send to the NEW generation, under the same name, should still refuse for the OLD generation's held operation: %v", err)
	}
	var out OpResult
	if err := c.Do("POST", "/v1/sessions/exit-held/send", SendRequest{Operation: "s-exit-3", Text: "hi\r", AckUnknown: true}, &out); err != nil || out.Outcome != "written" {
		t.Fatalf("-ack-unknown should still let it through on the new generation: %v %+v", err, out)
	}
	s.Close()
}
