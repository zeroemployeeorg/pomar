package seatterm

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAgent records each start's arguments; FAKE_MODE "linger" leaves a
// background process holding the inherited lock after the agent exits.
const fakeAgent = `#!/bin/sh
echo "$@" >> "$CALLS"
if [ "$FAKE_MODE" = linger ]; then (sleep 3) & fi
exit 0
`

func supervisor(t *testing.T, input, mode string) (Supervisor, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	agent := filepath.Join(dir, "claude")
	os.WriteFile(agent, []byte(fakeAgent), 0o755)
	calls := filepath.Join(dir, "calls")
	out := &bytes.Buffer{}
	return Supervisor{Seat: "zeocreator", StateDir: filepath.Join(dir, "state"), Agent: agent, Args: []string{"--settings", "/etc/pomar/seat-settings.json"},
		Env: []string{"PATH=/usr/bin:/bin", "CALLS=" + calls, "FAKE_MODE=" + mode}, In: strings.NewReader(input), Out: out}, calls, out
}

func starts(t *testing.T, calls string) []string {
	t.Helper()
	b, _ := os.ReadFile(calls)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestTheFirstStartRecordsASessionAndLaterStartsResumeIt(t *testing.T) {
	s, calls, _ := supervisor(t, "r\nr\n", "")
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	got := starts(t, calls)
	if len(got) != 2 {
		t.Fatalf("starts %q", got)
	}
	first, second := strings.Fields(got[0]), strings.Fields(got[1])
	if first[2] != "--session-id" || !uuidV4.MatchString(first[3]) {
		t.Fatalf("first start %q", got[0])
	}
	if second[2] != "--resume" || second[3] != first[3] {
		t.Fatalf("second start %q didn't resume %s", got[1], first[3])
	}
	if first[0] != "--settings" {
		t.Fatal("the fixed arguments were dropped")
	}
}

func TestNothingStartsUnlessAsked(t *testing.T) {
	s, calls, out := supervisor(t, "\nq\nstart\n", "")
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	if n := len(starts(t, calls)); n != 0 {
		t.Fatalf("%d starts without an r", n)
	}
	if !strings.Contains(out.String(), "nothing starts on its own") {
		t.Fatalf("prompt %q", out.String())
	}
}

// The actor's lock lives as long as anything holding its descriptor does,
// even after the agent and the supervisor's own descriptor are gone.
func TestASecondActorIsRefusedWhileTheFirstLives(t *testing.T) {
	s, calls, _ := supervisor(t, "", "linger")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); !errors.Is(err, ErrActorRunning) {
		t.Fatalf("a second start while the first lingers: %v", err)
	}
	if n := len(starts(t, calls)); n != 1 {
		t.Fatalf("%d starts", n)
	}
	time.Sleep(3500 * time.Millisecond)
	s.Env[2] = "FAKE_MODE="
	if err := s.Start(); err != nil {
		t.Fatalf("a start once the first has ended: %v", err)
	}
}

func TestAnUnreadableSessionIsNeverReplaced(t *testing.T) {
	s, calls, _ := supervisor(t, "", "")
	os.MkdirAll(s.StateDir, 0o700)
	os.WriteFile(filepath.Join(s.StateDir, "session.json"), []byte(`{"session_id":"not-a-uuid"}`), 0o600)
	if err := s.Start(); err == nil {
		t.Fatal("started over an unreadable session")
	}
	b, _ := os.ReadFile(filepath.Join(s.StateDir, "session.json"))
	if !strings.Contains(string(b), "not-a-uuid") || len(starts(t, calls)) != 0 {
		t.Fatal("the session record was replaced, or an actor started")
	}
}
