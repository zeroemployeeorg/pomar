package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/manager"
)

// The agent experience: what lets a program, an agent or a script, use
// Pomar without reading English output. Every command an agent needs
// prints JSON, has a fixed exit code, and says what to do next when it
// can't go on. Nothing here depends on which agent is calling.

// Exit codes of `pomar run -wait`, beyond the job's own. They are listed by
// `pomar help -json`, and are stable: an agent may branch on them.
const (
	exitWaitGaveUp = 75  // -timeout passed and the job is still running; it was not stopped
	exitTimedOut   = 124 // the job was stopped by its class's time limit
	exitPomar      = 125 // Pomar couldn't run the job to its end: it failed, or its helper was lost
	exitStopped    = 143 // the job was stopped on request
)

// noManager is what every command that needs a manager says when none is
// selected. Pomar never guesses one.
const noManager = "no manager selected: set POMAR_SOCKET to the control socket your host owner gave you, or pass -socket PATH (the owner on their own host may pass -root DIR). Pomar never guesses one; `pomar doctor` checks the one you name."

// waitFor polls an attempt until it ends, or until timeout (0: no limit).
// It never stops the attempt. The timeout bounds the polls themselves: a
// manager that stops answering mid-poll can't hold the wait past it, and
// the wait then reports the last state it saw.
func waitFor(c *manager.Client, id string, timeout time.Duration) (manager.Entry, bool, error) {
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	interval := 250 * time.Millisecond
	var last manager.Entry
	for {
		poll, done := context.WithTimeout(ctx, manager.CallBound)
		var e manager.Entry
		err := c.DoContext(poll, "GET", "/v1/attempts/"+id, nil, &e)
		done()
		if ctx.Err() != nil {
			return last, false, nil // the wait's own limit passed
		}
		if err != nil {
			return last, false, err
		}
		if e.Terminal() {
			return e, true, nil
		}
		last = e
		select {
		case <-ctx.Done():
			return last, false, nil
		case <-time.After(interval):
		}
		if interval < 2*time.Second {
			interval *= 2
		}
	}
}

// jobExitCode is the exit code `pomar run -wait` returns for an ended
// attempt: the job's own when it exited, and Pomar's fixed codes when it
// didn't.
func jobExitCode(e manager.Entry) int {
	switch e.State {
	case manager.StateExited:
		if e.ExitCode != nil {
			if c := *e.ExitCode; c >= 0 && c <= 255 {
				return c
			}
		}
		return exitPomar
	case manager.StateTimedOut:
		return exitTimedOut
	case manager.StateStopped:
		return exitStopped
	}
	return exitPomar
}

// notEnded explains a log or result asked for before the job ended.
func notEnded(c *manager.Client, id string, stderr io.Writer) bool {
	var e manager.Entry
	if c.Do("GET", "/v1/attempts/"+id, nil, &e) != nil || e.Terminal() {
		return false
	}
	fmt.Fprintf(stderr, "%s hasn't ended yet (state %s). Wait for it with `pomar wait -id %s`, then ask again.\n", id, e.State, id)
	return true
}

// command is one entry of the machine-readable catalogue.
type command struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Usage   string   `json:"usage"`
	Output  string   `json:"output"`
	Notes   []string `json:"notes,omitempty"`
}

// helpCatalogue is what `pomar help -json` prints: enough for an agent to use
// Pomar without parsing prose.
func helpCatalogue() map[string]any {
	return map[string]any{
		"schema":  "pomar.help/v1",
		"version": version,
		"source":  sourceCommit,
		"select_manager": map[string]any{
			"env":     "POMAR_SOCKET",
			"flag":    "-socket PATH",
			"owner":   "-root DIR (the host owner, on their own host)",
			"guessed": false,
			"check":   "pomar doctor -json",
		},
		"loop": []string{
			"pomar doctor -json",
			"pomar health -json",
			"pomar status",
			"pomar run -wait -id ID -class CLASS -- COMMAND...",
			"pomar logs -id ID",
			"pomar attempt result -id ID",
			"pomar result verify -reply FILE -public-key INDEPENDENT_KEY",
		},
		"commands": []command{
			{Name: "actor-switch", Summary: "Switch a retained seat between owner-configured harness profiles after fencing", Usage: "pomar actor-switch -root DIR -environment ID -session ID -incarnation ID -id OP -profile TARGET [-status]", Output: "json"},
			{"doctor", "Check this client, its bundled tools and the selected manager", "pomar doctor [-socket PATH] [-json]",
				"text, or a JSON array of {name, ok, skipped, detail, next}", []string{"exit 0 when everything needed is usable, 1 otherwise"}},
			{"health", "Explain manager reachability, known current fit and unknowns", "pomar health [-socket PATH] [-json] [-expected-key-id HEX] [-timeout D]",
				"text by default; -json emits pomar.health/v1", []string{"GET observations only; neither a reservation nor signed execution", "legacy idle fit is never current fit"}},
			{"status", "The manager's classes, capacity and admission limits", "pomar status [-socket PATH]", "JSON", []string{"the classes you may run are under classes[].name"}},
			{"run", "Start a job in a fresh micro-VM", "pomar run [-wait [-timeout D]] -id ID [-class CLASS] [-mirror NAME -ref REF [-sha SHA]] [-input NAME=PATH]... [-output NAME]... -- COMMAND...",
				"JSON: the attempt record (at admission, or with -wait once it ended)",
				[]string{"IDs are unique: reusing one is refused (409)", "after an uncertain start, inspect the ORIGINAL ID; do not resubmit with a new ID", "without -wait, success means admitted, not finished",
					"with -wait, the exit code is the job's own, or one of the codes in exit_codes"}},
			{"wait", "Wait for an admitted job to end", "pomar wait -id ID [-timeout D]", "JSON: the attempt record once it ended", []string{"the same exit codes as run -wait"}},
			{"jobs", "List attempts", "pomar jobs [-socket PATH]", "JSON array of attempt records", nil},
			{"attempt get", "One attempt's record, including its state", "pomar attempt get -id ID", "JSON", nil},
			{"attempt progress", "A running attempt's phase and bounded provisional output", "pomar attempt progress -id ID", "JSON", []string{"older managers don't expose it"}},
			{"logs", "An ended job's output, checked against its result", "pomar logs -id ID [-public-key KEY] [-o PATH]", "the log's bytes", []string{"refused, with what to do, while the job is still running"}},
			{"attempt result", "The original result and signature reply", "pomar attempt result -id ID", "JSON reply containing original signed document bytes and signature", []string{"verify it with the owner's independently retained public key"}},
			{"result verify", "Verify original signed bytes against an independent public key", "pomar result verify -reply FILE -public-key BASE64", "text", []string{"offline; no manager required", "exit 0 only when the signature verifies; input/binding acceptance is the consumer's separate check"}},
			{"attempt output", "Retrieve a named terminal artifact with size and digest checks", "pomar attempt output -id ID -name NAME -o PATH", "JSON receipt; artifact bytes written to PATH", []string{"expected source/class/input/tool/output bindings and signature remain consumer checks"}},
			{"attempt pins", "Read the admitted tool and environment pins", "pomar attempt pins -id ID", "JSON", nil},
			{"attempt signing-key", "Read the manager public key", "pomar attempt signing-key", "JSON", []string{"this response is not its own independent trust anchor"}},
			{"attempt stop", "Stop a running job", "pomar attempt stop -id ID", "JSON", nil},
			{"seats", "List declared development seats", "pomar seats [-root DIR] [-json] [-menu]", "a table, or JSON with -json", []string{"never prompts unless -menu is given, which needs a terminal"}},
			// session: POMAR-CC's entry, for Pomar to fold into the catalogue's release form.
			{"session", "Named programs on terminals that outlive every client, from this user's session service",
				"pomar session serve|new -id OP [-state] [-expected-sha256 HEX] NAME -- /ABS/PROGRAM...|ls [-json]|show NAME|screen [-json] NAME|scrollback [-json] [-lines N] NAME|health NAME|logs [-from N] [-limit N] [-o PATH] NAME|send -id OP [-enter] [-session ID] [-epoch N] [-when-ready] [-accept-wait D] [-ack-unknown] NAME [--] TEXT|input NAME INPUT_ID|paste -id OP [-file PATH] [-raw] [-session ID] [-epoch N] [-ack-unknown] NAME|attach [-view] [-take] NAME|stop -id OP NAME",
				"JSON (attach is the terminal; screen and logs print raw text/bytes, or JSON with -json where offered)",
				[]string{"exit 3: no session service for the root; 4: no such name; 5: refused as it stands (a running name, another controller, an operation still running); 6: the program exited; 7: stale (the session changed since you read it); 8: unsupported (-when-ready without a state channel); 9: not ready in time; 10: an operation's outcome is unknown, pass -ack-unknown; 11: the send's text is over the 64 KiB bound, nothing was sent",
					"send and paste refuse (exit 10) while any operation on the session is acceptance_unknown, unless -ack-unknown; show lists them as held_operations",
					"input reads a send's current acceptance live (yes/pending/unsupported/unknown); distinct from a retried send, which returns its own frozen reply; neither re-sends anything",
					"-id makes new, send, paste and stop once-only: a retry with the same ID returns what it did, and never acts twice",
					"a send's or paste's reply tells queued, written and accepted apart; accepted only from the session's adapter",
					"paste is a big send: the same fencing and reply, its payload bracketed as a paste unless -raw; bounded at 8 MiB",
					"logs are the session's durable, paged output, on disk from creation; unlike everything else here, they survive a restart of the service (by the session's exact ID)",
					"agent state is adapter-reported, never read off the screen; unknown without an adapter",
					"native execution under your OS user, not a guest; sessions don't survive a restart of the service or a reboot", "docs/sessions.md, docs/sessions-protocol.md"}},
			{"seat status", "Read one declared seat's location", "pomar seat status [-root DIR] SEAT", "JSON", nil},
			{"version", "Client version and source commit", "pomar version", "text", nil},
		},
		"owner_commands": ownerCatalogue(),
		"command_flags":  catalogueFlags(),
		"flag_coverage":  "Structured flags cover the main job loop and health/result/artifact reads; owner commands retain usage strings and pomar help all. This is not an exhaustive flag parser schema.",
		"command_exit_codes": map[string]map[string]string{
			"health":        {"0": "capacity responded and any requested key pin passed; explicit unknowns remain", "1": "attention/read failure", "2": "incomplete invocation or manager selection"},
			"result verify": {"0": "signature verified", "1": "unreadable or invalid reply/key/signature encoding", "2": "invalid invocation", "3": "signature does not match"},
			"venue status":  {"0": "scoped storage report", "1": "read failure", "2": "invalid invocation", "3": "unaccounted objects"},
		},
		"schemas": map[string]string{"help": "pomar.help/v1", "health": "pomar.health/v1", "attempt progress": "pomar.attempt-progress/v1 (manager support required)", "doctor": "unversioned diagnostic array", "status": "unversioned manager Capacity JSON", "jobs": "unversioned manager Entry array", "attempt result": "signed reply; result document schema is verified separately"},
		"exit_codes": map[string]string{
			"0":     "success (with run -wait: the job exited 0)",
			"1":     "the command failed; stderr says why and what to do",
			"2":     "usage: a flag or argument is wrong",
			"75":    "run -wait gave up after -timeout; the job is still running and was not stopped",
			"124":   "run -wait: the job was stopped by its class's time limit",
			"125":   "run -wait: Pomar couldn't run the job to its end (failed, or its helper was lost); see the record's reason",
			"143":   "run -wait: the job was stopped on request",
			"other": "run -wait: the job's own exit code",
		},
		"never": []string{
			"guess a socket or data root",
			"use sudo or widen a socket's permissions when refused: that is the host owner's decision",
			"treat admission as success: wait for the end, then read and verify the result",
		},
		"docs": "AGENTS.md, docs/operations.md, docs/health-and-diagnostics.md, docs/attempt-progress.md, docs/job-failure-diagnostics.md, docs/service-upgrade.md, docs/sessions.md, docs/sessions-protocol.md",
	}
}

func helpJSON(stdout io.Writer) int {
	b, _ := json.MarshalIndent(helpCatalogue(), "", "  ")
	fmt.Fprintln(stdout, string(b))
	return 0
}

// unknownAdmission explains a start whose reply never came: the job may have
// been admitted, so it is looked up by its own ID, and a fresh ID is never
// started in its place.
func unknownAdmission(err error, id string, stderr io.Writer) bool {
	if !errors.Is(err, manager.ErrNoReply) {
		return false
	}
	fmt.Fprintf(stderr, "pomar run: %v\n%s may have been admitted. Look it up with `pomar attempt get -id %s` (or `pomar wait -id %s`); don't start it again under a new ID.\n", err, id, id, id)
	return true
}

// lastSeen names the state a wait last saw, or says it saw none.
func lastSeen(e manager.Entry) string {
	if e.State == "" {
		return "unanswered (the manager gave no state in time)"
	}
	return string(e.State)
}
