package claudeactor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// The credential check sees only a regular, non-empty file; it never opens it.
func TestCredentialPresent(t *testing.T) {
	dir := t.TempDir()
	present := CredentialPresent(dir)
	if present() {
		t.Fatal("no file counted as a credential")
	}
	path := filepath.Join(dir, ".credentials.json")
	os.WriteFile(path, nil, 0o600)
	if present() {
		t.Fatal("an empty file counted as a credential")
	}
	os.Remove(path)
	other := filepath.Join(dir, "elsewhere")
	os.WriteFile(other, []byte("x"), 0o600)
	os.Symlink(other, path)
	if present() {
		t.Fatal("a symlink counted as a credential")
	}
	os.Remove(path)
	os.WriteFile(path, []byte("synthetic"), 0o000)
	if !present() {
		t.Fatal("an unreadable regular file must still count as present: the check never opens it")
	}
}

// The guest environment points at the relays and switches off optional
// traffic, and inherits nothing from the caller.
func TestGuestEnv(t *testing.T) {
	env := strings.Join(GuestEnv("/pomar/job", "/pomar/job/.claude", "/usr/bin:/bin"), "\n")
	for _, want := range []string{"HTTPS_PROXY=http://127.0.0.1:7072", "CLAUDE_CONFIG_DIR=/pomar/job/.claude", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "DISABLE_AUTOUPDATER=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("guest env lacks %s", want)
		}
	}
	if strings.Contains(env, "ANTHROPIC_API_KEY") || strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("a credential variable is in the guest env")
	}
}

// A real process launched by Exec, here a shell script standing in for
// Claude Code, completes a turn through the shared broker.
func TestExecProcessThroughTheBroker(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	// Reads one user line, then prints init and a success result for the
	// session named after --session-id.
	script := `#!/bin/sh
s=""; while [ $# -gt 0 ]; do [ "$1" = --session-id ] && s=$2; shift; done
read line
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$s"
printf '{"type":"result","subtype":"success","session_id":"%s","is_error":false}\n' "$s"
read rest
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := agentenv.Open(filepath.Join(dir, "state"), "env-1", "session-1", "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	broker := agentenv.NewBroker(store, dir)
	actor := New(Config{
		Version:       "2.1.280",
		Start:         Exec{Binary: fake, Dir: dir, Env: []string{"PATH=/usr/bin:/bin"}, UID: -1, GID: -1}.Start,
		Authenticated: func() bool { return true },
	}, broker.Observe)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := broker.Initialize(ctx, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op := store.Snapshot().Operations["op-1"]; op.Completion != "" {
			if op.Completion != "completed" {
				t.Fatalf("completion %q", op.Completion)
			}
			raw, _ := json.Marshal(store.Snapshot().Events)
			if !strings.Contains(string(raw), "turn/completed") {
				t.Fatal("no turn/completed event")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the turn did not complete through a real process")
}
