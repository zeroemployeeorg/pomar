package seatterm

import (
	"bytes"
	"errors"
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
	g, n := net.Pipe()
	defer g.Close()
	defer n.Close()
	if err := Attach(g, Command{}, 80, 24); err == nil {
		t.Fatal("an attach without a command started")
	}
}

// The POMAR Codex's counterexample (PR94 review): a peer that keeps the
// stream open but never reads can't hold Attach once its command exits.
func TestAPeerThatStopsReadingCantHoldAttach(t *testing.T) {
	guest, near := net.Pipe()
	defer near.Close()
	defer guest.Close()
	done := make(chan error, 1)
	go func() {
		done <- Attach(guest, Command{Path: "/bin/sh", Args: []string{"-c", "printf done"}, UID: -1, GID: -1}, 80, 24)
	}()
	select {
	case <-done:
	case <-time.After(DrainTimeout + 5*time.Second):
		t.Fatal("Attach was held by a peer that stopped reading")
	}
}

// The same when the peer disconnects mid-output while not reading: the
// attach client is hung up and Attach returns, bounded.
func TestASilentPeerDuringOutputCantHoldAttach(t *testing.T) {
	guest, near := net.Pipe()
	defer guest.Close()
	done := make(chan error, 1)
	go func() {
		done <- Attach(guest, Command{Path: "/bin/sh", Args: []string{"-c", "while :; do echo spam; done"}, UID: -1, GID: -1}, 80, 24)
	}()
	time.Sleep(300 * time.Millisecond)
	WriteFrame(near, Close, nil) // ask to end, then never read the screen
	select {
	case <-done:
	case <-time.After(DrainTimeout + 8*time.Second):
		t.Fatal("Attach was held by a peer that asked to close but never read")
	}
	near.Close()
}

// A short write is an error, never a silently truncated frame.
func TestAShortWriteIsAnError(t *testing.T) {
	if err := WriteFrame(shortWriter{}, Data, []byte("abcdef")); err == nil {
		t.Fatal("a short write passed")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		return 1, nil
	}
	return len(p), nil
}

// A terminal that stops reading its input can't hold Attach: the stream is
// still read, so the operator's disconnect is seen and the attach ends,
// bounded, though the PTY's input is full (the POMAR Codex's review of #106).
func TestANonConsumingTerminalCantHoldAttach(t *testing.T) {
	guest, near := net.Pipe()
	defer guest.Close()
	done := make(chan error, 1)
	go func() {
		done <- Attach(guest, Command{Path: "/bin/sh", Args: []string{"-c", "stty raw -echo; echo ready; exec sleep 600"}, UID: -1, GID: -1}, 80, 24)
	}()
	go func() { // the screen is read throughout
		for {
			if _, _, err := ReadFrame(near); err != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	keys := bytes.Repeat([]byte("k"), MaxData)
	near.SetWriteDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 4; i++ { // more than the PTY's input holds, less than the queue
		if err := WriteFrame(near, Data, keys); err != nil {
			t.Fatalf("the input frames weren't read: %v", err)
		}
	}
	near.Close()
	select {
	case <-done:
	case <-time.After(DrainTimeout + 8*time.Second):
		t.Fatal("Attach was held by a terminal that stopped reading its input")
	}
}

// Input the terminal never takes is bounded too: once the queue is full the
// attach ends with ErrInputStalled, rather than reading without limit or
// stopping reading the stream.
func TestUnconsumedInputEndsTheAttach(t *testing.T) {
	guest, near := net.Pipe()
	defer near.Close()
	defer guest.Close()
	done := make(chan error, 1)
	go func() {
		done <- Attach(guest, Command{Path: "/bin/sh", Args: []string{"-c", "stty raw -echo; exec sleep 600"}, UID: -1, GID: -1}, 80, 24)
	}()
	go func() {
		for {
			if _, _, err := ReadFrame(near); err != nil {
				return
			}
		}
	}()
	go func() {
		keys := bytes.Repeat([]byte("k"), MaxData)
		for WriteFrame(near, Data, keys) == nil {
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInputStalled) {
			t.Fatalf("Attach ended with %v, want ErrInputStalled", err)
		}
	case <-time.After(DrainTimeout + 8*time.Second):
		t.Fatal("unconsumed input held Attach")
	}
}

// A stream whose deadlines can't be set is refused before anything starts:
// without them no side of the teardown is bounded.
func TestAStreamWithoutDeadlinesIsRefused(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	err := Attach(noDeadlines{}, Command{Path: "/bin/sh", Args: []string{"-c", "touch " + marker}, UID: -1, GID: -1}, 80, 24)
	if err == nil {
		t.Fatal("a stream without deadlines was accepted")
	}
	time.Sleep(200 * time.Millisecond)
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the command ran on a refused stream")
	}
}

type noDeadlines struct{}

func (noDeadlines) Read([]byte) (int, error)         { select {} }
func (noDeadlines) Write(p []byte) (int, error)      { return len(p), nil }
func (noDeadlines) SetReadDeadline(time.Time) error  { return errors.New("no deadlines") }
func (noDeadlines) SetWriteDeadline(time.Time) error { return errors.New("no deadlines") }
