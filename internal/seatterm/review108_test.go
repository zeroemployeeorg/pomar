// The POMAR Codex's fixture from its review of #108 at 0285806e (zmsg
// msg-pomar-pomar-review108-finite-deadline-20261009-v1, sha256 41ccf0d1...),
// formatted; its logic is unchanged.

package seatterm

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// A permitted stream can accept cancellation of a deadline yet refuse a
// later finite deadline. Teardown must return its error without a blocking
// final Close frame or acquisition of the still-held screen writer mutex.
func TestReviewerFiniteDeadlineFailureCannotHoldFinalClose(t *testing.T) {
	guest, near := net.Pipe()
	stream := &reviewerFiniteFailure{Conn: guest, writing: make(chan struct{}), finite: make(chan struct{})}
	done := make(chan error, 1)
	finished := false
	defer func() {
		near.Close()
		guest.Close()
		if !finished {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("fixture did not finish after owned endpoints closed")
			}
		}
	}()
	go func() {
		done <- Attach(stream, Command{Path: "/bin/sh", Args: []string{"-c", "printf x"}, UID: -1, GID: -1}, 80, 24)
	}()
	select {
	case <-stream.writing:
	case err := <-done:
		finished = true
		t.Fatalf("attach ended before writer fixture: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("writer fixture setup failed")
	}
	select {
	case <-stream.finite:
	case err := <-done:
		finished = true
		t.Fatalf("attach ended before finite-deadline fixture: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("finite-deadline fixture setup failed")
	}
	select {
	case err := <-done:
		finished = true
		if err == nil {
			t.Fatal("finite deadline error was lost")
		}
	case <-time.After(DrainTimeout + 2*time.Second):
		t.Fatal("Attach still blocked after failed finite deadline and its bounded screen wait; final Close send waits on screen writer")
	}
}

type reviewerFiniteFailure struct {
	net.Conn
	writing    chan struct{}
	finite     chan struct{}
	writeOnce  sync.Once
	finiteOnce sync.Once
}

func (s *reviewerFiniteFailure) Write(p []byte) (int, error) {
	s.writeOnce.Do(func() { close(s.writing) })
	return s.Conn.Write(p)
}
func (s *reviewerFiniteFailure) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() {
		s.finiteOnce.Do(func() { close(s.finite) })
		return errors.New("reviewer fixture refuses finite write deadlines")
	}
	return s.Conn.SetWriteDeadline(t)
}
