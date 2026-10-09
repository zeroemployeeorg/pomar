package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestHelpIsSuccessfulAndDoesNotConnect(t *testing.T) {
	t.Setenv("POMAR_SOCKET", "/not/a/socket")
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"help", "all"}, {"help", "server"}, {"help", "install"}} {
		var out, err bytes.Buffer
		if rc := run(args, &out, &err); rc != 0 || out.Len() == 0 || err.Len() != 0 {
			t.Fatalf("%v: %d %s", args, rc, err.String())
		}
	}
}
func TestFriendlyCommandsUseExplicitSocketAndExistingAPI(t *testing.T) {
	dir, err := os.MkdirTemp(os.TempDir(), "pc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "ctl.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var path, method string
	var mu sync.Mutex
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path, method = r.URL.Path, r.Method
		mu.Unlock()
		switch path {
		case "/v1/capacity":
			w.Write([]byte(`{}`))
		case "/v1/attempts":
			if r.Method == "GET" {
				w.Write([]byte(`[]`))
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["id"] != "test-job" {
				t.Errorf("wrong job %v", body)
			}
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
		}
	})}
	go srv.Serve(l)
	defer srv.Close()
	t.Setenv("POMAR_SOCKET", socket)
	t.Setenv("POMAR_DATA_ROOT", "/not/the/owner")
	for _, tc := range []struct {
		args         []string
		path, method string
	}{{[]string{"status"}, "/v1/capacity", "GET"}, {[]string{"jobs"}, "/v1/attempts", "GET"}, {[]string{"run", "-id", "test-job", "--", "true"}, "/v1/attempts", "POST"}, {[]string{"run", "-id", "test-job", "true", "-root", "/guest"}, "/v1/attempts", "POST"}} {
		var out, err bytes.Buffer
		if rc := run(tc.args, &out, &err); rc != 0 {
			t.Fatalf("%v: %d %s", tc.args, rc, err.String())
		}
		mu.Lock()
		if path != tc.path || method != tc.method {
			t.Fatalf("%v used %s %s", tc.args, method, path)
		}
		mu.Unlock()
	}
}
func TestExplicitRootOverridesSocketEnvironment(t *testing.T) {
	t.Setenv("POMAR_SOCKET", "/env/notused")
	var out, err bytes.Buffer
	rc := run([]string{"status", "-root", "/explicit/root"}, &out, &err)
	if rc == 0 || !strings.Contains(err.String(), "/explicit/root") || strings.Contains(err.String(), "/env/notused") {
		t.Fatalf("rc=%d error=%s", rc, err.String())
	}
}
func TestServerCannotInheritADataRoot(t *testing.T) {
	t.Setenv("POMAR_DATA_ROOT", t.TempDir())
	for _, args := range [][]string{{"server", "start"}, {"server", "prepare", "-root", "relative"}, {"server", "kernel", "-root", "relative"}} {
		var out, err bytes.Buffer
		if rc := run(args, &out, &err); rc != 2 {
			t.Fatalf("%v: rc %d %s", args, rc, err.String())
		}
	}
}
func TestDoctorJSONDoesNotCreateRoots(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent.sock")
	var out, err bytes.Buffer
	if rc := run([]string{"doctor", "-json", "-socket", p}, &out, &err); rc != 1 {
		t.Fatal(rc)
	}
	var d []diagnostic
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d) != 4 || d[3].OK {
		t.Fatal(d)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("doctor created state")
	}
}
func TestGuestFlagsDoNotSelectTheOwner(t *testing.T) {
	if explicitFlag([]string{"--", "echo", "-root", "/guest"}, "root") {
		t.Fatal("guest argv selected owner")
	}
}
