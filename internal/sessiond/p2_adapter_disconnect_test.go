package sessiond

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// council P2: a disconnected adapter's last report is no longer anyone's
// word for the program's current state. Before this fix, a state channel
// that disconnected (the adapter crashed, or was never going to report
// again) left whatever it last said — including "waiting-input" — showing
// forever, which a -when-ready send would have trusted as if the program
// were still actually ready.
func TestAdapterDisconnectResetsAgentStateToUnknown(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-disc", "agent", `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
	req.StateChannel = true
	var made Created
	if err := c.Do("POST", "/v1/sessions", req, &made); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := strings.TrimSpace(waitScreen(t, c, "agent", ".sock").Lines[0])

	var before SessionView
	if err := c.Do("GET", "/v1/sessions/agent", nil, &before); err != nil || before.Agent.State != "unknown" {
		t.Fatalf("no adapter has connected yet: agent state should be unknown: %v %+v", err, before)
	}

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ad := &fakeAdapter{conn: conn, rd: bufio.NewReader(conn)}
	ad.report(t, AdapterReport{State: "waiting-input"})

	var ready SessionView
	for end := time.Now().Add(5 * time.Second); ready.Agent.State != "waiting-input" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &ready)
	}
	if ready.Agent.State != "waiting-input" {
		t.Fatalf("the report should have been seen: %+v", ready)
	}

	// The adapter disconnects — not superseded by anything, just gone.
	conn.Close()

	var after SessionView
	for end := time.Now().Add(5 * time.Second); after.Agent.State != "unknown" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &after)
	}
	if after.Agent.State != "unknown" {
		t.Fatalf("a disconnected adapter's stale state must reset to unknown, not keep reading %q", after.Agent.State)
	}

	// A fresh adapter connecting and reporting again works normally after
	// the reset — this isn't a one-way trip into brokenness.
	conn2, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	ad2 := &fakeAdapter{conn: conn2, rd: bufio.NewReader(conn2)}
	ad2.report(t, AdapterReport{State: "running"})
	var again SessionView
	for end := time.Now().Add(5 * time.Second); again.Agent.State != "running" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &again)
	}
	if again.Agent.State != "running" {
		t.Fatalf("a fresh adapter's report after a reset should be seen: %+v", again)
	}
}

// council P2-1 on #137: the first version of this fix tracked stateConn
// separately from conn, but still reset state whenever the stateConn
// connection disconnected for ANY reason — including merely being
// superseded. serve() closes whatever was the single latest connection
// the instant any new one is accepted, so a short-lived op-only hook
// call (begin/finish, never an AdapterReport) forcibly evicted a
// long-lived real reporter, whose read loop then ended and whose
// disconnect still fired a reset — every such hook call wiped real
// state; only the moment moved from "hook exits" to "hook arrives". The
// fix requires a reset to ALSO be this connection's own genuine end
// (error, EOF, the program closing it) — never merely having been
// superseded, which is instead distinguishable because a.conn no longer
// names it by the time disconnected runs. This test drives exactly that:
// a real reporter superseded by a later connection keeps its last
// reported state, and only a later, genuinely unsuperseded close of a
// state-reporting connection resets anything.
func TestAnOpOnlyConnectionNeverResetsAgentState(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-oponly", "agent", `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
	req.StateChannel = true
	var made Created
	if err := c.Do("POST", "/v1/sessions", req, &made); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := strings.TrimSpace(waitScreen(t, c, "agent", ".sock").Lines[0])

	// An op-only connection, as the very first connection: it never
	// touches state at all.
	ops := dialOps(t, path)
	if r := ops.call(t, opRequest{Op: "begin", ID: "op-1", Digest: "d1"}); r["first"] != true {
		t.Fatalf("begin: %+v", r)
	}

	// A real reporter connects next — superseding (closing) the op-only
	// connection above. Its own disconnect must not have left anything
	// behind that disturbs this report.
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ad := &fakeAdapter{conn: conn, rd: bufio.NewReader(conn)}
	ad.report(t, AdapterReport{State: "waiting-input"})
	var state SessionView
	for end := time.Now().Add(5 * time.Second); state.Agent.State != "waiting-input" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &state)
	}
	if state.Agent.State != "waiting-input" {
		t.Fatalf("the reporter's state should stick, undisturbed by the op-only connection that preceded it: %+v", state)
	}

	// A second op-only connection now supersedes the still-open reporter —
	// exactly the shape of a begin/finish hook call arriving while a real
	// adapter stays connected. The reporter's disconnect here is NOT its
	// own doing; it must leave the reported state alone, not reset it.
	ops2 := dialOps(t, path)
	if r := ops2.call(t, opRequest{Op: "begin", ID: "op-2", Digest: "d2"}); r["first"] != true {
		t.Fatalf("begin: %+v", r)
	}
	for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if err := c.Do("GET", "/v1/sessions/agent", nil, &state); err != nil {
			t.Fatal(err)
		}
		if state.Agent.State != "waiting-input" {
			t.Fatalf("a reporter merely superseded by a new connection (even one that's op-only) must not have its state reset — only its own genuine exit should: %+v", state)
		}
	}

	// A third connection reports a fresh state, superseding the op-only
	// one above. If that op-only connection had incorrectly become
	// stateConn, its own disconnect (triggered by this very supersession)
	// would try to reset — which must lose to this fresh report, never
	// clobber it.
	conn3, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ad3 := &fakeAdapter{conn: conn3, rd: bufio.NewReader(conn3)}
	ad3.report(t, AdapterReport{State: "running"})
	for end := time.Now().Add(5 * time.Second); state.Agent.State != "running" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &state)
	}
	if state.Agent.State != "running" {
		t.Fatalf("a fresh report must never be clobbered by an op-only connection's own disconnect: %+v", state)
	}

	// Now conn3's own genuine exit — not superseded by anything — must
	// still reset state: it was the real stateConn, and nothing else has
	// spoken for state since.
	conn3.Close()
	for end := time.Now().Add(5 * time.Second); state.Agent.State != "unknown" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &state)
	}
	if state.Agent.State != "unknown" {
		t.Fatalf("a state reporter's own, unsuperseded exit must still reset state: %+v", state)
	}
}

// council P2-2 on #137: ResetAgentUnknown used to rebuild the whole
// AgentState, losing Accepted along with State and Detail. An acceptance
// is a past fact (the program already confirmed it took an input), not a
// claim about the program's current state; a disconnected adapter no
// longer vouches for "running", but it never un-confirms an input it
// already said was accepted. Before the fix, a disconnect right after an
// acceptance would flip a live input-status read from "yes" back to
// "pending", and a send already waiting in awaitAccepted could settle
// without ever seeing the acceptance it was actually told about.
func TestResetAgentUnknownKeepsAcceptedButClearsState(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-keep-accepted", "agent", `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
	req.StateChannel = true
	var made Created
	if err := c.Do("POST", "/v1/sessions", req, &made); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := strings.TrimSpace(waitScreen(t, c, "agent", ".sock").Lines[0])

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ad := &fakeAdapter{conn: conn, rd: bufio.NewReader(conn)}
	ad.report(t, AdapterReport{State: "waiting-input", Accepted: "in-1"})

	var reported SessionView
	for end := time.Now().Add(5 * time.Second); reported.Agent.Accepted != "in-1" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &reported)
	}
	if reported.Agent.State != "waiting-input" || reported.Agent.Accepted != "in-1" {
		t.Fatalf("the combined state+accepted report should have been seen: %+v", reported)
	}

	conn.Close()

	var after SessionView
	for end := time.Now().Add(5 * time.Second); after.Agent.State != "unknown" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &after)
	}
	if after.Agent.State != "unknown" {
		t.Fatalf("state should still reset on disconnect: %+v", after)
	}
	if after.Agent.Accepted != "in-1" {
		t.Fatalf("Accepted is a past fact, not current state: a disconnect must not erase it: %+v", after)
	}
}

// council P3-3 on #137 (round three): statically, the mutant
// `resetDue := stillLatest` (dropping the `&& wasStateConn` half of the
// guard entirely) survives the whole existing suite -- nothing actually
// drives an op-only connection's OWN, unsuperseded close while it is the
// single latest connection, after a real reporter already set state and
// was itself superseded away. That mutant would wrongly reset state on
// every hook's own exit, exactly the original round-one bug, just
// reached a different way. This drives precisely that shape: a reporter
// sets state, an op-only connection supersedes it (state must survive,
// as the test above already checks), and then THAT op-only connection's
// own close (nothing superseding it) must likewise leave state alone --
// it was never stateConn, regardless of being the latest.
func TestAnOpOnlyConnectionsOwnUnsupersededExitNeverResetsAgentState(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-oponly-own-exit", "agent", `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
	req.StateChannel = true
	var made Created
	if err := c.Do("POST", "/v1/sessions", req, &made); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := strings.TrimSpace(waitScreen(t, c, "agent", ".sock").Lines[0])

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ad := &fakeAdapter{conn: conn, rd: bufio.NewReader(conn)}
	ad.report(t, AdapterReport{State: "waiting-input"})
	var state SessionView
	for end := time.Now().Add(5 * time.Second); state.Agent.State != "waiting-input" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &state)
	}
	if state.Agent.State != "waiting-input" {
		t.Fatalf("setup: %+v", state)
	}

	// An op-only connection supersedes the reporter -- now the single
	// latest connection, but never stateConn.
	ops := dialOps(t, path)
	if r := ops.call(t, opRequest{Op: "begin", ID: "op-own-1", Digest: "d1"}); r["first"] != true {
		t.Fatalf("begin: %+v", r)
	}

	// Its own close, unsuperseded by anything -- a plain hook exit after
	// its begin/finish calls -- must not touch state, even though it IS
	// a.conn at the moment it closes.
	ops.conn.Close()
	for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if err := c.Do("GET", "/v1/sessions/agent", nil, &state); err != nil {
			t.Fatal(err)
		}
		if state.Agent.State != "waiting-input" {
			t.Fatalf("an op-only connection's own exit, however latest, must not reset state it never set: %+v", state)
		}
	}
}
