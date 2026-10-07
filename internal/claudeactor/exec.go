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

// BashRules is Pomar's own permission policy for Claude Code (adviser note
// r42 §6), passed with --settings under --restricted. --restricted ignores
// user, project and local settings files, so the agent cannot change it.
// Pomar, not Claude Code's built-in defaults, names:
//   - the read-only commands that run without asking;
//   - the network tools that are refused outright;
//   - the git operations that reach a remote, which are always asked about.
//
// Anything else follows Claude Code's default, which the live probe records
// per pin (TestLiveProbeBashRules). Prefix rules are a policy layer, not
// containment: a refused tool can be reached another way (sh -c, env), and
// the guest's egress allowlist and fence remain what contains it.
const BashRules = `{"permissions":{` +
	`"allow":["Bash(ls)","Bash(ls *)","Bash(pwd)","Bash(cat *)","Bash(head *)","Bash(tail *)","Bash(wc *)","Bash(grep *)",` +
	`"Bash(git status)","Bash(git status *)","Bash(git log)","Bash(git log *)","Bash(git diff)","Bash(git diff *)","Bash(git show *)"],` +
	`"deny":["Bash(curl *)","Bash(wget *)","Bash(nc *)","Bash(ncat *)","Bash(ssh *)","Bash(scp *)","Bash(sftp *)","Bash(rsync *)","Bash(telnet *)","Bash(ftp *)","WebFetch","WebSearch"],` +
	`"ask":["Bash(git fetch)","Bash(git fetch *)","Bash(git pull)","Bash(git pull *)","Bash(git push)","Bash(git push *)","Bash(git clone *)","Bash(git remote *)","Bash(git submodule *)","Bash(git ls-remote *)"]}}`

// GuestTools are the built-in tools a guest's Claude Code has, named by Pomar
// (--restricted adds no command tool --tools does not name): no subagents,
// scheduling, worktrees or web tools.
const GuestTools = "Bash,Read,Edit,Write,Glob,Grep,NotebookEdit"

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
