package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/distribution"
)

const quickHelp = `Pomar — isolated Linux jobs and development seats on Apple silicon

Usage: pomar COMMAND [OPTIONS]

Everyday commands:
  run       Start a job: pomar run -id build-1 -class CLASS -- COMMAND
  status    Show manager capacity and admission limits
  jobs      List attempts
  logs      Read a completed job's log: pomar logs -id build-1
  seats     List declared development seats
  seat      Inspect, start, stop or attach to a declared seat
  doctor    Check this client, bundled tools and an explicit manager socket
  version   Print client version and source commit

Installation and development owners:
  install   Verify and install an extracted release in a user-owned prefix
  upgrade   Check, upgrade or roll back an already provisioned service
  server    Prepare a base/kernel or start a foreground development manager

Set POMAR_SOCKET to the control socket supplied by your host owner.
No socket or data root is guessed. Options go before positional arguments.
Job admission is not completion: retrieve the result and verify its signature.

More: pomar help all | pomar help server | pomar help install | pomar help upgrade
Docs: https://github.com/zeroemployeeorg/pomar/tree/main/docs
`

func helpCmd(args []string, stdout, stderr io.Writer) int {
	topic := ""
	if len(args) > 1 {
		topic = args[1]
	}
	if len(args) > 2 {
		fmt.Fprintln(stderr, "help: use pomar help [all|server|install|upgrade]")
		return 2
	}
	switch topic {
	case "":
		fmt.Fprint(stdout, quickHelp)
	case "all":
		fmt.Fprint(stdout, quickHelp, "\n", usage)
	case "server":
		fmt.Fprint(stdout, serverHelp)
	case "install":
		fmt.Fprint(stdout, installHelp)
	case "upgrade":
		fmt.Fprint(stdout, upgradeHelp)
	default:
		fmt.Fprintf(stderr, "unknown help topic %q; use pomar help all\n", topic)
		return 2
	}
	return 0
}

// Stop at the command separator: guest arguments never select the client's root.
func explicitFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "-"+name || a == "--"+name || strings.HasPrefix(a, "-"+name+"=") || strings.HasPrefix(a, "--"+name+"=") {
			return true
		}
	}
	return false
}

func siblingTool(name string) (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	p = filepath.Join(filepath.Dir(p), name)
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("bundled %s missing: install a complete Pomar release", name)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("bundled %s is not executable", name)
	}
	return p, nil
}

const serverHelp = `Usage:
  pomar server prepare -root ABSOLUTE_PRIVATE_DIR [-image CATALOGUE_NAME]
  pomar server kernel  -root ABSOLUTE_PRIVATE_DIR
  pomar server start  -root ABSOLUTE_PRIVATE_DIR -kernel-sha256 HEX [MANAGER_OPTIONS]

Use a new, dedicated development root. prepare builds the pinned catalogue base;
kernel fetches the catalogue-pinned kernel and prints its digest. start runs the
manager in the foreground using this release's signed host and Linux shim.
Stop the foreground manager normally with Ctrl-C after jobs finish.
Use pomar help all for class, mirror, capacity and control-socket options.
This command does not install a launch daemon or provision accounts.
`

func serverCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(stdout, serverHelp)
		return 0
	}
	step := args[0]
	rest := args[1:]
	if step != "prepare" && step != "kernel" && step != "start" {
		fmt.Fprint(stderr, serverHelp)
		return 2
	}
	// Require the root in argv; production/private roots cannot be inherited.
	root := ""
	for i, a := range rest {
		if a == "--" {
			break
		}
		if a == "-root" || a == "--root" {
			if i+1 < len(rest) {
				root = rest[i+1]
			}
		}
		if strings.HasPrefix(a, "-root=") {
			root = strings.TrimPrefix(a, "-root=")
		}
		if strings.HasPrefix(a, "--root=") {
			root = strings.TrimPrefix(a, "--root=")
		}
	}
	if !filepath.IsAbs(root) || os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "server: run as the development owner with an explicit absolute -root; service installation is a separate owner operation")
		return 2
	}
	if explicitFlag(rest, "host-bin") || explicitFlag(rest, "shim-bin") {
		fmt.Fprintln(stderr, "server: host and shim are bound to this release; use the low-level manager command for an explicit source build")
		return 2
	}
	host, err := siblingTool("pomar-host")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	rest = append([]string{"-host-bin", host}, rest...)
	switch step {
	case "prepare":
		return baseBuild(rest, stdout, stderr)
	case "kernel":
		return smokeCmd("fetch-kernel", rest, stdout, stderr)
	default:
		shim, err := siblingTool("pomar-shim-linux-arm64")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return managerCmd(append([]string{"-shim-bin", shim}, rest...), stdout, stderr)
	}
}

const installHelp = `Usage: pomar install -bundle EXTRACTED_DIR [-prefix ABSOLUTE_DIR]

Verify every release file and install an immutable version under PREFIX/releases.
PREFIX defaults to $HOME/.local/share/pomar. PREFIX/current selects this version;
previous versions remain available. Add PREFIX/current/bin to PATH.
No sudo, account, socket, service, provider or data-root changes are made.
`

func installCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bundle := fs.String("bundle", "", "extracted release directory")
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	prefix := fs.String("prefix", filepath.Join(home, ".local", "share", "pomar"), "user-owned installation prefix")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *bundle == "" || fs.NArg() != 0 {
		fmt.Fprint(stderr, installHelp)
		return 2
	}
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "install: use your own account; permanent-service provisioning is separate")
		return 2
	}
	m, err := distribution.Install(*bundle, *prefix)
	if err != nil {
		fmt.Fprintln(stderr, "install:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Installed Pomar %s (source %s)\nPATH: %s\nPrevious versions retained; no service activated.\n", m.Version, m.Commit, filepath.Join(*prefix, "current", "bin"))
	return 0
}

type diagnostic struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func doctorCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", os.Getenv("POMAR_SOCKET"), "explicit manager socket")
	machine := fs.Bool("json", false, "machine-readable diagnostics")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return 2
	}
	checks := []diagnostic{{"client", true, fmt.Sprintf("%s source %s; %s/%s", version, sourceCommit, runtime.GOOS, runtime.GOARCH)}}
	for _, tool := range []string{"pomar-host", "pomar-shim-linux-arm64"} {
		p, err := siblingTool(tool)
		d := diagnostic{tool, err == nil, p}
		if err != nil {
			d.Detail = err.Error()
		}
		checks = append(checks, d)
	}
	// Connection only: no mutation, admission, provider inspection or guessed root.
	d := diagnostic{"manager", false, "not selected: set POMAR_SOCKET or pass -socket"}
	if *socket != "" {
		c, err := net.DialTimeout("unix", *socket, 2*time.Second)
		d.Detail = *socket
		if err != nil {
			d.Detail = err.Error()
		} else {
			c.Close()
			d.OK = true
		}
	}
	checks = append(checks, d)
	rc := 0
	if *machine {
		json.NewEncoder(stdout).Encode(checks)
	} else {
		for _, d := range checks {
			s := "OK"
			if !d.OK {
				s = "FAIL"
			}
			fmt.Fprintf(stdout, "%-4s %-24s %s\n", s, d.Name, d.Detail)
		}
	}
	for _, d := range checks {
		if !d.OK {
			rc = 1
		}
	}
	return rc
}
