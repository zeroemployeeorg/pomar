package manager

import (
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/proc"
)

const hostBin = "/opt/pomar/pomar-host"
const me = 503

func helperArgs(attempt string) string {
	return hostBin + " helper --attempt " + attempt + " --state-dir d -- /bin/sh -c true"
}

func find(fs []Finding, attempt string) (Finding, bool) {
	for _, f := range fs {
		if f.Attempt == attempt {
			return f, true
		}
	}
	return Finding{}, false
}

func TestReconcile(t *testing.T) {
	entries := []Entry{
		{Attempt: "alive", PID: 100, Start: "T1", State: StateRunning},
		{Attempt: "gone", PID: 101, Start: "T1", State: StateRunning},
		{Attempt: "reused-start", PID: 102, Start: "T1", State: StateRunning},
		{Attempt: "reused-other", PID: 103, Start: "T1", State: StateRunning},
		{Attempt: "reused-uid", PID: 104, Start: "T1", State: StateRunning},
		{Attempt: "reused-helper", PID: 105, Start: "T1", State: StateRunning},
		{Attempt: "done", PID: 106, Start: "T1", State: StateExited},
	}
	ps := []proc.Process{
		{PID: 100, UID: me, Start: "T1", Args: helperArgs("alive")},
		{PID: 102, UID: me, Start: "T2", Args: helperArgs("reused-start")},
		{PID: 103, UID: me, Start: "T1", Args: "/usr/bin/vim notes"},
		{PID: 104, UID: 0, Start: "T1", Args: helperArgs("reused-uid")},
		{PID: 105, UID: me, Start: "T1", Args: helperArgs("someone-else")},
		{PID: 106, UID: me, Start: "T1", Args: "/bin/zsh"}, // pid of a terminal entry, reused
		{PID: 200, UID: me, Start: "T1", Args: helperArgs("orphan")},
		{PID: 201, UID: 0, Start: "T1", Args: helperArgs("root-owned")},
		{PID: 202, UID: me, Start: "T1", Args: "/other/pomar-host helper --attempt x"},
		{PID: 203, UID: me, Start: "T1", Args: hostBin + " boot-smoke --attempt y"},
	}
	fs := Reconcile(entries, ps, me, hostBin)

	type pair struct {
		attempt string
		d       Decision
	}
	want := map[pair]bool{
		{"alive", Adopted}:       true,
		{"gone", Lost}:           true,
		{"reused-start", Lost}:   true,
		{"reused-other", Lost}:   true,
		{"reused-uid", Lost}:     true,
		{"reused-helper", Lost}:  true,
		{"orphan", Orphan}:       true,
		{"someone-else", Orphan}: true, // pid 105: a helper no entry claims
		{"reused-start", Orphan}: true, // pid 102: a newer helper for the same attempt is not the recorded one
	}
	got := map[pair]bool{}
	for _, f := range fs {
		got[pair{f.Attempt, f.Decision}] = true
	}
	for p := range want {
		if !got[p] {
			t.Errorf("missing %s %s", p.attempt, p.d)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("unexpected %s %s", p.attempt, p.d)
		}
	}
	if len(fs) != len(want) {
		t.Errorf("findings = %d, want %d: %+v", len(fs), len(want), fs)
	}
	for _, never := range []string{"done", "root-owned", "x", "y"} {
		if f, ok := find(fs, never); ok {
			t.Errorf("%s must not be reported: %+v", never, f)
		}
	}
}

func TestHelperAttempt(t *testing.T) {
	cases := []struct {
		args string
		want string
		ok   bool
	}{
		{helperArgs("a1"), "a1", true},
		{hostBin + " helper --state-dir d -- /bin/sh --attempt fake", "", false},
		{hostBin + "x helper --attempt a1", "", false},
		{"/bin/echo " + hostBin + " helper --attempt a1", "", false},
	}
	for _, c := range cases {
		got, ok := helperAttempt(c.args, hostBin)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q %v, want %q %v", c.args, got, ok, c.want, c.ok)
		}
	}
}
