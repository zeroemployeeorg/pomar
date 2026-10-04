// Command pomar-agent-guest is the root broker inside a dedicated Linux VM.
// It starts only Codex as the job uid; host controllers use its private socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pomar-agent-guest:", err)
		os.Exit(1)
	}
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
	flag.Parse()
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
	broker := agentenv.NewBroker(store, *workspace)
	cmd := exec.Command(*codex, "app-server", "--listen", "stdio://", "-c", "cli_auth_credentials_store=\"file\"", "-c", "analytics.enabled=false")
	cmd.Dir = *workspace
	cmd.Env = []string{"HOME=/pomar/job", "CODEX_HOME=/pomar/job/.codex", "PATH=/opt/pomar-codex/codex-path:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "HTTPS_PROXY=http://127.0.0.1:7072", "HTTP_PROXY=http://127.0.0.1:7072", "ALL_PROXY=http://127.0.0.1:7072", "GOPROXY=http://127.0.0.1:7070", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly"}
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
	actor := agentenv.NewRPC(out, in, broker.Observe)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = broker.Initialize(ctx, actor); err != nil {
		return err
	}
	if err = broker.Resume(ctx); err != nil {
		return err
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
	go func() { <-actor.Done(); server.Close() }()
	err = server.Serve(ln)
	// The VM owner must confirm stop before replacing this actor. The broker
	// never signals an arbitrary process or resumes a previous uncertain task.
	if err == http.ErrServerClosed {
		return fmt.Errorf("actor exited; replacement and execution fencing required")
	}
	return err
}
