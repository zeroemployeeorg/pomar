package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
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

func startWith(t *testing.T, root, op string) (map[string]string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	args := []string{"-root", root, "-environment", "env-1", "-start"}
	if op != "" {
		args = append(args, "-operation-id", op)
	}
	err := run(args, stdinWith(t, ""), &out, &errb)
	var started map[string]string
	json.Unmarshal(out.Bytes(), &started)
	return started, err
}

func completeWith(t *testing.T, root, loginID, op string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	args := []string{"-root", root, "-environment", "env-1", "-complete", loginID}
	if op != "" {
		args = append(args, "-operation-id", op)
	}
	err := run(args, stdinWith(t, code+"\n"), &out, &errb)
	return out.String() + errb.String(), err
}

// A start under an explicit operation ID reports that ID; retrying it returns
// the retained operation, never a second sign-in and never the URL again.
func TestStartOperationIDIsKeptOnRetry(t *testing.T) {
	root, _, calls := hostWith(t)
	started, err := startWith(t, root, "login-op-1")
	if err != nil || started["operation_id"] != "login-op-1" || !strings.HasPrefix(started["auth_url"], "https://") {
		t.Fatalf("start: %v %v", started, err)
	}
	again, err := startWith(t, root, "login-op-1")
	if err == nil || !strings.Contains(err.Error(), "already recorded") || again["auth_url"] != "" {
		t.Fatalf("a retried start: %v %v", again, err)
	}
	if s, _ := callLog(t, calls); s != 1 {
		t.Fatalf("the actor started %d sign-ins", s)
	}
}

// A completion under an explicit operation ID is kept on retry: the original
// outcome is reported and the actor never completes twice. The same ID bound
// to another login is refused.
func TestCompleteOperationIDIsKeptOnRetry(t *testing.T) {
	root, _, calls := hostWith(t)
	started, err := startWith(t, root, "login-op-2")
	if err != nil {
		t.Fatal(err)
	}
	out, err := completeWith(t, root, started["login_id"], "complete-op-2")
	if err != nil || !strings.Contains(out, "signed in: authenticated: true") || !strings.Contains(out, "operation: complete-op-2") {
		t.Fatalf("complete: %q %v", out, err)
	}
	out, err = completeWith(t, root, started["login_id"], "complete-op-2")
	if err != nil || !strings.Contains(out, "already recorded; not repeated") {
		t.Fatalf("a retried completion: %q %v", out, err)
	}
	if _, c := callLog(t, calls); c != 1 {
		t.Fatalf("the actor completed %d sign-ins", c)
	}
	if out, err = completeWith(t, root, "another-login-id", "complete-op-2"); err == nil {
		t.Fatalf("the same operation ID for another login was accepted: %q", out)
	}
	if strings.Contains(out, code) {
		t.Fatal("the code was printed")
	}
}

// An operation ID recorded for one action is refused for the other, and
// nothing is signed in under it.
func TestOperationIDForTheOtherActionIsRefused(t *testing.T) {
	root, _, calls := hostWith(t)
	started, err := startWith(t, root, "shared-op")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := completeWith(t, root, started["login_id"], "shared-op"); err == nil {
		t.Fatalf("a start's operation ID was accepted for a completion: %q", out)
	}
	if _, c := callLog(t, calls); c != 0 {
		t.Fatalf("the actor completed %d sign-ins", c)
	}
}

// -operation-id belongs to exactly one of -start or -complete, and must be a
// valid operation ID; both are refused before any request.
func TestOperationIDIsValidatedBeforeAnyRequest(t *testing.T) {
	root, _, calls := hostWith(t)
	for name, args := range map[string][]string{
		"with neither":        {"-operation-id", "op-1"},
		"with both":           {"-start", "-complete", "x", "-operation-id", "op-1"},
		"with -status":        {"-status", "-operation-id", "op-1"},
		"an invalid ID":       {"-start", "-operation-id", "../op"},
		"an ID with a space":  {"-start", "-operation-id", "op 1"},
		"an ID starting a -":  {"-start", "-operation-id", "-op"},
		"an ID over 128 long": {"-start", "-operation-id", strings.Repeat("a", 129)},
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
