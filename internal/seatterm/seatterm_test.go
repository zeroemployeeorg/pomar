package seatterm

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the processes an attach runs: "daemon" starts
// a "survivor" in a session of its own (as tmux starts its server) and then
// waits on its terminal, as a tmux client does.
func TestMain(m *testing.M) {
	switch os.Getenv("SEATTERM_HELPER") {
	case "daemon":
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "SEATTERM_HELPER=survivor")
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			os.Exit(3)
		}
		os.WriteFile(os.Getenv("SEATTERM_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		os.Stdout.WriteString("ready\n")
		buf := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				os.Exit(0)
			}
		}
	case "survivor":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// client reads the guest's frames in the background.
type client struct {
	conn   net.Conn
	mu     sync.Mutex
	screen bytes.Buffer
	closed chan struct{}
}

func dial(t *testing.T, c Command, cols, rows uint16) (*client, chan error) {
	t.Helper()
	guest, near := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Attach(guest, c, cols, rows); guest.Close() }()
	cl := &client{conn: near, closed: make(chan struct{})}
	go func() {
		defer close(cl.closed)
		for {
			kind, payload, err := ReadFrame(near)
			if err != nil || kind == Close {
				return
			}
			cl.mu.Lock()
			cl.screen.Write(payload)
			cl.mu.Unlock()
		}
	}()
	return cl, done
}

func (c *client) waitFor(t *testing.T, s string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		c.mu.Lock()
		got := c.screen.String()
		c.mu.Unlock()
		if strings.Contains(got, s) {
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("never saw %q in %q", s, c.screen.String())
}

func TestTheTerminalCarriesSizeKeysAndScreen(t *testing.T) {
	cl, done := dial(t, Command{Path: "/bin/sh", Args: []string{"-c", `stty size; read l; echo "got:$l"`}, UID: -1, GID: -1}, 100, 30)
	cl.waitFor(t, "30 100")
	if err := WriteFrame(cl.conn, Data, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	cl.waitFor(t, "got:hello")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attach didn't end when its command did")
	}
	<-cl.closed
}

func TestAResizeReachesTheTerminal(t *testing.T) {
	cl, done := dial(t, Command{Path: "/bin/sh", Args: []string{"-c", `read l; stty size`}, UID: -1, GID: -1}, 80, 24)
	WriteFrame(cl.conn, Resize, ResizePayload(132, 43))
	time.Sleep(100 * time.Millisecond)
	WriteFrame(cl.conn, Data, []byte("\n"))
	cl.waitFor(t, "43 132")
	<-done
}

// The operator's connection drops: the attach client is hung up, and what
// it started in another session (as tmux's server, and the actor in it)
// keeps running.
func TestADisconnectHangsUpOnlyTheAttachClient(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "survivor")
	cl, done := dial(t, Command{Path: os.Args[0], Env: append(os.Environ(), "SEATTERM_HELPER=daemon", "SEATTERM_PIDFILE="+pidfile), UID: -1, GID: -1}, 80, 24)
	cl.waitFor(t, "ready")
	b, _ := os.ReadFile(pidfile)
	survivor, _ := strconv.Atoi(string(b))
	if survivor == 0 {
		t.Fatal("no survivor recorded")
	}
	t.Cleanup(func() { syscall.Kill(survivor, syscall.SIGKILL) })
	cl.conn.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("attach outlived its client")
	}
	if err := syscall.Kill(survivor, 0); err != nil {
		t.Fatalf("the process in another session was ended: %v", err)
	}
}

func TestAMalformedFrameEndsTheAttach(t *testing.T) {
	cl, done := dial(t, Command{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}, UID: -1, GID: -1}, 80, 24)
	cl.conn.Write([]byte{'q', 0, 0})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a malformed frame didn't end the attach")
	}
}

func TestFramesAreBounded(t *testing.T) {
	var b bytes.Buffer
	if WriteFrame(&b, Data, make([]byte, MaxData+1)) == nil {
		t.Fatal("an oversized frame was written")
	}
	for name, raw := range map[string][]byte{
		"an unknown type":        {'z', 0, 0},
		"a close with a payload": {'x', 0, 1, 'a'},
		"a short resize":         {'r', 0, 2, 0, 80},
		"a zero resize":          append([]byte{'r', 0, 4}, ResizePayload(0, 24)...),
		"an oversized resize":    append([]byte{'r', 0, 4}, ResizePayload(5000, 24)...),
		"a truncated payload":    {'d', 0, 9, 'a'},
		"an oversized length":    {'d', 0xff, 0xff},
	} {
		if _, _, err := ReadFrame(bytes.NewReader(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	b.Reset()
	WriteFrame(&b, Resize, ResizePayload(80, 24))
	if kind, p, err := ReadFrame(&b); err != nil || kind != Resize || len(p) != 4 {
		t.Fatal(kind, p, err)
	}
	if err := Attach(&b, Command{}, 80, 24); err == nil {
		t.Fatal("an attach without a command started")
	}
}
