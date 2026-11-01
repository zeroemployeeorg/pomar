package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
	"github.com/zeroemployeeorg/pomar/internal/codexseat"
	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

const codexStateDir = "/var/lib/pomar-agent/codex-seat"
const codexTmuxDir = "/run/pomar-codex-terminal"
const codexOwnerDir = "/run/pomar-codex-owner"
const codexConsoleSocket = codexOwnerDir + "/console.sock"
const codexTmuxSocket = codexTmuxDir + "/tmux.sock"
const codexTmuxConfig = codexOwnerDir + "/tmux.conf"

// Custodian terminal has no command bindings, user config, environment update,
// or usable shell. Only the fixed supervisor command can run in its one pane.
const codexCustodianTmuxConfig = `set-option -g prefix None
set-option -g prefix2 None
set-option -g default-shell /bin/false
set-option -g update-environment ""
set-option -g mouse off
set-option -g status off
set-option -g allow-rename off
set-option -g set-titles off
set-option -g set-clipboard off
set-option -g allow-passthrough off
unbind-key -a
unbind-key -a -T root
unbind-key -a -T copy-mode
unbind-key -a -T copy-mode-vi
`

func codexCustodianEnv() []string {
	return []string{"HOME=" + codexTmuxDir, "PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8"}
}
func codexTmuxHas() []string {
	return []string{seatTmux, "-f", codexTmuxConfig, "-S", codexTmuxSocket, "has-session", "-t", seatSession}
}
func codexSeatEnv(seat string) []string {
	return []string{"HOME=" + seatHome, "CODEX_HOME=" + seatHome + "/.codex", "PATH=/opt/pomar-codex/codex-path:/pomar/job/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8", "HTTPS_PROXY=http://127.0.0.1:7072", "HTTP_PROXY=http://127.0.0.1:7072", "ALL_PROXY=http://127.0.0.1:7072", "GOPROXY=http://127.0.0.1:7070", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly", "ZMAIL_URL=http://127.0.0.1:7074", "ZMAIL_INSTANCE=" + seat, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"}
}

func codexTmuxEnsure(seat, self string) []string {
	return []string{seatTmux, "-f", codexTmuxConfig, "-S", codexTmuxSocket, "new-session", "-d", "-s", seatSession, "-x", "200", "-y", "50", "--", self, "codex-seat-console", "-seat", seat, "-socket", codexConsoleSocket}
}

func runCodexSeat(store *agentenv.Store, seat, binary, version, workspace, socket string, launch *agentenv.SeatLaunch) error {
	if err := launch.Validate(); err != nil {
		return err
	}
	if launch.CWD != workspace {
		return errors.New("declared cwd differs from guest source")
	}
	if !seatName.MatchString(seat) || version != codexseat.Version || !filepath.IsAbs(binary) || !filepath.IsAbs(workspace) {
		return errors.New("Codex seat requires a valid seat, pinned version and absolute guest paths")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("Codex seat broker requires the VM custodian")
	}
	if err = prepareCodexCustody(); err != nil {
		return err
	}
	if err = codexseat.CheckCustodianExecutable(self); err != nil {
		return err
	}
	if err = codexseat.CheckTmux(seatTmux); err != nil {
		return err
	}
	command := func(argv []string) *exec.Cmd {
		args := append([]string{"codex-custodian-exec", "--"}, argv...)
		c := exec.Command(self, args...)
		c.Env = codexCustodianEnv()
		c.Dir = "/"
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: codexseat.ConsoleUID, Gid: codexseat.ConsoleUID, Groups: []uint32{codexseat.ConsoleUID}}}
		return c
	}
	versionCmd := command([]string{seatTmux, "-V"})
	b, e := versionCmd.Output()
	if e != nil || strings.TrimSpace(string(b)) != codexseat.TmuxVersion {
		return errors.New("pinned tmux version readback refused")
	}
	console, err := net.ListenUnix("unix", &net.UnixAddr{Name: codexConsoleSocket, Net: "unix"})
	if err != nil {
		return err
	}
	defer console.Close()
	if err = os.Chown(codexConsoleSocket, 0, codexseat.ConsoleUID); err != nil {
		return err
	}
	if err = os.Chmod(codexConsoleSocket, 0660); err != nil {
		return err
	}
	supervisor := codexseat.Supervisor{Seat: seat, Binary: binary, Home: seatHome + "/.codex", StateDir: codexStateDir, Workspace: workspace, Launch: launch, Env: codexSeatEnv(seat)}
	consoleErr := make(chan error, 1)
	go func() { consoleErr <- supervisor.ServeConsole(console) }()
	ensure := func() error {
		if command(codexTmuxHas()).Run() == nil {
			return nil
		}
		if err := command(codexTmuxEnsure(seat, self)).Run(); err != nil {
			return errors.New("Codex custodian console did not start; inspect guest custody")
		}
		return nil
	}
	attach := seatterm.Command{Path: self, Args: []string{"codex-custodian-exec", "--", seatTmux, "-f", codexTmuxConfig, "-S", codexTmuxSocket, "attach-session", "-t", seatSession}, Env: codexCustodianEnv(), Dir: "/", UID: codexseat.ConsoleUID, GID: codexseat.ConsoleUID}
	route, err := seatterm.NewRoute(seatBinding(store), ensure, attach)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	if _, err = os.Lstat(socket); err == nil {
		return errors.New("guest broker socket exists; replacement refused")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	// Authentication is not inferred from the presence of a credentials file.
	// The TUI performs its own guest login; protocol login remains app-server's.
	handler := http.NewServeMux()
	handler.Handle("/", seatHandler(store, route, func() bool { return false }))
	handler.HandleFunc("GET /v1/codex/startup", func(w http.ResponseWriter, r *http.Request) {
		s := codexseat.Supervisor{Seat: seat, Binary: binary, Home: seatHome + "/.codex", StateDir: codexStateDir, Workspace: workspace, Launch: launch, Env: codexSeatEnv(seat)}
		info, e := s.InspectStartup()
		if e != nil {
			http.Error(w, "startup inspection held", 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})
	handler.HandleFunc("GET /v1/codex/fence", func(w http.ResponseWriter, r *http.Request) {
		s := codexseat.Supervisor{StateDir: codexStateDir}
		info, e := s.InspectFence()
		if e != nil {
			http.Error(w, "fence observation unavailable", 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})
	handler.HandleFunc("POST /v1/codex/startup/abandon", func(w http.ResponseWriter, r *http.Request) {
		var req codexseat.ResetRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid reset request", 400)
			return
		}
		launchRecord := codexseat.Supervisor{Seat: seat, Binary: binary, Home: seatHome + "/.codex", StateDir: codexStateDir, Workspace: workspace, Launch: launch, Env: codexSeatEnv(seat)}
		receipt, e := launchRecord.AbandonStart(req, "/var/lib/pomar-agent/codex-start-resets")
		if e != nil {
			http.Error(w, "original unbound startup remains held", 409)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(receipt)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	ownerErr := make(chan error, 1)
	go func() { ownerErr <- server.Serve(ln) }()
	select {
	case e := <-ownerErr:
		return e
	case e := <-consoleErr:
		server.Close()
		return e
	}
}

func runCodexSeatConsole() error {
	fs := flag.NewFlagSet("codex-seat-console", flag.ContinueOnError)
	seat := fs.String("seat", "", "seat name")
	socket := fs.String("socket", codexConsoleSocket, "root broker socket")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if !seatName.MatchString(*seat) || *socket != codexConsoleSocket || fs.NArg() != 0 {
		return errors.New("console binding refused")
	}
	return codexseat.RunConsole(*socket, *seat, os.Stdin, os.Stdout)
}

func prepareCodexCustody() error {
	if os.Geteuid() != 0 {
		return errors.New("root broker custody required")
	}
	if err := os.MkdirAll(codexStateDir, 0700); err != nil {
		return err
	}
	if err := codexseat.CheckCustodianDirectory(codexStateDir); err != nil {
		return err
	}
	for _, item := range []struct {
		path     string
		uid, gid int
		mode     os.FileMode
	}{{codexTmuxDir, codexseat.ConsoleUID, codexseat.ConsoleUID, 0700}, {codexOwnerDir, 0, codexseat.ConsoleUID, 0750}} {
		err := os.Mkdir(item.path, item.mode)
		if err == nil {
			// The parent is root-owned, so the newly created child cannot be
			// replaced before this descriptor is opened. Apply the exact mode
			// despite a restrictive inherited umask, before dropping ownership.
			fd, e := os.OpenFile(item.path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
			if e != nil {
				return e
			}
			e = fd.Chmod(item.mode)
			if e == nil {
				e = fd.Chown(item.uid, item.gid)
			}
			fd.Close()
			if e != nil {
				return e
			}
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
		fi, err := os.Lstat(item.path)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != item.mode {
			return errors.New("custodian directory mode refused")
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(item.uid) || st.Gid != uint32(item.gid) {
			return errors.New("custodian directory ownership refused")
		}
	}
	f, err := os.OpenFile(codexTmuxConfig, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0440)
	if errors.Is(err, os.ErrExist) {
		fi, e := os.Lstat(codexTmuxConfig)
		if e != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0440 {
			return errors.New("tmux configuration custody refused")
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || st.Gid != codexseat.ConsoleUID {
			return errors.New("tmux configuration owner refused")
		}
		b, e := os.ReadFile(codexTmuxConfig)
		if e != nil || string(b) != codexCustodianTmuxConfig {
			return errors.New("tmux configuration differs")
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Chmod(0440); err != nil {
		return err
	}
	if err = f.Chown(0, codexseat.ConsoleUID); err != nil {
		return err
	}
	if _, err = fmt.Fprint(f, codexCustodianTmuxConfig); err != nil {
		return err
	}
	return f.Sync()
}

func parseCodexLaunch(raw string) (*agentenv.SeatLaunch, error) {
	if len(raw) > 4096 {
		return nil, errors.New("launch declaration too large")
	}
	var l agentenv.SeatLaunch
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&l); err != nil {
		return nil, errors.New("invalid launch declaration")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing launch declaration")
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return &l, nil
}
