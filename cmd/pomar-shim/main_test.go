package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:7070": true,
		"[::1]:7070":     true,
		"0.0.0.0:7070":   false,
		":7070":          false,
		"192.0.2.1:7070": false, // RFC 5737 documentation address
		"localhost:7070": false, // a name could resolve anywhere
	} {
		if err := checkLoopback(addr); (err == nil) != ok {
			t.Errorf("checkLoopback(%q) = %v", addr, err)
		}
	}
}

func TestCheckLoopbackPort(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1"} {
		for _, port := range []string{"0", "1", "7070", "65534", "65535", "00000", "00080"} {
			addr := net.JoinHostPort(host, port)
			t.Run(addr, func(t *testing.T) {
				if err := checkLoopback(addr); err != nil {
					t.Fatalf("valid address rejected: %v", err)
				}
			})
		}
		for _, port := range []string{"", "http", "https", "+80", "-1", " 80", "80 ", "8 0", "\t80", "80\n", "８０", "٨٠", "65536", "99999", "100000", "000000", strings.Repeat("9", 1024)} {
			addr := net.JoinHostPort(host, port)
			t.Run(addr, func(t *testing.T) {
				if err := checkLoopback(addr); err == nil {
					t.Fatal("invalid port accepted")
				}
			})
		}
	}
}

// A request to the shim's port arrives unchanged at the Unix socket's
// server, and its answer comes back.
func TestForwardsToTheSocket(t *testing.T) {
	// Keep the fixture in TMPDIR without t.TempDir's test-name components:
	// the recovery workspace already consumes much of Darwin's socket budget.
	root, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "p.sock")
	if len(sock) > 103 {
		t.Fatalf("fixture socket path is %d bytes; use a shorter physical TMPDIR", len(sock))
	}
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	go http.Serve(ul, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "got %s %s", r.Method, r.RequestURI)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go serve(ln, sock)
	defer ln.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/golang.org/x/sys/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "got GET /golang.org/x/sys/@v/list" {
		t.Fatalf("body %q", line)
	}
}

func TestRunWritesReadyAfterListening(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	if err := run("0.0.0.0:0", "x", ready); err == nil {
		t.Fatal("run accepted a non-loopback address")
	}
	if _, err := os.Stat(ready); err == nil {
		t.Fatal("ready written for a refused address")
	}
}
