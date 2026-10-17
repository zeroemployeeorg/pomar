package sessiond

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// council P2: a when-ready send refused for some OTHER reason after it
// waited for readiness (control held by someone else, here) must not burn
// the readiness it waited for. The adapter never reports twice in this
// test; the only way a second when-ready send can succeed quickly is if
// the first one's claim was given back.
func TestARefusedWhenReadySendGivesBackTheReadinessItClaimed(t *testing.T) {
	_, c := serve(t, root(t))
	req := shell("op-giveback", "agent", `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
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
	var ready SessionView
	for end := time.Now().Add(5 * time.Second); ready.Agent.State != "waiting-input" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &ready)
	}
	if ready.Agent.State != "waiting-input" {
		t.Fatalf("the report should have been seen: %+v", ready)
	}

	// A second client takes control and holds it, by never detaching: the
	// next send (without -take) must wait for readiness, then be refused
	// at the control step, having already claimed it.
	held, err := c.Attach("agent", false, false)
	if err != nil {
		t.Fatal(err)
	}
	in, inW := io.Pipe()
	go Pump(held, in, io.Discard, 60, 8, nil, nil)
	// Give the attach a moment to actually land and take control before
	// the send below races it.
	var holding SessionView
	for end := time.Now().Add(5 * time.Second); holding.Controller == "" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &holding)
	}
	if holding.Controller == "" {
		t.Fatal("the second client never took control")
	}

	var refused OpResult
	err = c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "giveback-1", Text: "hi\r", WhenReady: true}, &refused)
	var r *Refusal
	if !errors.As(err, &r) || r.Status != http.StatusConflict || r.Kind != KindConflict {
		t.Fatalf("a send while another client controls should refuse as conflict, not %v", err)
	}

	// The second client detaches (Ctrl-]), releasing control.
	inW.Write([]byte{DetachKey})
	var released SessionView
	for end := time.Now().Add(5 * time.Second); released.Controller != "" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &released)
	}
	if released.Controller != "" {
		t.Fatal("the second client never actually released control")
	}

	// The adapter has not reported anything new. A when-ready send with a
	// short bound must still succeed immediately: the earlier refusal must
	// not have consumed the one real "waiting-input" report there is.
	var out OpResult
	start := time.Now()
	if err := c.Do("POST", "/v1/sessions/agent/send", SendRequest{Operation: "giveback-2", Text: "hi\r", WhenReady: true, ReadyWaitMS: 200}, &out); err != nil {
		t.Fatalf("a when-ready send on the same still-current readiness should succeed, not refuse: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("this send waited %s instead of using the already-claimed-then-given-back readiness immediately", elapsed)
	}
	waitScreen(t, c, "agent", "got:hi")
}

// council P3 on #137: a give-back used to update the bookkeeping without
// waking anyone already blocked in awaitReady's own select -- a second
// when-ready send, already waiting when the first one's claim is given
// back, would sit out its whole wait bound instead of noticing the
// readiness is available again right away. Driven directly against
// awaitReady/unclaimReady (same package, the actual functions under
// test), not through two concurrent HTTP sends: a real send's claim is
// given back the instant Control fails, with no way to hold it open from
// outside, so there'd be no real window to put a second send to sleep in
// first.
func TestABlockedAwaitReadyWakesOnAGiveBackNotJustATimeout(t *testing.T) {
	s, c := serve(t, root(t))
	req := shell("op-giveback-wake", "agent", `printf '%s\n' "$POMAR_SESSION_STATE"; while read l; do printf 'got:%s\n' "$l"; done`)
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
	var ready SessionView
	for end := time.Now().Add(5 * time.Second); ready.Agent.State != "waiting-input" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.Do("GET", "/v1/sessions/agent", nil, &ready)
	}
	if ready.Agent.State != "waiting-input" {
		t.Fatalf("the report should have been seen: %+v", ready)
	}

	s.mu.Lock()
	sess := s.named["agent"]
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("the session isn't registered under its name")
	}

	// Claim the one readiness directly, exactly as a send's awaitReady
	// would, and hold the claim open -- nothing refuses it here, so it
	// stays claimed until explicitly given back below.
	seq, ok := s.awaitReady(sess, time.Second)
	if !ok {
		t.Fatal("the first claim should have succeeded")
	}

	// A second, independent awaitReady call blocks now: the readiness is
	// claimed, and the adapter never reports again, so without a wake
	// signal this can only resolve by its own long timeout.
	secondDone := make(chan bool, 1)
	start := time.Now()
	go func() {
		_, ok := s.awaitReady(sess, 10*time.Second)
		secondDone <- ok
	}()
	time.Sleep(100 * time.Millisecond) // let the second call actually reach its blocking select

	s.unclaimReady(sess, sess.ID, seq)

	select {
	case ok := <-secondDone:
		if !ok {
			t.Fatal("the second awaitReady should have claimed the given-back readiness")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("the blocked second call took %s to notice the give-back, want well under its 10s bound", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the blocked second awaitReady never woke on the give-back; it's sitting out its own 10s timeout instead")
	}
}
