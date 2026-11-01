package seatterm

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/termstate"
)

// journal is an in-memory audit that can be made to fail.
type journal struct {
	mu   sync.Mutex
	recs []AuditRecord
	fail bool
}

func (j *journal) add(r AuditRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail {
		return errors.New("journal full")
	}
	j.recs = append(j.recs, r)
	return nil
}

func (j *journal) has(action, phase, outcome string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, r := range j.recs {
		if r.Action == action && r.Phase == phase && (outcome == "" || r.Outcome == outcome) {
			return true
		}
	}
	return false
}

func sh(script string) SessionSpec {
	return SessionSpec{Name: "agent", Kind: "agent", Command: Command{Path: "/bin/sh", Args: []string{"-c", script}, UID: -1, GID: -1}, Cols: 80, Rows: 24}
}

// collect reads a client's output until it contains want, or fails.
func collect(t *testing.T, c *Client, prefix []byte, want string) []byte {
	t.Helper()
	got := append([]byte(nil), prefix...)
	deadline := time.After(10 * time.Second)
	for !bytes.Contains(got, []byte(want)) {
		select {
		case e := <-c.Events():
			got = append(got, e.Data...)
		case <-deadline:
			t.Fatalf("never saw %q in %q", want, got)
		}
	}
	return got
}

func newHost(t *testing.T) (*SessionHost, *journal) {
	j := &journal{}
	return NewSessionHost(j.add), j
}

// A session's program outlives its clients: detaching never stops it, and
// a new client's snapshot shows what it printed meanwhile.
func TestASessionOutlivesItsClients(t *testing.T) {
	h, j := newHost(t)
	s, created, err := h.Create("op-1", "operator", sh("printf first; sleep 0.5; printf second; sleep 30"))
	if err != nil || !created {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	c, snap, err := s.Attach("operator")
	if err != nil {
		t.Fatal(err)
	}
	collect(t, c, snap.Data, "first")
	c.Detach()
	if !s.Info().Running {
		t.Fatal("detaching stopped the program")
	}
	// pomar's review: attaching c2 immediately and polling ITS snapshot
	// would let "second" arrive over c2's own live event stream while it
	// is already attached, which no longer tests the actual premise here
	// -- that the session retains output while NO client is attached at
	// all. Poll the session's own retained state directly instead (same
	// package: snapshotLocked, under its own s.mu), with nothing attached,
	// and only attach c2 once that is already true. The shell's own 0.5s
	// sleep is measured from ITS process start, not from any fixed point
	// this test controls, so a fixed wait here only has to be "long
	// enough" under whatever scheduling delay happens to apply to the
	// CHILD specifically -- poll for the real condition instead of
	// guessing a bigger margin that still has to hold under arbitrary
	// contention.
	var observedDetached bool
	var lastRetained []byte
	for end := time.Now().Add(5 * time.Second); !observedDetached && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		s.mu.Lock()
		lastRetained = s.snapshotLocked().Data
		s.mu.Unlock()
		if strings.Contains(string(lastRetained), "firstsecond") {
			observedDetached = true
		}
	}
	// pomar's review: falling through to Attach regardless of whether the
	// loop above actually observed the condition (vs merely expiring)
	// would let output that only arrives in the gap between expiry and
	// Attach satisfy c2's later snapshot without ever having proven the
	// no-client-attached premise at all -- fail explicitly here instead.
	if !observedDetached {
		t.Fatalf("never observed retained output while no client was attached: %q", lastRetained)
	}
	if !s.Info().Running {
		t.Fatal("waiting with no client attached stopped the program")
	}
	c2, snap2, err := s.Attach("operator")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Detach()
	if !strings.Contains(string(snap2.Data), "firstsecond") || !snap2.Complete || snap2.From != 0 {
		t.Fatalf("snapshot %+v %q", snap2, snap2.Data)
	}
	if !j.has("attach", "intent", "") || !j.has("detach", "outcome", "ok") {
		t.Fatal("attach and detach weren't journaled")
	}
}

// Creating is once per operation ID: a retry returns the same session and
// starts no second program.
func TestCreateIsOncePerOperation(t *testing.T) {
	h, _ := newHost(t)
	s1, created1, err := h.Create("op-same", "operator", sh("sleep 30"))
	if err != nil || !created1 {
		t.Fatal(err)
	}
	defer s1.cmd.Process.Kill()
	s2, created2, err := h.Create("op-same", "operator", sh("sleep 30"))
	if err != nil || created2 || s2 != s1 || len(h.List()) != 1 {
		t.Fatalf("a retry made another session: %v %v %d", created2, err, len(h.List()))
	}
}

// Output reaches a client in order: its events start exactly where its
// snapshot ends, with no byte missing or repeated.
func TestSnapshotAndOutputAreOneOrderedStream(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-2", "operator", sh("i=0; while [ $i -lt 200 ]; do printf 'line-%d\\n' $i; i=$((i+1)); done; while :; do echo tick; sleep 0.1; done"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	time.Sleep(50 * time.Millisecond)
	c, snap, err := s.Attach("operator")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Detach()
	next, got := snap.To, append([]byte(nil), snap.Data...)
	deadline := time.After(10 * time.Second)
	for !bytes.Contains(got, []byte("line-199")) {
		select {
		case e := <-c.Events():
			if e.Offset != next {
				t.Fatalf("event at %d, expected %d", e.Offset, next)
			}
			next += uint64(len(e.Data))
			got = append(got, e.Data...)
		case <-deadline:
			t.Fatal("output never finished")
		}
	}
	for i := 0; i < 200; i++ {
		if bytes.Count(got, []byte("line-"+strconv.Itoa(i)+"\r\n")) != 1 {
			t.Fatalf("line %d is missing or repeated", i)
		}
	}
}

// A viewer types nothing and sizes nothing; the controller's input reaches
// the program.
func TestOnlyTheControllerReachesThePTY(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-3", "operator", sh("stty -echo; while read l; do printf 'got:%s\\n' \"$l\"; done"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	viewer, _, _ := s.Attach("viewer")
	defer viewer.Detach()
	if viewer.Input([]byte("x\n")) != ErrNotController || viewer.Resize(100, 30) != ErrNotController {
		t.Fatal("a viewer's input or resize was accepted")
	}
	ctl, snap, _ := s.Attach("operator")
	defer ctl.Detach()
	if _, err := ctl.Control(false, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Control(false, "test", nil); err != ErrControlled {
		t.Fatalf("a second controller was granted: %v", err)
	}
	if err := ctl.Input([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	collect(t, ctl, snap.Data, "got:hello")
	if err := ctl.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	for d := time.Now().Add(5 * time.Second); time.Now().Before(d); time.Sleep(20 * time.Millisecond) {
		if in := s.Info(); in.Cols == 100 && in.Rows == 30 {
			return
		}
	}
	t.Fatal("the controller's resize never applied")
}

// A takeover fences the old controller at the write itself: its write in
// flight, blocked on a terminal that isn't reading, is cut short; its
// queued input never lands; and only the new controller's input arrives.
func TestATakeoverFencesTheOldControllersQueuedInput(t *testing.T) {
	h, j := newHost(t)
	// pomar/architect review: a fixed sleep here only has to be "long
	// enough" for the child to have actually reached `stty raw -echo`
	// under whatever host scheduling delay applies to ITS startup
	// specifically -- unrelated to delay anywhere else in the test. The
	// shell now writes an explicit marker right after that call returns,
	// so the test can wait for the real condition the blocking-writer
	// behavior below actually depends on (raw/no-echo mode genuinely
	// active), rather than guess a duration.
	s, _, err := h.Create("op-4", "operator", sh("stty raw -echo; printf 'RAWREADY\\n'; sleep 2; exec cat"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	a, snap, _ := s.Attach("seat")
	defer a.Detach()
	if _, err := a.Control(false, "test", nil); err != nil {
		t.Fatal(err)
	}
	snap.Data = collect(t, a, snap.Data, "RAWREADY")
	filler := bytes.Repeat([]byte("."), MaxData)
	for i := 0; i < 4; i++ { // more than the terminal's input holds: the writer blocks
		if err := a.Input(filler); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Input([]byte("AAAA-STALE")); err != nil {
		t.Fatal(err)
	}
	b, _, _ := s.Attach("operator")
	defer b.Detach()
	start := time.Now()
	if _, err := b.Control(true, "operator takeover", nil); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if time.Since(start) > defaultFenceBound+time.Second {
		t.Fatal("the takeover wasn't bounded")
	}
	if a.Input([]byte("late")) != ErrNotController {
		t.Fatal("the old controller could still queue input")
	}
	if err := b.Input([]byte("BBBB-FRESH")); err != nil {
		t.Fatal(err)
	}
	got := collect(t, b, snap.Data, "BBBB-FRESH")
	if bytes.Contains(got, []byte("AAAA-STALE")) {
		t.Fatal("the old controller's queued input landed after the takeover")
	}
	if !j.has("take", "intent", "") || !j.has("take", "outcome", "ok") {
		t.Fatal("the takeover wasn't journaled as intent and outcome")
	}
}

// A grant whose intent can't be journaled is refused; giving control up,
// and detaching, still work when the journal is failing.
func TestNoGrantWithoutItsJournalButRevocationAlways(t *testing.T) {
	h, j := newHost(t)
	s, _, err := h.Create("op-5", "operator", sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	c, _, _ := s.Attach("operator")
	if _, err := c.Control(false, "test", nil); err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	j.fail = true
	j.mu.Unlock()
	other, _, err := s.Attach("seat")
	if err != ErrAudit || other != nil {
		t.Fatalf("an attach was granted without its journal: %v", err)
	}
	c.Release()
	if s.Info().Controller != "" {
		t.Fatal("release was held up by the journal")
	}
	if _, err := c.Control(false, "test", nil); err != ErrAudit {
		t.Fatalf("a grant without its journal: %v", err)
	}
	c.Detach()
	if len(s.Info().Viewers) != 0 || !s.Info().Running {
		t.Fatal("detach was held up by the journal, or stopped the program")
	}
}

// A client that stops reading falls behind and is told so, never silently
// skipped; a new snapshot resumes it exactly after its cut. The terminal
// keeps draining meanwhile.
func TestAClientThatFallsBehindIsResnapshotted(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-6", "operator", sh("i=0; while [ $i -lt 4000 ]; do printf '%0200d\\n' $i; i=$((i+1)); done; while :; do echo tick; sleep 0.1; done"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	c, _, _ := s.Attach("viewer")
	defer c.Detach()
	select {
	case <-c.Signals():
	case <-time.After(10 * time.Second):
		t.Fatal("a client that stopped reading was never told it fell behind")
	}
	if !c.Behind() {
		t.Fatal("the client isn't marked behind")
	}
	snap := c.Resnapshot()
	if c.Behind() || len(snap.Data) == 0 {
		t.Fatal("no fresh cut")
	}
	select {
	case e := <-c.Events():
		if e.Offset != snap.To {
			t.Fatalf("output after the cut starts at %d, not exactly at %d", e.Offset, snap.To)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no output after the cut: the program keeps printing, so the client was never resumed")
	}
}

// When the program exits, the session says so with its code, and grants
// nothing more.
func TestAnExitedSessionGrantsNothing(t *testing.T) {
	h, j := newHost(t)
	s, _, err := h.Create("op-7", "operator", sh("exit 3"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the session never ended")
	}
	in := s.Info()
	if in.Running || in.ExitCode == nil || *in.ExitCode != 3 {
		t.Fatalf("state %+v", in)
	}
	c, _, _ := s.Attach("operator")
	if _, err := c.Control(false, "test", nil); err != ErrSessionExited {
		t.Fatalf("control of an exited session: %v", err)
	}
	if !j.has("exit", "outcome", "ok") {
		t.Fatal("the exit wasn't journaled")
	}
}

// Detaching revokes control and removes the client in one step: a detached
// client controls nothing and queues nothing, whatever it held, and the
// program keeps running (ZEOAPP's review of 6b4691ca, SOW 184).
func TestADetachedClientControlsNothing(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-8", "operator", sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	c, _, _ := s.Attach("operator")
	if _, err := c.Control(false, "test", nil); err != nil {
		t.Fatal(err)
	}
	c.Detach()
	if _, err := c.Control(false, "again", nil); err == nil {
		t.Fatal("a detached client regained control")
	}
	if c.Input([]byte("x")) != ErrNotController || c.Resize(90, 30) != ErrNotController {
		t.Fatal("a detached client queued input or a resize")
	}
	if in := s.Info(); in.Controller != "" || !in.Running {
		t.Fatalf("state after detach: %+v", in)
	}
}

// Detach racing Control on the same client never leaves the client in
// control once both have returned.
func TestDetachRacingControlNeverLeavesADetachedController(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-9", "operator", sh("sleep 60"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	for i := 0; i < 300; i++ {
		principal := "client-" + strconv.Itoa(i)
		c, _, err := s.Attach(principal)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); c.Control(false, "race", nil) }()
		go func() { defer wg.Done(); c.Detach() }()
		wg.Wait()
		if in := s.Info(); in.Controller == principal {
			t.Fatalf("round %d: a detached client holds control", i)
		}
		if c.Input([]byte("x")) == nil {
			t.Fatalf("round %d: a detached client queued input", i)
		}
	}
}

// Queued is not written: the outcomes say what reached the program, and
// writes refused for a revoked grant are counted, never resent.
func TestWriteOutcomesAreCounted(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-10", "operator", sh("stty -echo; while read l; do printf 'got:%s\\n' \"$l\"; done"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	c, snap, _ := s.Attach("operator")
	defer c.Detach()
	if _, err := c.Control(false, "test", nil); err != nil {
		t.Fatal(err)
	}
	c.Input([]byte("one\n"))
	c.Resize(100, 30)
	collect(t, c, snap.Data, "got:one")
	for d := time.Now().Add(5 * time.Second); time.Now().Before(d); time.Sleep(20 * time.Millisecond) {
		if o := c.Outcomes(); o.Written == 2 {
			return
		}
	}
	t.Fatalf("outcomes %+v", c.Outcomes())
}

// Against a program that never reads, a take still confirms the old
// controller's write ended, by cutting it short; nothing of the old
// controller's is in flight once control is granted.
func TestATakeoverCutsAWriteToAProgramThatNeverReads(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-11", "operator", sh("stty raw -echo; exec sleep 600"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	time.Sleep(200 * time.Millisecond)
	a, _, _ := s.Attach("seat")
	defer a.Detach()
	a.Control(false, "test", nil)
	for i := 0; i < 4; i++ {
		a.Input(bytes.Repeat([]byte("."), MaxData))
	}
	for d := time.Now().Add(5 * time.Second); time.Now().Before(d); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		inflight := s.inflight
		s.mu.Unlock()
		if inflight != 0 {
			break
		}
	}
	b, _, _ := s.Attach("operator")
	defer b.Detach()
	start := time.Now()
	epoch, err := b.Control(true, "takeover", nil)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the takeover waited %v: the write in flight wasn't cut short", time.Since(start))
	}
	s.mu.Lock()
	inflight := s.inflight
	s.mu.Unlock()
	if inflight != 0 && inflight < epoch {
		t.Fatal("an old controller's write was still in flight when control was granted")
	}
}

// When the old controller's write can't be confirmed ended within the
// bound, the take grants nothing and says so; the old controller stays
// revoked.
func TestATakeoverThatCantConfirmTheFenceGrantsNothing(t *testing.T) {
	h, j := newHost(t)
	release := make(chan struct{})
	defer close(release)
	h.fenceBound, h.writeHook = 200*time.Millisecond, func() { <-release }
	s, _, err := h.Create("op-12", "operator", sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	a, _, _ := s.Attach("seat")
	defer a.Detach()
	a.Control(false, "test", nil)
	a.Input([]byte("x"))
	for d := time.Now().Add(5 * time.Second); time.Now().Before(d); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		inflight := s.inflight
		s.mu.Unlock()
		if inflight != 0 {
			break
		}
	}
	b, _, _ := s.Attach("operator")
	defer b.Detach()
	if _, err := b.Control(true, "takeover", nil); err != ErrFenceUncertain {
		t.Fatalf("an unconfirmed fence: %v", err)
	}
	if in := s.Info(); in.Controller != "" {
		t.Fatalf("control was granted without a confirmed fence: %+v", in)
	}
	if a.Input([]byte("y")) != ErrNotController {
		t.Fatal("the old controller wasn't revoked")
	}
	if !j.has("take", "outcome", "uncertain") {
		t.Fatal("the uncertain outcome wasn't journaled")
	}
}

// An operation ID belongs to the principal that issued it, and a retry must
// ask for the same session.
func TestOperationsAreScopedToTheirPrincipal(t *testing.T) {
	h, _ := newHost(t)
	alice, _, err := h.Create("op-13", "alice", sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	defer alice.cmd.Process.Kill()
	mallory, created, err := h.Create("op-13", "mallory", sh("sleep 30"))
	if err != nil || !created || mallory == alice {
		t.Fatalf("another principal's operation ID returned alice's session: %v %v", created, err)
	}
	defer mallory.cmd.Process.Kill()
	if _, _, err := h.Create("op-13", "alice", sh("sleep 31")); err != ErrOperationConflict {
		t.Fatalf("a retry with a different spec: %v", err)
	}
}

// Nothing of the host's environment reaches a session its spec doesn't
// name.
func TestASessionGetsNoneOfTheHostsEnvironment(t *testing.T) {
	t.Setenv("POMAR_SEATTERM_HOST_ONLY", "must-not-leak")
	h, _ := newHost(t)
	s, _, err := h.Create("op-14", "operator", SessionSpec{Name: "shell", Kind: "shell",
		Command: Command{Path: "/usr/bin/env", UID: -1, GID: -1}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	<-s.Done()
	c, snap, _ := s.Attach("operator")
	defer c.Detach()
	if bytes.Contains(snap.Data, []byte("must-not-leak")) {
		t.Fatal("the host's environment reached the session")
	}
}

// The session bound counts live sessions: exited ones leave it, and a
// retry of an exited session's operation still returns it.
func TestExitedSessionsLeaveTheLiveBound(t *testing.T) {
	h, _ := newHost(t)
	for i := 0; i < MaxSessions+4; i++ {
		s, _, err := h.Create("op-exit-"+strconv.Itoa(i), "operator", sh("exit 0"))
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		<-s.Done()
		for d := time.Now().Add(2 * time.Second); time.Now().Before(d) && len(h.List()) > 0; time.Sleep(5 * time.Millisecond) {
		}
	}
	again, created, err := h.Create("op-exit-0", "operator", sh("exit 0"))
	if err != nil || created || again.Info().Running {
		t.Fatalf("a retry of an exited session's operation: %v %v", created, err)
	}
	var live []*TermSession
	for i := 0; i < MaxSessions; i++ {
		s, _, err := h.Create("op-live-"+strconv.Itoa(i), "operator", sh("sleep 30"))
		if err != nil {
			t.Fatal(err)
		}
		live = append(live, s)
	}
	defer func() {
		for _, s := range live {
			s.cmd.Process.Kill()
		}
	}()
	if _, _, err := h.Create("op-live-over", "operator", sh("sleep 30")); err != ErrTooMany {
		t.Fatalf("one more live session than the bound: %v", err)
	}
}

// Terminate stops the program and journals it; clients' event channels
// close when the program has exited, and on detach.
func TestTerminateStopsTheProgramAndClosesClients(t *testing.T) {
	h, j := newHost(t)
	s, _, err := h.Create("op-15", "operator", sh("sleep 60"))
	if err != nil {
		t.Fatal(err)
	}
	c, _, _ := s.Attach("viewer")
	if err := h.Terminate("op-15-stop", "operator", s.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the program wasn't stopped")
	}
	for range c.Events() {
	}
	if !j.has("terminate", "outcome", "ok") {
		t.Fatal("terminate wasn't journaled")
	}
	d, _, _ := s.Attach("late")
	d.Detach()
	for range d.Events() {
	}
}

// A slow journal never holds the session's lock: the session's state stays
// readable, and its output keeps flowing, while a grant is being journaled.
func TestASlowJournalHoldsNothingUp(t *testing.T) {
	slow := &journal{}
	var delay atomic.Bool
	h := NewSessionHost(func(r AuditRecord) error {
		if delay.Load() {
			time.Sleep(500 * time.Millisecond)
		}
		return slow.add(r)
	})
	s, _, err := h.Create("op-16", "operator", sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	c, _, _ := s.Attach("operator")
	defer c.Detach()
	delay.Store(true)
	go c.Control(false, "slow", nil)
	time.Sleep(700 * time.Millisecond) // past the intent's record, inside the outcome's
	start := time.Now()
	s.Info()
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("reading the session waited %v on the journal", time.Since(start))
	}
	// A create journaling slowly holds up neither listing nor its own retry.
	go h.Create("op-16-b", "operator", sh("sleep 30"))
	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	h.List()
	if _, _, err := h.Create("op-16-b", "operator", sh("sleep 30")); err != ErrCreating {
		t.Fatalf("a retry while the first create journals: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("listing waited %v on a create's journal", time.Since(start))
	}
	delay.Store(false)
	for d := time.Now().Add(5 * time.Second); time.Now().Before(d); time.Sleep(20 * time.Millisecond) {
		if s2, created, err := h.Create("op-16-b", "operator", sh("sleep 30")); err == nil && !created {
			s2.cmd.Process.Kill()
			return
		}
	}
	t.Fatal("the slow create never finished")
}

// Terminate waits on the program's reap, never on its exit's journal: a slow
// journal makes it neither slow nor uncertain. A program that ignores the
// hangup is killed after the grace.
func TestTerminateWaitsOnTheReapNotTheJournal(t *testing.T) {
	j := &journal{}
	var delay atomic.Bool
	h := NewSessionHost(func(r AuditRecord) error {
		if delay.Load() && r.Action == "exit" {
			time.Sleep(2 * time.Second)
		}
		return j.add(r)
	})
	h.fenceBound, h.hangupGrace = 300*time.Millisecond, 200*time.Millisecond
	s, _, err := h.Create("op-17", "operator", sh("trap '' HUP; while :; do sleep 1; done"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the trap is set
	delay.Store(true)
	start := time.Now()
	if err := h.Terminate("op-17-stop", "operator", s.ID); err != nil {
		t.Fatalf("terminate behind a slow journal: %v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("terminate waited %v, on the journal", d)
	}
	select {
	case <-s.reaped:
	default:
		t.Fatal("terminate returned before the reap")
	}
	if !j.has("terminate", "outcome", "ok") {
		t.Fatal("the terminate's outcome wasn't ok")
	}
	<-s.Done()
}

// A command is named by its absolute path, never looked up on the host's
// PATH.
func TestARelativeCommandIsRefused(t *testing.T) {
	h, _ := newHost(t)
	spec := sh("true")
	spec.Command.Path = "sh"
	if _, _, err := h.Create("op-18", "operator", spec); err == nil {
		t.Fatal("a bare command name was looked up")
	}
}

// When the leader exits on its own, a straggler it leaves behind — a
// background child that ignores the kernel's automatic SIGHUP to the
// terminal's group — doesn't keep the session from being noticed to have
// ended: the group is killed right behind the leader's own reap, so
// pump's Read sees EIO and the session's Done closes within a bound, not
// only once the straggler happens to exit on its own (zeocouncil's review
// of #126, LOW 3).
func TestAStragglerThatIgnoresHUPDoesntKeepTheSessionAlive(t *testing.T) {
	h, j := newHost(t)
	s, _, err := h.Create("op-19", "operator",
		sh("(trap '' HUP; while :; do sleep 1; done) & disown; exit 7"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session never ended: a straggler kept the PTY open")
	}
	in := s.Info()
	if in.Running || in.ExitCode == nil || *in.ExitCode != 7 {
		t.Fatalf("state %+v", in)
	}
	if !j.has("exit", "outcome", "ok") {
		t.Fatal("the exit wasn't journaled")
	}
}

// An attach's screen cut, fed to a terminal and followed by the output from
// its offset, renders the session's own screen, without its history.
func TestTheScreenCutRendersTheLiveScreen(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-20", "operator", sh(`i=0; while [ $i -lt 60 ]; do printf 'line %d\n' $i; i=$((i+1)); done; printf '\033[2;5H\033[1mbold\033[0m'; read l; printf 'after:%s' "$l"; sleep 30`))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	c, _, _ := s.Attach("operator")
	collect(t, c, nil, "bold")
	c.Detach()
	v, snap, _ := s.Attach("viewer")
	defer v.Detach()
	if !snap.ScreenExact || snap.ScreenFormat != termstate.SnapshotVersion {
		t.Fatalf("cut %+v", snap)
	}
	op, _, _ := s.Attach("operator")
	defer op.Detach()
	if _, err := op.Control(false, "test", nil); err != nil {
		t.Fatal(err)
	}
	op.Input([]byte("go\n"))
	live := collect(t, v, nil, "after:go")
	model := termstate.New(int(snap.Cols), int(snap.Rows), 0)
	model.Feed(snap.Screen)
	model.Feed(live)
	want := s.Screen()
	for y, line := range want.Lines {
		if model.Row(y) != line {
			t.Fatalf("row %d: %q, want %q", y, model.Row(y), line)
		}
	}
	if !strings.Contains(strings.Join(want.Lines, "\n"), "after:go") || strings.Contains(strings.Join(want.Lines, "\n"), "line 0\n") {
		t.Fatalf("screen %q", want.Lines)
	}
}

// AttachHidden is a viewer in every way that matters for fan-out (it gets
// output, falls behind and resnapshots the same as any other), but never
// shows in Viewers, Presence, or the audit journal: sessiond's durable log
// uses it precisely so it isn't mistaken for a real client.
func TestAttachHiddenIsInvisibleButStillAViewer(t *testing.T) {
	h, j := newHost(t)
	s, _, err := h.Create("op-hidden", "operator", sh("printf hi; sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.cmd.Process.Kill()
	hc, snap, err := s.AttachHidden("log")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Data) != 0 && !strings.Contains(string(snap.Data), "hi") {
		t.Fatalf("hidden snapshot: %q", snap.Data)
	}
	if in := s.Info(); len(in.Viewers) != 0 {
		t.Fatalf("a hidden client is listed: %+v", in.Viewers)
	}
	vc, _, err := s.Attach("viewer")
	if err != nil {
		t.Fatal(err)
	}
	defer vc.Detach()
	if p := vc.Presence(); len(p.Viewers) != 1 || p.Viewers[0] != "viewer" {
		t.Fatalf("presence lists the hidden client: %+v", p.Viewers)
	}
	collect(t, hc, nil, "hi") // it still receives output, the same as any viewer
	if j.has("attach", "outcome", "ok") {
		for _, r := range j.recs {
			if r.Action == "attach" && r.Principal == "log" {
				t.Fatal("a hidden attach was journaled")
			}
		}
	}
	hc.Detach()
	for _, r := range j.recs {
		if r.Action == "detach" && r.Principal == "log" {
			t.Fatal("a hidden detach was journaled")
		}
	}
}
