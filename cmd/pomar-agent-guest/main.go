// Command pomar-agent-guest is the root broker inside a dedicated Linux VM.
// It starts the coding agent (Codex by default, or Claude Code with -actor
// claude) as the job uid; host controllers use its private socket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
	"github.com/zeroemployeeorg/pomar/internal/claudeactor"
)

func main() {
	run := run
	if len(os.Args) > 1 && os.Args[1] == "seat-supervisor" {
		run = runSeatSupervisor
	}
	if len(os.Args) > 1 && os.Args[1] == "claude-mcp" {
		run = runClaudeMCP
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pomar-agent-guest:", err)
		os.Exit(1)
	}
}

// The bridge between Claude Code's permission tool and the root broker: the
// broker listens; the MCP server, started by Claude Code as the coding user,
// connects. Only the coding user can reach it, in its own directory: the
// broker's root-only /run/pomar is not opened up for it.
const claudeBridgeSocket = "/run/pomar-claude/bridge.sock"

// runClaudeMCP is `pomar-agent-guest claude-mcp -socket S`: the MCP server on
// stdio that forwards Claude Code's permission calls to the bridge socket.
func runClaudeMCP() error {
	fs := flag.NewFlagSet("claude-mcp", flag.ContinueOnError)
	socket := fs.String("socket", claudeBridgeSocket, "the root broker's bridge socket")
	capabilities := fs.String("controller-capabilities", "", "the environment's controller capabilities, comma-separated, for the controller request tool")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	var names []string
	if *capabilities != "" {
		names = strings.Split(*capabilities, ",")
	}
	return claudeactor.ServeMCPController(os.Stdin, os.Stdout, names, func(tool string, args json.RawMessage) (claudeactor.BridgeReply, error) {
		return claudeactor.Forward(*socket, tool, args)
	})
}

// listenClaudeBridge makes the bridge socket for the coding user (uid 1000)
// only, refusing to replace an existing path.
func listenClaudeBridge() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(claudeBridgeSocket), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(claudeBridgeSocket); err == nil {
		return nil, fmt.Errorf("claude bridge socket already exists; refusing to replace it")
	}
	ln, err := net.Listen("unix", claudeBridgeSocket)
	if err != nil {
		return nil, err
	}
	if err = os.Chown(claudeBridgeSocket, 1000, 1000); err == nil {
		err = os.Chmod(claudeBridgeSocket, 0o600)
	}
	if err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func run() error {
	environment := flag.String("environment", "", "environment id")
	session := flag.String("session", "", "stable session id")
	incarnation := flag.String("incarnation", "", "fenced actor incarnation")
	state := flag.String("state", "/var/lib/pomar-agent", "root-only journal")
	socket := flag.String("socket", "/run/pomar/agent.sock", "root-only control socket")
	codex := flag.String("codex", "/opt/pomar-codex/bin/codex", "pinned codex binary")
	workspace := flag.String("workspace", "/work", "isolated workspace")
	sourceSHA := flag.String("source-sha", "", "initial source commit")
	capabilities := flag.String("controller-capabilities", "", "owner-configured comma-separated controller capability names")
	actorKind := flag.String("actor", "codex", "the coding agent: codex (app-server), claude (Claude Code, stream-json) or seat (an interactive Claude Code seat)")
	seat := flag.String("seat", "", "with -actor seat: the seat's name")
	claude := flag.String("claude", "/opt/pomar-claude/bin/claude", "pinned Claude Code binary, with -actor claude")
	claudeVersion := flag.String("claude-version", "", "the pinned Claude Code version, reported in the userAgent, with -actor claude")
	flag.Parse()
	if *actorKind != "codex" && *actorKind != "claude" && *actorKind != "seat" {
		return fmt.Errorf("unknown -actor %q", *actorKind)
	}
	if os.Getuid() != 0 {
		return fmt.Errorf("guest broker requires root inside its VM")
	}
	store, err := agentenv.Open(*state, *environment, *session, *incarnation)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.BindSource(*sourceSHA); err != nil {
		return err
	}
	if *actorKind == "seat" {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		return runSeat(store, seatConfig{Seat: *seat, Claude: *claude, ClaudeVersion: *claudeVersion, Self: self, Workspace: *workspace}, *socket)
	}
	broker := agentenv.NewBroker(store, *workspace)
	var names []string
	if *capabilities != "" {
		names = strings.Split(*capabilities, ",")
	}
	if err = broker.ConfigureController(names); err != nil {
		return err
	}
	var actor agentenv.Actor
	if *actorKind == "claude" {
		// Controller capabilities are refused at Initialize unless the
		// controller bridge is qualified for this Claude Code version.
		if *claudeVersion == "" {
			return fmt.Errorf("-actor claude needs -claude-version")
		}
		configDir := "/pomar/job/.claude"
		self, err := os.Executable()
		if err != nil {
			return err
		}
		mcpConfig, err := claudeactor.MCPConfig(self, claudeBridgeSocket, names...)
		if err != nil {
			return err
		}
		// Not --bare: it never reads OAuth credentials (claude --help, 2.1.280).
		// --restricted ignores user, project and local settings files, so
		// neither a task repository nor the agent itself (its own user settings
		// are writable by it) can add hooks or permission rules. Its tools are
		// the ones Pomar names, and its permission policy is Pomar's own,
		// through --settings (adviser note r42 §6; TestLiveBashRules).
		// The only MCP server is Pomar's bridge. The controller request tool,
		// when configured, is the controlled exchange itself, so it is allowed
		// rather than put to the permission tool. The owner's adapter selection
		// binds these arguments, so it covers the tools and rules too.
		args := []string{"--restricted", "--tools", claudeactor.GuestTools, "--settings", claudeactor.BashRules, "--strict-mcp-config", "--mcp-config", mcpConfig, "--permission-prompt-tool", claudeactor.PermissionToolName}
		if len(names) > 0 {
			args = append(args, "--allowedTools", claudeactor.ControllerToolName)
		}
		env := claudeactor.GuestEnv("/pomar/job", configDir, "/opt/pomar-claude/bin:/pomar/job/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin")
		selection, err := agentenv.SelectAdapterBinary("anthropic", "claude-code", *claudeVersion, *claude, args, env)
		if err != nil {
			return err
		}
		if err = broker.ConfigureAdapterSelection(selection); err != nil {
			return err
		}
		ca := claudeactor.New(claudeactor.Config{
			Version:                *claudeVersion,
			ControllerCapabilities: names,
			Args:                   args,
			Start:                  claudeactor.Exec{Binary: *claude, Dir: *workspace, Env: env, UID: 1000, GID: 1000}.Start,
			Authenticated:          claudeactor.CredentialPresent(configDir),
			// Sign-in for the coding user, through the same relayed egress: the
			// profile's allowedHosts must include claude.com and
			// platform.claude.com for it, and api.anthropic.com for inference.
			Login: func() (claudeactor.Process, error) {
				return claudeactor.Exec{Binary: *claude, Dir: "/pomar/job", Env: claudeactor.GuestEnv("/pomar/job", configDir, "/opt/pomar-claude/bin:/usr/local/bin:/usr/bin:/bin"), UID: 1000, GID: 1000}.Start([]string{"auth", "login", "--claudeai"})
			},
		}, broker.Observe)
		ln, err := listenClaudeBridge()
		if err != nil {
			return err
		}
		defer ln.Close()
		go ca.ServeBridge(ln)
		actor = ca
		return serve(broker, actor, socket)
	}
	cmd := exec.Command(*codex, "app-server", "--listen", "stdio://", "-c", "cli_auth_credentials_store=\"file\"", "-c", "analytics.enabled=false")
	cmd.Dir = *workspace
	cmd.Env = []string{"HOME=/pomar/job", "CODEX_HOME=/pomar/job/.codex", "PATH=/opt/pomar-codex/codex-path:/pomar/job/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "HTTPS_PROXY=http://127.0.0.1:7072", "HTTP_PROXY=http://127.0.0.1:7072", "ALL_PROXY=http://127.0.0.1:7072", "GOPROXY=http://127.0.0.1:7070", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly"}
	selection, err := agentenv.SelectAdapterBinary("openai", "codex-app-server", "0.160.0", cmd.Path, cmd.Args[1:], cmd.Env)
	if err != nil {
		return err
	}
	if err = broker.ConfigureAdapterSelection(selection); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}, Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Raw authentication/protocol stderr is never exported through the public API.
	if err = cmd.Start(); err != nil {
		return err
	}
	defer in.Close()
	actor = agentenv.NewRPC(out, in, broker.Observe)
	return serve(broker, actor, socket)
}

// serve initializes the actor, resumes any retained thread, and serves the
// broker's private socket until the actor or the server ends.
func serve(broker *agentenv.Broker, actor agentenv.Actor, socket *string) error {
	var err error
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = broker.Initialize(ctx, actor); err != nil {
		return err
	}
	if err = broker.Resume(ctx); err != nil {
		// Keep durable inspection available while all execution routes are held.
		// Never hide the original acceptance evidence behind a failed resume.
		fmt.Fprintln(os.Stderr, "pomar-agent-guest: retained thread resume held; inspect original operation")
	}
	if err = os.MkdirAll(filepath.Dir(*socket), 0o700); err != nil {
		return err
	}
	if _, err = os.Lstat(*socket); err == nil {
		return fmt.Errorf("broker socket already exists; refusing to replace it")
	}
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err = os.Chmod(*socket, 0o600); err != nil {
		return err
	}
	server := &http.Server{Handler: broker.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	// Actor loss must not remove the retained-operation inspection endpoint.
	err = server.Serve(ln)
	// The VM owner must confirm stop before replacing this actor. The broker
	// never signals an arbitrary process or resumes a previous uncertain task.
	if err == http.ErrServerClosed {
		return fmt.Errorf("actor exited; replacement and execution fencing required")
	}
	return err
}
