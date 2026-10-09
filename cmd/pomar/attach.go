package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

var environmentName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

// seatAttach opens a seat environment's interactive terminal (SOW 15 §4,
// §5): it binds the terminal to the environment's current session and
// incarnation, puts the local terminal in raw mode, and carries it until
// the seat's terminal closes. Detaching is tmux's own Ctrl-b d, and the
// seat keeps running; a dropped connection leaves it running too.
func seatAttach(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seat attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := seatHostRoot(fs)
	env := fs.String("environment", "", "the seat's environment")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !environmentName.MatchString(*env) {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var s struct {
		Session struct {
			ID          string `json:"session_id"`
			Incarnation string `json:"incarnation"`
		} `json:"session"`
	}
	if err := seatGet(*root, "/v1/environments/"+*env+"/agent/session", &s); err != nil {
		fmt.Fprintln(stderr, "pomar seat attach:", err)
		return 1
	}
	if s.Session.ID == "" || s.Session.Incarnation == "" {
		fmt.Fprintln(stderr, "pomar seat attach: the environment reported no session or incarnation")
		return 1
	}
	conn, err := localclient.Dial(context.Background(), filepath.Join(*root, "host.sock"), os.Geteuid())
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat attach:", err)
		return 1
	}
	defer conn.Close()
	q := url.Values{"session_id": {s.Session.ID}, "expected_incarnation": {s.Session.Incarnation}}
	stream, err := seatterm.Open(conn, "/v1/environments/"+*env+"/agent/terminal?"+q.Encode())
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat attach:", err)
		return 1
	}
	cols, rows := uint16(80), uint16(24)
	var resizes chan [2]uint16
	if onTerminal(stdin) {
		if c, r, err := ttySize(stdin); err == nil {
			cols, rows = c, r
		}
		restore, err := rawMode(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "pomar seat attach:", err)
			return 1
		}
		defer restore()
		resizes = make(chan [2]uint16, 4)
		winch := make(chan os.Signal, 4)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if c, r, err := ttySize(stdin); err == nil {
					resizes <- [2]uint16{c, r}
				}
			}
		}()
		fmt.Fprintf(stderr, "Attached to %s. Detach with Ctrl-b d; the seat keeps running.\r\n", *env)
	}
	if err := seatterm.Pump(stream, stdin, stdout, cols, rows, resizes); err != nil {
		fmt.Fprintln(stderr, "\r\npomar seat attach:", err)
		return 1
	}
	return 0
}

func stty(f *os.File, args ...string) (string, error) {
	c := exec.Command("/bin/stty", args...)
	c.Stdin = f
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}

// ttySize reads the local terminal's columns and rows.
func ttySize(f *os.File) (uint16, uint16, error) {
	out, err := stty(f, "size")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, errors.New("unreadable terminal size")
	}
	rows, e1 := strconv.Atoi(fields[0])
	cols, e2 := strconv.Atoi(fields[1])
	if e1 != nil || e2 != nil || cols < 1 || cols > seatterm.MaxCols || rows < 1 || rows > seatterm.MaxRows {
		return 0, 0, errors.New("terminal size out of bounds")
	}
	return uint16(cols), uint16(rows), nil
}

// rawMode puts the local terminal in raw mode and returns its restore.
func rawMode(f *os.File) (func(), error) {
	saved, err := stty(f, "-g")
	if err != nil {
		return nil, err
	}
	if _, err := stty(f, "raw", "-echo"); err != nil {
		return nil, err
	}
	return func() { stty(f, saved) }, nil
}
