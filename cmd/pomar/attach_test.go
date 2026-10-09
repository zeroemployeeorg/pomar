package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/seatterm"
)

// attachHost serves a guest (session and terminal) behind a host-shaped
// agent proxy on an owner-only socket.
func attachHost(t *testing.T, incarnation string) string {
	t.Helper()
	root, _ := os.MkdirTemp("/tmp", "attach")
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { os.RemoveAll(root) })
	rt, err := seatterm.NewRoute(func(s, i string) bool { return s == "session-1" && i == "inc-1" }, func() error { return nil },
		seatterm.Command{Path: "/bin/sh", Args: []string{"-c", `stty size; read l; echo "got:$l"`}, UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	guestMux := http.NewServeMux()
	guestMux.Handle("/v1/terminal", rt)
	guestMux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]string{"session_id": "session-1", "incarnation": incarnation}})
	})
	guestSock := filepath.Join(root, "guest.sock")
	gl, _ := net.Listen("unix", guestSock)
	guest := &http.Server{Handler: guestMux}
	go guest.Serve(gl)
	t.Cleanup(func() { guest.Close() })
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
	hl, _ := net.Listen("unix", filepath.Join(root, "host.sock"))
	os.Chmod(filepath.Join(root, "host.sock"), 0o600)
	host := &http.Server{Handler: hostMux}
	go host.Serve(hl)
	t.Cleanup(func() { host.Close() })
	return root
}

func TestSeatAttachCarriesTheTerminalBoundToTheCurrentIncarnation(t *testing.T) {
	root := attachHost(t, "inc-1")
	in, typing, _ := os.Pipe()
	defer in.Close()
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- seatAttach([]string{"-root", root, "-environment", "env-1"}, in, &out, &errb) }()
	time.Sleep(300 * time.Millisecond)
	typing.Write([]byte("hello\n"))
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("%d %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attach never ended")
	}
	typing.Close()
	if !strings.Contains(out.String(), "24 80") || !strings.Contains(out.String(), "got:hello") {
		t.Fatalf("screen %q", out.String())
	}
}

func TestSeatAttachRefusesAStaleBindingAndBadFlags(t *testing.T) {
	root := attachHost(t, "inc-0") // the session reports an incarnation the guest's route won't accept
	in, _, _ := os.Pipe()
	defer in.Close()
	var out, errb bytes.Buffer
	if code := seatAttach([]string{"-root", root, "-environment", "env-1"}, in, &out, &errb); code != 1 || !strings.Contains(errb.String(), "409") {
		t.Fatalf("%d %s", code, errb.String())
	}
	if code := seatAttach([]string{"-root", root, "-environment", "../env"}, in, &out, &errb); code != 2 {
		t.Fatal("an invalid environment name was accepted")
	}
}
