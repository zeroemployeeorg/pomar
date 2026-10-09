package seatterm

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serveRoute(t *testing.T, ensure func() error, script string) string {
	t.Helper()
	rt, err := NewRoute(func(s, i string) bool { return s == "session-1" && i == "inc-1" }, ensure,
		Command{Path: "/bin/sh", Args: []string{"-c", script}, UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/terminal", rt)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// upgrade sends the terminal request and returns the connection and the
// status line.
func upgrade(t *testing.T, addr, query, upgradeHeader string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "POST /v1/terminal?%s HTTP/1.1\r\nHost: guest\r\nConnection: Upgrade\r\nUpgrade: %s\r\nContent-Length: 0\r\n\r\n", query, upgradeHeader)
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	status, _ := br.ReadString('\n')
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	return c, br, status
}

func TestTheTerminalRouteUpgradesAndCarriesTheScreen(t *testing.T) {
	addr := serveRoute(t, func() error { return nil }, "stty size")
	c, br, status := upgrade(t, addr, "session_id=session-1&expected_incarnation=inc-1", Protocol)
	if !strings.Contains(status, "101") {
		t.Fatalf("status %q", status)
	}
	WriteFrame(c, Resize, ResizePayload(120, 40))
	var screen strings.Builder
	for {
		kind, p, err := ReadFrame(br)
		if err != nil || kind == Close {
			break
		}
		screen.Write(p)
	}
	if !strings.Contains(screen.String(), "40 120") {
		t.Fatalf("screen %q", screen.String())
	}
}

func TestTheTerminalRouteRefusesBadRequests(t *testing.T) {
	addr := serveRoute(t, func() error { return nil }, "stty size")
	for name, c := range map[string]struct{ query, upgrade, want string }{
		"a stale incarnation": {"session_id=session-1&expected_incarnation=inc-0", Protocol, "409"},
		"another session":     {"session_id=session-2&expected_incarnation=inc-1", Protocol, "409"},
		"no binding":          {"", Protocol, "409"},
		"another protocol":    {"session_id=session-1&expected_incarnation=inc-1", "websocket", "400"},
	} {
		if _, _, status := upgrade(t, addr, c.query, c.upgrade); !strings.Contains(status, c.want) {
			t.Errorf("%s: %q", name, status)
		}
	}
	// The first frame must be the window size.
	conn, br, status := upgrade(t, addr, "session_id=session-1&expected_incarnation=inc-1", Protocol)
	if !strings.Contains(status, "101") {
		t.Fatal(status)
	}
	WriteFrame(conn, Data, []byte("ls\n"))
	if kind, _, err := ReadFrame(br); err != nil || kind != Close {
		t.Fatalf("a first frame that isn't a resize: %q %v", kind, err)
	}
}

func TestTheSeatsSessionMustExistFirst(t *testing.T) {
	addr := serveRoute(t, func() error { return errors.New("no tmux") }, "stty size")
	if _, _, status := upgrade(t, addr, "session_id=session-1&expected_incarnation=inc-1", Protocol); !strings.Contains(status, "503") {
		t.Fatalf("status %q", status)
	}
}

func TestAttachesAreBounded(t *testing.T) {
	addr := serveRoute(t, func() error { return nil }, "sleep 5")
	for i := 0; i < MaxAttaches; i++ {
		c, _, status := upgrade(t, addr, "session_id=session-1&expected_incarnation=inc-1", Protocol)
		if !strings.Contains(status, "101") {
			t.Fatalf("attach %d: %q", i, status)
		}
		WriteFrame(c, Resize, ResizePayload(80, 24))
	}
	time.Sleep(200 * time.Millisecond)
	if _, _, status := upgrade(t, addr, "session_id=session-1&expected_incarnation=inc-1", Protocol); !strings.Contains(status, "429") {
		t.Fatalf("an attach past the bound: %q", status)
	}
	if _, err := NewRoute(nil, nil, Command{}); err == nil {
		t.Fatal("a route without a binding was made")
	}
}
