package manager

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/proc"
)

// limited is the CI class with a one-second time limit.
func limited() capacity.Class {
	c := capacity.CI
	c.TimeLimitSeconds = 1
	return c
}

// An attempt past its class's time limit is sent SIGTERM and marked; once
// its helper is gone it ends timed-out, with its exit code kept, and the
// signed result's state says so.
func TestAttemptPastItsTimeLimitIsStoppedAndEndsTimedOut(t *testing.T) {
	pid, done := sleeper(t)
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openFull(t, procs, 100*capacity.GiB, time.Hour)
	procs.lists, procs.n = [][]proc.Process{{helperProc("a", pid)}}, 0
	m.t.entries["a"] = &Entry{Attempt: "a", PID: pid, Start: "T1", State: StateRunning, Class: limited(),
		Created: time.Now().Add(-time.Hour)}
	writeStatus(t, m, "a", `{"phase":"running"}`)

	m.poll()
	if e := entry(m, "a"); e.State != StateStopping || !e.TimedOut {
		t.Fatalf("after the limit: %+v, want stopping and timed out", e)
	}
	select {
	case err := <-done:
		ws, _ := err.(*exec.ExitError)
		if ws == nil || ws.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Fatalf("child ended with %v, want SIGTERM", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper past its limit was not signalled")
	}
	// The helper reports stopped, with an exit code, and is gone.
	writeStatus(t, m, "a", `{"phase":"exited","exit_code":"143"}`)
	procs.lists, procs.n = [][]proc.Process{nil}, 0
	m.poll()
	e := entry(m, "a")
	if e.State != StateTimedOut || !strings.HasPrefix(e.Reason, "time limit 1s (") {
		t.Fatalf("after it ended: %+v, want timed-out", e)
	}
	if e.ExitCode == nil || *e.ExitCode != 143 {
		t.Fatalf("exit code lost: %v", e.ExitCode)
	}
	if !e.Terminal() {
		t.Fatal("timed-out is not terminal")
	}
	if got := (&Manager{}).resultDoc(e)["state"]; got != string(StateTimedOut) {
		t.Fatalf("result state = %v", got)
	}
}

// Within its limit an attempt is left alone. Its pid is the test's own: were
// it ever signalled, the test would die rather than a stranger.
func TestAttemptWithinItsTimeLimitIsLeftRunning(t *testing.T) {
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openFull(t, procs, 100*capacity.GiB, time.Hour)
	procs.lists, procs.n = [][]proc.Process{{helperProc("a", os.Getpid())}}, 0
	c := limited()
	c.TimeLimitSeconds = 3600
	m.t.entries["a"] = &Entry{Attempt: "a", PID: os.Getpid(), Start: "T1", State: StateRunning, Class: c, Created: time.Now()}
	writeStatus(t, m, "a", `{"phase":"running"}`)
	m.poll()
	if e := entry(m, "a"); e.State != StateRunning || e.TimedOut {
		t.Fatalf("entry = %+v, want running", e)
	}
}

// A class with no time limit never stops an attempt, however old.
func TestNoTimeLimitMeansNone(t *testing.T) {
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openFull(t, procs, 100*capacity.GiB, time.Hour)
	procs.lists, procs.n = [][]proc.Process{{helperProc("a", os.Getpid())}}, 0
	c := limited()
	c.TimeLimitSeconds = 0
	m.t.entries["a"] = &Entry{Attempt: "a", PID: os.Getpid(), Start: "T1", State: StateRunning, Class: c,
		Created: time.Now().Add(-100 * time.Hour)}
	writeStatus(t, m, "a", `{"phase":"running"}`)
	m.poll()
	if e := entry(m, "a"); e.State != StateRunning || e.TimedOut {
		t.Fatalf("entry = %+v, want running", e)
	}
}

// A helper that ignores SIGTERM is killed once a further StallWait passes.
func TestHelperIgnoringTheTimeLimitIsKilled(t *testing.T) {
	child := exec.Command("/bin/sh", "-c", `trap "" TERM; while :; do sleep 1; done`)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	t.Cleanup(func() { child.Process.Kill() })
	pid := child.Process.Pid
	time.Sleep(200 * time.Millisecond) // let the shell set its trap

	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openFull(t, procs, 100*capacity.GiB, 300*time.Millisecond)
	procs.lists, procs.n = [][]proc.Process{{helperProc("a", pid)}}, 0
	m.t.entries["a"] = &Entry{Attempt: "a", PID: pid, Start: "T1", State: StateRunning, Class: limited(),
		Created: time.Now().Add(-time.Hour)}
	writeStatus(t, m, "a", `{"phase":"running"}`)
	m.poll() // SIGTERM, ignored
	select {
	case err := <-done:
		t.Fatalf("the child ended on SIGTERM (%v); the test needs it to ignore it", err)
	case <-time.After(300 * time.Millisecond):
	}
	m.poll() // an hour past the limit, more than StallWait: SIGKILL
	select {
	case err := <-done:
		ws, _ := err.(*exec.ExitError)
		if ws == nil || ws.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("child ended with %v, want SIGKILL", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper ignoring SIGTERM was not killed")
	}
}

// A host condition is named ahead of the time limit: the host, not the job,
// compromised the attempt.
func TestHostConditionIsNamedAheadOfTheTimeLimit(t *testing.T) {
	m := openFull(t, &seqProcs{lists: [][]proc.Process{nil}}, 100*capacity.GiB, time.Hour)
	m.t.entries["a"] = &Entry{Attempt: "a", PID: 100, Start: "T1", State: StateStopping, Class: limited(),
		TimedOut: true, HostCondition: capacity.ReasonHostDiskFull}
	writeStatus(t, m, "a", `{"phase":"stopped"}`)
	m.poll()
	if e := entry(m, "a"); e.State != StateFailed || !strings.HasPrefix(e.Reason, capacity.ReasonHostDiskFull) {
		t.Fatalf("entry = %+v, want failed host-disk-full", e)
	}
}

// An entry with no admission time is never judged against a limit. Its pid is
// the test's own, as in the tests that build entries without one: judged, it
// would read as decades past the limit and signal the test itself.
func TestEntryWithoutAdmissionTimeIsNeverJudged(t *testing.T) {
	procs := &seqProcs{lists: [][]proc.Process{nil}}
	m := openFull(t, procs, 100*capacity.GiB, time.Hour)
	procs.lists, procs.n = [][]proc.Process{{helperProc("a", os.Getpid())}}, 0
	m.t.entries["a"] = &Entry{Attempt: "a", PID: os.Getpid(), Start: "T1", State: StateRunning, Class: limited()}
	writeStatus(t, m, "a", `{"phase":"running"}`)
	m.poll()
	if e := entry(m, "a"); e.State != StateRunning || e.TimedOut {
		t.Fatalf("entry = %+v, want running", e)
	}
}

// One attempt is read through the control socket without paging the list.
func TestGetOneAttemptThroughTheControlSocket(t *testing.T) {
	m, ctl, _ := openSigning(t, false)
	e := terminalEntry("one-1")
	m.mu.Lock()
	m.t.entries["one-1"] = &e
	m.mu.Unlock()
	var got Entry
	if err := ctl.Do("GET", "/v1/attempts/one-1", nil, &got); err != nil || got.Attempt != "one-1" || got.State != StateExited {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := ctl.Do("GET", "/v1/attempts/no-such", nil, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a missing attempt: %v, want 404", err)
	}
	if err := ctl.Do("GET", "/v1/attempts/Bad_ID", nil, nil); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("an invalid id: %v, want 400", err)
	}
}
