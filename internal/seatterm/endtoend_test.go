package seatterm

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
)

// The whole path: the operator's client on the owner-only socket, through a
// proxy shaped like the host's agent proxy, to the guest's terminal route.
func TestTheTerminalCrossesTheHostProxy(t *testing.T) {
	root, _ := os.MkdirTemp("/tmp", "seatterm")
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { os.RemoveAll(root) })

	rt, err := NewRoute(func(s, i string) bool { return s == "session-1" && i == "inc-1" }, func() error { return nil },
		Command{Path: "/bin/sh", Args: []string{"-c", `stty size; read l; echo "got:$l"`}, UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	guestMux := http.NewServeMux()
	guestMux.Handle("/v1/terminal", rt)
	guestSock := filepath.Join(root, "guest.sock")
	gl, err := net.Listen("unix", guestSock)
	if err != nil {
		t.Fatal(err)
	}
	guest := &http.Server{Handler: guestMux}
	go guest.Serve(gl)
	t.Cleanup(func() { guest.Close() })

	// As internal/agentenv's /v1/environments/{id}/agent/{path...} proxy.
	hostMux := http.NewServeMux()
	hostMux.HandleFunc("/v1/environments/{id}/agent/{path...}", func(w http.ResponseWriter, r *http.Request) {
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", guestSock)
		}}
		defer transport.CloseIdleConnections()
		(&httputil.ReverseProxy{Transport: transport, Director: func(req *http.Request) {
			req.URL.Scheme, req.URL.Host, req.URL.Path, req.Host = "http", "guest", "/v1/"+r.PathValue("path"), "guest"
		}}).ServeHTTP(w, r)
	})
	hostSock := filepath.Join(root, "host.sock")
	hl, err := net.Listen("unix", hostSock)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(hostSock, 0o600)
	host := &http.Server{Handler: hostMux}
	go host.Serve(hl)
	t.Cleanup(func() { host.Close() })

	conn, err := localclient.Dial(context.Background(), hostSock, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := Open(conn, "/v1/environments/env-1/agent/terminal?session_id=session-1&expected_incarnation=inc-1")
	if err != nil {
		t.Fatal(err)
	}
	in, typing := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Pump(stream, in, &out, 120, 40, nil) }()
	time.Sleep(300 * time.Millisecond)
	typing.Write([]byte("hello\n"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal never closed")
	}
	if !strings.Contains(out.String(), "40 120") || !strings.Contains(out.String(), "got:hello") {
		t.Fatalf("screen %q", out.String())
	}

	// A stale binding is refused at the guest, through the proxy.
	conn2, _ := localclient.Dial(context.Background(), hostSock, os.Geteuid())
	defer conn2.Close()
	if _, err := Open(conn2, "/v1/environments/env-1/agent/terminal?session_id=session-1&expected_incarnation=inc-0"); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("a stale binding: %v", err)
	}
}
