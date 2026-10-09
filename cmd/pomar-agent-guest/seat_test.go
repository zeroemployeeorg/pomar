package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
	"github.com/zeroemployeeorg/pomar/internal/claudeactor"
	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

// The tmux session's program is the supervisor, never Claude Code itself:
// nothing starts the actor but the operator (SOW 16 §3.1).
func TestTheSeatsSessionHoldsOnlyTheSupervisor(t *testing.T) {
	argv := tmuxEnsure(seatConfig{Seat: "zeocreator", Claude: "/opt/pomar-claude/bin/claude", Self: "/opt/pomar/pomar-agent-guest"})
	i := slices.Index(argv, "--")
	if i < 0 || argv[i+1] != "/opt/pomar/pomar-agent-guest" || argv[i+2] != "seat-supervisor" {
		t.Fatalf("the session's program is %q", argv[i+1:])
	}
	if slices.Contains(argv[:i], "/opt/pomar-claude/bin/claude") {
		t.Fatal("tmux was asked to run Claude Code directly")
	}
	if a := tmuxAttach(); a.UID != seatUID || a.GID != seatGID || !slices.Contains(a.Args, "attach-session") {
		t.Fatalf("attach %+v", a)
	}
}

// Claude Code in a seat keeps Pomar's restricted tools and Bash rules, and
// the seat's environment turns off auto-update and nonessential traffic.
func TestTheSeatsAgentIsRestricted(t *testing.T) {
	args := agentArgs()
	if !slices.Contains(args, "--restricted") || !slices.Contains(args, claudeactor.BashRules) || !slices.Contains(args, claudeactor.GuestTools) {
		t.Fatalf("args %q", args)
	}
	for _, a := range args {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") {
			t.Fatalf("an unsafe flag %q", a)
		}
	}
	env := strings.Join(seatEnv(), "\n")
	for _, want := range []string{"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CLAUDE_CONFIG_DIR=" + seatConfigDir} {
		if !strings.Contains(env, want) {
			t.Fatalf("the seat's environment lacks %s", want)
		}
	}
	for _, banned := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR", "SSH_AUTH_SOCK"} {
		if strings.Contains(env, banned) {
			t.Fatalf("the seat's environment carries %s", banned)
		}
	}
}

func seatStore(t *testing.T) *agentenv.Store {
	t.Helper()
	store, err := agentenv.Open(filepath.Join(t.TempDir(), "state"), "env-1", "session-1", "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestTheSeatBindsItsOwnSessionAndIncarnation(t *testing.T) {
	bind := seatBinding(seatStore(t))
	if !bind("session-1", "inc-1") {
		t.Fatal("the current binding was refused")
	}
	for _, c := range [][2]string{{"session-1", "inc-0"}, {"session-2", "inc-1"}, {"", ""}, {"session-1", ""}} {
		if bind(c[0], c[1]) {
			t.Fatalf("%q was accepted", c)
		}
	}
}

// The seat's socket serves its session record and its terminal, end to end.
func TestTheSeatSocketServesItsSessionAndTerminal(t *testing.T) {
	store := seatStore(t)
	route, err := seatterm.NewRoute(seatBinding(store), func() error { return nil },
		seatterm.Command{Path: "/bin/sh", Args: []string{"-c", "stty size"}, UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(seatHandler(store, route, func() bool { return false }))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Mode          string            `json:"mode"`
		Authenticated bool              `json:"authenticated"`
		Session       map[string]string `json:"session"`
	}
	json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if s.Mode != "seat" || s.Session["session_id"] != "session-1" || s.Session["incarnation"] != "inc-1" || s.Authenticated {
		t.Fatalf("session %+v", s)
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := seatterm.Open(conn, "/v1/terminal?session_id=session-1&expected_incarnation=inc-1")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r, w, _ := os.Pipe()
	defer w.Close()
	done := make(chan error, 1)
	go func() { done <- seatterm.Pump(stream, r, &out, 100, 30, nil) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal never closed")
	}
	if !strings.Contains(out.String(), "30 100") {
		t.Fatalf("screen %q", out.String())
	}
	// No headless turn routes are served in a seat.
	resp, _ = http.Post(srv.URL+"/v1/turns", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != 404 {
		t.Fatalf("a turn route answered %d", resp.StatusCode)
	}
}
