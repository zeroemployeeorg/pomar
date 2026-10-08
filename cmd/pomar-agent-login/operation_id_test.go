package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// callLog reads the fake sign-in's call log: how often the actor really
// started or completed a sign-in.
func callLog(t *testing.T, calls string) (starts, completes int) {
	t.Helper()
	data, _ := os.ReadFile(calls)
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch l {
		case "start":
			starts++
		case "complete":
			completes++
		}
	}
	return starts, completes
}

// reported is the scripted modes' JSON report.
type reported struct {
	Outcome                   string `json:"outcome"`
	Cached                    bool   `json:"cached"`
	Transport                 string `json:"transport_outcome"`
	OperationState            string `json:"operation_state"`
	SessionState              string `json:"session_state"`
	ResponseAvailable         bool   `json:"response_available"`
	RetainedResponseAvailable bool   `json:"retained_response_available"`
	EventID                   string `json:"event_id"`
	Succeeded                 bool   `json:"succeeded"`
	LoginID                   string `json:"login_id"`
	AuthURL                   string `json:"auth_url"`
}

func records(root string) string { return filepath.Join(root, "records") }

func startWith(t *testing.T, root, recs, op string, extra ...string) (reported, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"-root", root, "-environment", "env-1", "-start", "-operation-id", op, "-records", recs}, extra...)
	err := run(args, stdinWith(t, ""), &out, &errb)
	var r reported
	json.Unmarshal(out.Bytes(), &r)
	return r, out.String() + errb.String(), err
}

func completeWith(t *testing.T, root, recs, loginID, op, stdin string, extra ...string) (reported, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"-root", root, "-environment", "env-1", "-complete", loginID, "-operation-id", op, "-records", recs}, extra...)
	err := run(args, stdinWith(t, stdin), &out, &errb)
	var r reported
	json.Unmarshal(out.Bytes(), &r)
	return r, out.String() + errb.String(), err
}

// A start's one-time reply is retained in the caller's journal: a rerun under
// the same operation ID (a lost stdout, a restart) reads it back, marked as
// cached, and the actor never starts a second sign-in.
func TestStartReplyIsRetainedAndReadBack(t *testing.T) {
	root, _, calls := hostWith(t)
	first, _, err := startWith(t, root, records(root), "login.op-1")
	if err != nil || !first.Succeeded || first.Cached || first.Outcome != "response-available" || !strings.HasPrefix(first.AuthURL, "https://") || first.LoginID == "" || first.EventID == "" {
		t.Fatalf("start: %+v %v", first, err)
	}
	again, _, err := startWith(t, root, records(root), "login.op-1")
	if err != nil || !again.Succeeded || !again.Cached || again.AuthURL != first.AuthURL || again.LoginID != first.LoginID || again.EventID != first.EventID {
		t.Fatalf("a rerun start: %+v %v", again, err)
	}
	if s, _ := callLog(t, calls); s != 1 {
		t.Fatalf("the actor started %d sign-ins", s)
	}
}

// A completion is retained with its own success: a rerun reads it back
// without the code and without a second completion; the code file is removed
// once the reply is durable, and the code is never printed or journaled.
func TestCompletionIsRetainedAndItsCodeConsumed(t *testing.T) {
	root, _, calls := hostWith(t)
	started, _, err := startWith(t, root, records(root), "login-op-2")
	if err != nil {
		t.Fatal(err)
	}
	done, out, err := completeWith(t, root, records(root), started.LoginID, "complete-op-2", code+"\n")
	if err != nil || !done.Succeeded || done.Outcome != "response-available" || done.SessionState != "authenticated" {
		t.Fatalf("complete: %+v %q %v", done, out, err)
	}
	if _, e := os.Lstat(filepath.Join(records(root), ".requests", "complete-op-2.json")); !os.IsNotExist(e) {
		t.Fatal("the code file outlived the durable reply")
	}
	again, out, err := completeWith(t, root, records(root), started.LoginID, "complete-op-2", "")
	if err != nil || !again.Succeeded || !again.Cached {
		t.Fatalf("a rerun completion: %+v %q %v", again, out, err)
	}
	if _, c := callLog(t, calls); c != 1 {
		t.Fatalf("the actor completed %d sign-ins", c)
	}
	journal := ""
	filepath.Walk(records(root), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			journal += string(b)
		}
		return nil
	})
	if strings.Contains(out, code) || strings.Contains(journal, code) {
		t.Fatal("the code was printed or journaled")
	}
}

// The same operation ID bound to another login, or to the other action, is
// refused, and nothing is signed in under it.
func TestOperationIDIsBoundToItsRequest(t *testing.T) {
	root, _, calls := hostWith(t)
	started, _, err := startWith(t, root, records(root), "bound-op")
	if err != nil {
		t.Fatal(err)
	}
	if r, out, err := completeWith(t, root, records(root), started.LoginID, "bound-op", code+"\n"); err == nil || r.Succeeded {
		t.Fatalf("a start's operation ID was accepted for a completion: %q", out)
	}
	if _, _, err := completeWith(t, root, records(root), started.LoginID, "complete-bound", "wrong-code-0000\n"); err == nil {
		t.Fatal("a wrong code succeeded")
	}
	if r, out, err := completeWith(t, root, records(root), "another-login-id", "complete-bound", code+"\n"); err == nil || r.Succeeded {
		t.Fatalf("the same operation ID for another login was accepted: %q", out)
	}
	if _, c := callLog(t, calls); c != 0 {
		t.Fatalf("the actor completed %d sign-ins", c)
	}
}

// A completion still dispatching is not success, even when the account is
// already authenticated by an earlier sign-in: a second client of the same
// records is refused locally, a client of other records gets only the
// broker's retained control, and the actor completes once.
func TestRetainedDispatchingCompletionIsNotSuccess(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	root, config, calls := hostGated(t, gate)
	os.WriteFile(filepath.Join(config, ".credentials.json"), []byte("earlier-sign-in"), 0o600)
	started, _, err := startWith(t, root, records(root), "held-start")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		_, _, err := completeWith(t, root, records(root), started.LoginID, "held-complete", code+"\n")
		first <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if data, _ := os.ReadFile(calls); strings.Contains(string(data), "waiting") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first completion never reached the actor")
		}
	}
	if r, out, err := completeWith(t, root, records(root), started.LoginID, "held-complete", code+"\n"); err == nil || r.Succeeded {
		t.Fatalf("a concurrent client of the same records: %q %v", out, err)
	}
	other := filepath.Join(root, "other-records")
	os.Mkdir(other, 0o700)
	r, out, err := completeWith(t, root, other, started.LoginID, "held-complete", code+"\n")
	if err == nil || r.Succeeded || r.OperationState != "dispatching" || r.SessionState != "authenticated" {
		t.Fatalf("a retained dispatching control: %+v %q %v", r, out, err)
	}
	os.WriteFile(gate, nil, 0o600)
	if err := <-first; err != nil {
		t.Fatalf("the first completion: %v", err)
	}
	if _, c := callLog(t, calls); c != 1 {
		t.Fatalf("the actor completed %d sign-ins", c)
	}
}

// Flags are refused before any request: modes are exclusive, the scripted
// modes need an operation ID and records, the recovery flags belong to them,
// and the ID must be the broker's.
func TestFlagsAreValidatedBeforeAnyRequest(t *testing.T) {
	root, _, calls := hostWith(t)
	recs := records(root)
	os.Mkdir(filepath.Join(root, "open"), 0o755)
	for name, args := range map[string][]string{
		"-operation-id alone":         {"-operation-id", "op-1"},
		"-records alone":              {"-records", recs},
		"-reconcile alone":            {"-reconcile"},
		"-start and -complete":        {"-start", "-complete", "x", "-operation-id", "op-1", "-records", recs},
		"-status with -start":         {"-status", "-start", "-operation-id", "op-1", "-records", recs},
		"-status with -complete":      {"-status", "-complete", "x", "-operation-id", "op-1", "-records", recs},
		"-status with -operation-id":  {"-status", "-operation-id", "op-1"},
		"-start with no ID":           {"-start", "-records", recs},
		"-complete with no ID":        {"-complete", "x", "-records", recs},
		"-start with no records":      {"-start", "-operation-id", "op-1"},
		"retry without -reconcile":    {"-start", "-operation-id", "op-1", "-records", recs, "-retry-known-unaccepted"},
		"an invalid ID":               {"-start", "-operation-id", "../op", "-records", recs},
		"an ID with a space":          {"-start", "-operation-id", "op 1", "-records", recs},
		"an ID starting with a -":     {"-start", "-operation-id", "-op", "-records", recs},
		"an ID over 128 long":         {"-start", "-operation-id", strings.Repeat("a", 129), "-records", recs},
		"records that aren't private": {"-start", "-operation-id", "op-1", "-records", filepath.Join(root, "open")},
	} {
		var out, errb bytes.Buffer
		if err := run(append([]string{"-root", root, "-environment", "env-1"}, args...), stdinWith(t, ""), &out, &errb); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if s, c := callLog(t, calls); s != 0 || c != 0 {
		t.Fatalf("a refused flag reached the actor (%d starts, %d completions)", s, c)
	}
}

// A completion whose reply arrives but can't be retained is uncertain, never
// success: a restart under the same ID stays uncertain, a fresh
// reconciliation observes only the broker's control (it was sent, its result
// unknown), the code file is kept for inspection, and the actor completes
// once.
func TestUnretainedCompletionReplyStaysUncertain(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	root, _, calls := hostGated(t, gate)
	started, _, err := startWith(t, root, records(root), "lost-start")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan reported, 1)
	go func() {
		r, _, _ := completeWith(t, root, records(root), started.LoginID, "lost-complete", code+"\n")
		first <- r
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if data, _ := os.ReadFile(calls); strings.Contains(string(data), "waiting") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the completion never reached the actor")
		}
	}
	journal := filepath.Join(records(root), "lost-complete")
	os.Chmod(journal, 0o500)
	os.WriteFile(gate, nil, 0o600)
	r := <-first
	os.Chmod(journal, 0o700)
	if r.Succeeded || r.Transport != "response-not-durable" {
		t.Fatalf("an unretained reply: %+v", r)
	}
	if r, out, err := completeWith(t, root, records(root), started.LoginID, "lost-complete", ""); err == nil || r.Succeeded {
		t.Fatalf("a restart: %+v %q", r, out)
	}
	r, out, err := completeWith(t, root, records(root), started.LoginID, "lost-complete", "", "-reconcile")
	if err == nil || r.Succeeded || r.Cached || r.OperationState != "sent" || r.ResponseAvailable || r.RetainedResponseAvailable {
		t.Fatalf("a fresh reconciliation: %+v %q %v", r, out, err)
	}
	if _, e := os.Lstat(filepath.Join(records(root), ".requests", "lost-complete.json")); e != nil {
		t.Fatal("the private request was removed without a durable reply")
	}
	if _, c := callLog(t, calls); c != 1 {
		t.Fatalf("the actor completed %d sign-ins", c)
	}
}

// After a fresh reconciliation the latest event is that observation, but the
// retained one-time reply still establishes the start: a later rerun reads
// it back through the retained-response pointer, with no second sign-in.
func TestRetainedReplySurvivesALaterReconciliation(t *testing.T) {
	root, _, calls := hostWith(t)
	first, _, err := startWith(t, root, records(root), "seen-start")
	if err != nil || !first.Succeeded {
		t.Fatalf("start: %+v %v", first, err)
	}
	observed, out, err := startWith(t, root, records(root), "seen-start", "-reconcile")
	if !observed.Succeeded || observed.Cached || !observed.RetainedResponseAvailable || observed.AuthURL != first.AuthURL || err != nil {
		t.Fatalf("a fresh reconciliation: %+v %q %v", observed, out, err)
	}
	again, out, err := startWith(t, root, records(root), "seen-start")
	if err != nil || !again.Succeeded || again.AuthURL != first.AuthURL {
		t.Fatalf("a rerun after reconciliation: %+v %q %v", again, out, err)
	}
	if s, _ := callLog(t, calls); s != 1 {
		t.Fatalf("the actor started %d sign-ins", s)
	}
}

// The client dies after the broker accepted the start and its one-time reply
// arrived, but before that reply was retained: the journal holds the sealed
// intent and the dispatch, and nothing else. The URL is lost, and that is
// reported, not resolved: a restart is uncertain, a fresh reconciliation
// sees only that the start was sent, a retry is refused because nothing
// proves non-acceptance, and the actor never starts a second sign-in.
func TestReplyLostBeforeRetentionIsReportedNotReplayed(t *testing.T) {
	root, config, calls := hostWith(t)
	os.WriteFile(filepath.Join(config, ".credentials.json"), []byte("earlier-sign-in"), 0o600)
	first, _, err := startWith(t, root, records(root), "died-start")
	if err != nil || !first.Succeeded {
		t.Fatalf("start: %+v %v", first, err)
	}
	// The state a death between receipt and retention leaves behind.
	if err := os.Remove(filepath.Join(records(root), "died-start", first.EventID+".json")); err != nil {
		t.Fatal(err)
	}
	restart, out, err := startWith(t, root, records(root), "died-start")
	if err == nil || restart.Succeeded || restart.AuthURL != "" || restart.Transport != "lost-or-unrecorded" {
		t.Fatalf("a restart: %+v %q %v", restart, out, err)
	}
	seen, out, err := startWith(t, root, records(root), "died-start", "-reconcile")
	if err == nil || seen.Succeeded || seen.AuthURL != "" || seen.OperationState != "sent" || seen.RetainedResponseAvailable || seen.SessionState != "authenticated" {
		t.Fatalf("a fresh reconciliation: %+v %q %v", seen, out, err)
	}
	retry, out, err := startWith(t, root, records(root), "died-start", "-reconcile", "-retry-known-unaccepted")
	if err == nil || retry.Succeeded || retry.AuthURL != "" {
		t.Fatalf("a retry without proof of non-acceptance: %+v %q %v", retry, out, err)
	}
	if s, _ := callLog(t, calls); s != 1 {
		t.Fatalf("the actor started %d sign-ins", s)
	}
}
