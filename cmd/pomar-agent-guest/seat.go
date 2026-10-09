package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
	"github.com/zeroemployeeorg/pomar/internal/claudeactor"
	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

// An interactive seat (POMAR-CC SOW 15 §5, SOW 16 §3): -actor seat runs no
// headless actor. The broker socket serves the seat's terminal, bound to
// this environment's session and incarnation, and its session record; the
// seat's tmux session, run as the coding user, holds only the supervisor,
// which starts the pinned Claude Code when the operator asks.
const (
	seatUID, seatGID = 1000, 1000
	seatHome         = "/pomar/job"
	seatConfigDir    = "/pomar/job/.claude"
	seatStateDir     = "/pomar/job/.pomar-seat"
	seatTmuxDir      = "/run/pomar-seat"
	seatTmuxSocket   = "/run/pomar-seat/tmux.sock"
	seatTmux         = "/usr/bin/tmux"
	seatSession      = "seat"
	seatPath         = "/opt/pomar-claude/bin:/pomar/job/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"
)

var seatName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type seatConfig struct {
	Seat, Claude, ClaudeVersion, Self, Workspace string
}

// seatEnv is the environment of everything the seat runs: the coding
// user's home, its Claude configuration on the workspace disk, the relayed
// egress, and no auto-update or nonessential traffic (claudeactor.GuestEnv).
func seatEnv() []string {
	return append(claudeactor.GuestEnv(seatHome, seatConfigDir, seatPath), "TERM=xterm-256color", "LANG=C.UTF-8")
}

// agentArgs are Claude Code's fixed arguments in a seat: Pomar's own tools
// and Bash rules under --restricted, as for the headless actor, so neither
// a repository nor the agent can loosen them. Permission prompts go to the
// operator at the terminal, not to a bridge (SOW 16 §3.5).
func agentArgs() []string {
	return []string{"--restricted", "--tools", claudeactor.GuestTools, "--settings", claudeactor.BashRules}
}

// tmuxEnsure is the command that makes the seat's tmux session exist,
// holding only the idle supervisor; tmuxHas checks for it first.
func tmuxHas() []string {
	return []string{seatTmux, "-S", seatTmuxSocket, "has-session", "-t", seatSession}
}

func tmuxEnsure(c seatConfig) []string {
	return []string{seatTmux, "-S", seatTmuxSocket, "new-session", "-d", "-s", seatSession, "-x", "200", "-y", "50", "--",
		c.Self, "seat-supervisor", "-seat", c.Seat, "-claude", c.Claude}
}

func tmuxAttach() seatterm.Command {
	return seatterm.Command{Path: seatTmux, Args: []string{"-S", seatTmuxSocket, "attach-session", "-t", seatSession}, Env: seatEnv(), Dir: seatHome, UID: seatUID, GID: seatGID}
}

func asSeatUser(argv []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = seatEnv(), seatHome
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: seatUID, Gid: seatGID}}
	return cmd
}

// seatBinding admits only this environment's current session and
// incarnation, as recorded by the guest's own store.
func seatBinding(store *agentenv.Store) func(session, incarnation string) bool {
	return func(session, incarnation string) bool {
		s := store.Snapshot()
		return session != "" && incarnation != "" && session == s.SessionID && incarnation == s.Incarnation
	}
}

// seatHandler serves the seat's terminal and its session record.
func seatHandler(store *agentenv.Store, route http.Handler, authenticated func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/terminal", route)
	mux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, r *http.Request) {
		s := store.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"mode":          "seat",
			"authenticated": authenticated(),
			"session":       map[string]string{"environment_id": s.EnvironmentID, "session_id": s.SessionID, "incarnation": s.Incarnation},
		})
	})
	return mux
}

// runSeat serves an interactive seat on the broker's private socket.
func runSeat(store *agentenv.Store, c seatConfig, socket string) error {
	if !seatName.MatchString(c.Seat) || c.ClaudeVersion == "" {
		return errors.New("-actor seat needs -seat and -claude-version")
	}
	if err := os.MkdirAll(seatTmuxDir, 0o700); err != nil {
		return err
	}
	if err := os.Chown(seatTmuxDir, seatUID, seatGID); err != nil {
		return err
	}
	ensure := func() error {
		if asSeatUser(tmuxHas()).Run() == nil {
			return nil
		}
		if out, err := asSeatUser(tmuxEnsure(c)).CombinedOutput(); err != nil {
			return fmt.Errorf("the seat's session didn't start: %v (%d bytes of output withheld)", err, len(out))
		}
		return nil
	}
	route, err := seatterm.NewRoute(seatBinding(store), ensure, tmuxAttach())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(socket); err == nil {
		return fmt.Errorf("broker socket already exists; refusing to replace it")
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	server := &http.Server{Handler: seatHandler(store, route, claudeactor.CredentialPresent(seatConfigDir)), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	return server.Serve(ln)
}

// runSeatSupervisor is `pomar-agent-guest seat-supervisor`, the program in
// the seat's tmux session, run as the coding user.
func runSeatSupervisor() error {
	fs := flag.NewFlagSet("seat-supervisor", flag.ContinueOnError)
	seat := fs.String("seat", "", "the seat's name")
	claude := fs.String("claude", "/opt/pomar-claude/bin/claude", "the pinned Claude Code binary")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if !seatName.MatchString(*seat) {
		return errors.New("seat-supervisor needs -seat")
	}
	return seatterm.Supervisor{Seat: *seat, StateDir: seatStateDir, Agent: *claude, Args: agentArgs(), Env: seatEnv(), Dir: "/work", In: os.Stdin, Out: os.Stdout}.Run()
}
