package seatterm

import "testing"

// council P3 on #137: ResetAgentUnknown is called by a caller (sessiond's
// adapter) that decided a reset was due under its OWN lock, then releases
// it before actually calling this — a different goroutine, under this
// session's lock, can report a fresh state in that exact gap. The fence
// (ifStateSeq) makes a stale decision a deliberate no-op instead of a
// clobber: it only resets if StateSeq still equals what the caller saw
// when it decided to reset.
func TestResetAgentUnknownIsFencedAgainstAFreshReport(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-fence-1", "operator", sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReportAgent("waiting-input", "", ""); err != nil {
		t.Fatal(err)
	}
	stale, _ := s.Agent()

	// A fresh report lands before the fenced reset is actually called --
	// exactly the race the fence exists for.
	if _, err := s.ReportAgent("running", "", ""); err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.Agent()
	if fresh.StateSeq == stale.StateSeq {
		t.Fatalf("the fresh report should have moved StateSeq on: stale=%d fresh=%d", stale.StateSeq, fresh.StateSeq)
	}

	// The reset, decided against the now-stale seq, must be a no-op.
	s.ResetAgentUnknown(stale.StateSeq)
	after, _ := s.Agent()
	if after.State != "running" || after.StateSeq != fresh.StateSeq {
		t.Fatalf("a reset fenced against a stale seq must not clobber the fresh report: %+v", after)
	}

	// Decided against the CURRENT seq, it does reset.
	s.ResetAgentUnknown(after.StateSeq)
	final, _ := s.Agent()
	if final.State != "unknown" {
		t.Fatalf("a reset fenced against the current seq should still fire: %+v", final)
	}
}

// council P2-2 on #137: Accepted is a past fact, not a claim about the
// program's current state. A reset must clear State and Detail but never
// un-confirm an acceptance the program already reported.
func TestResetAgentUnknownKeepsAcceptedDirectly(t *testing.T) {
	h, _ := newHost(t)
	s, _, err := h.Create("op-fence-2", "operator", sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportAgent("waiting-input", "a detail", "in-1"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Agent()
	if before.Accepted != "in-1" || before.Detail != "a detail" {
		t.Fatalf("setup: %+v", before)
	}

	s.ResetAgentUnknown(before.StateSeq)
	after, _ := s.Agent()
	if after.State != "unknown" {
		t.Fatalf("state should reset: %+v", after)
	}
	if after.Detail != "" {
		t.Fatalf("detail is part of the current state and should clear: %+v", after)
	}
	if after.Accepted != "in-1" {
		t.Fatalf("accepted is a past fact and must survive the reset: %+v", after)
	}
}
