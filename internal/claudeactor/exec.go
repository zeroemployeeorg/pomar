package claudeactor

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Exec launches Claude Code as a real process: as the coding user, in its
// own process group, with an explicit environment and no inherited one.
// Its stderr is not attached, as with the Codex launch: raw authentication
// and protocol diagnostics never reach the public API.
type Exec struct {
	Binary string
	Dir    string
	Env    []string
	// UID and GID run the process as the coding user; -1 keeps the caller's
	// (tests only: the guest broker always sets the coding user).
	UID, GID int
}

// GuestEnv is the coding user's environment for Claude Code in a Pomar
// agent environment: egress only through the guest's loopback relay to the
// host's allowlisted CONNECT proxy, the Go module proxy, and Claude Code's
// optional traffic, telemetry, error reporting and auto-update switched off.
func GuestEnv(home, configDir, path string) []string {
	return []string{
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=" + configDir,
		"PATH=" + path,
		"HTTPS_PROXY=http://127.0.0.1:7072", "HTTP_PROXY=http://127.0.0.1:7072", "ALL_PROXY=http://127.0.0.1:7072",
		"GOPROXY=http://127.0.0.1:7070", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "DISABLE_AUTOUPDATER=1",
	}
}

// Start implements Config.Start.
func (e Exec) Start(args []string) (Process, error) {
	cmd := exec.Command(e.Binary, args...)
	cmd.Dir = e.Dir
	cmd.Env = append([]string(nil), e.Env...)
	attr := &syscall.SysProcAttr{Setpgid: true}
	if e.UID >= 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(e.UID), Gid: uint32(e.GID)}
	}
	cmd.SysProcAttr = attr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, in: in, out: out}, nil
}

type execProcess struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.Reader
}

func (p *execProcess) Stdin() io.WriteCloser { return p.in }
func (p *execProcess) Stdout() io.Reader     { return p.out }
func (p *execProcess) Wait() error           { return p.cmd.Wait() }

// Interrupt sends SIGINT to the process group. Whether Claude Code's
// stream-json mode then ends the turn with a result, or ends the process,
// is part of the pinned version's qualification.
func (p *execProcess) Interrupt() error {
	return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGINT)
}

// CredentialPresent reports whether Claude Code's credential file exists in
// configDir as a regular, non-empty file. It never opens or reads the file:
// placing the credential is the owner's act (POMAR-CC SOW 01 §5 P4).
func CredentialPresent(configDir string) func() bool {
	return func() bool {
		fi, err := os.Lstat(filepath.Join(configDir, ".credentials.json"))
		return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
	}
}
