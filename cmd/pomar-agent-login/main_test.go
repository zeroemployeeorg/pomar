package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
	"github.com/zeroemployeeorg/pomar/internal/claudeactor"
)

const code = "synthetic-code-0123456789"

// A shell script that behaves like `claude auth login --claudeai` with no
// terminal (probed on 2.1.280): the URL, then a code on stdin.
const fakeLogin = `#!/bin/sh
[ -n "$CALLS" ] && echo start >> "$CALLS"
echo "Opening browser to sign in…"
echo "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=c&state=s"
printf 'Paste code here if prompted > '
read c
[ "$c" = "` + code + `" ] || exit 1
[ -n "$GATE" ] && { echo waiting >> "$CALLS"; while [ ! -e "$GATE" ]; do sleep 0.05; done; }
printf 'synthetic-credential' > "$CRED"
[ -n "$CALLS" ] && echo complete >> "$CALLS"
`

// host serves a real broker, with a Claude actor whose sign-in is the fake
// script, behind a host-shaped socket: /v1/environments/env-1/agent/...
func host(t *testing.T) (root string, config string) {
	root, config, _ = hostWith(t)
	return root, config
}

// hostWith is host, also returning the fake sign-in's call log: a "start" line
// each time the actor starts a sign-in, a "complete" line each time one
// completes.
func hostWith(t *testing.T) (root string, config string, calls string) {
	return hostGated(t, "")
}

// hostGated is hostWith, with a sign-in completion that, after reading a
// right code, logs "waiting" and blocks until the file gate exists.
func hostGated(t *testing.T, gate string) (root string, config string, calls string) {
	t.Helper()
	dir := t.TempDir()
	root, _ = os.MkdirTemp("/tmp", "pal")
	t.Cleanup(func() { os.RemoveAll(root) })
	config = filepath.Join(dir, "cfg")
	os.Mkdir(config, 0o700)
	script := filepath.Join(dir, "claude")
	os.WriteFile(script, []byte(fakeLogin), 0o755)
	store, err := agentenv.Open(filepath.Join(dir, "state"), "env-1", "session-1", "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	broker := agentenv.NewBroker(store, dir)
	calls = filepath.Join(dir, "calls")
	env := []string{"PATH=/usr/bin:/bin", "CRED=" + filepath.Join(config, ".credentials.json"), "CALLS=" + calls, "GATE=" + gate}
	actor := claudeactor.New(claudeactor.Config{
		Version:       "2.1.280",
		Start:         claudeactor.Exec{Binary: script, Dir: dir, Env: env, UID: -1, GID: -1}.Start,
		Authenticated: claudeactor.CredentialPresent(config),
		Login: func() (claudeactor.Process, error) {
			return claudeactor.Exec{Binary: script, Dir: dir, Env: env, UID: -1, GID: -1}.Start(nil)
		},
	}, broker.Observe)
	if err := broker.Initialize(t.Context(), actor); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(root, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/environments/env-1/agent/", http.StripPrefix("/v1/environments/env-1/agent", rewrite(broker.Handler())))
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return root, config, calls
}

// rewrite maps agent/{path} to /v1/{path}, as the host proxy does.
func rewrite(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/v1" + r.URL.Path
		h.ServeHTTP(w, r)
	})
}

func stdinWith(t *testing.T, s string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s)
	f.Seek(0, 0)
	return f
}

// One command: the URL is printed, the code read, the sign-in completed.
func TestInteractiveSignIn(t *testing.T) {
	root, _ := host(t)
	var out, errb bytes.Buffer
	if err := run([]string{"-root", root, "-environment", "env-1"}, stdinWith(t, code+"\n"), &out, &errb); err != nil {
		t.Fatalf("%v; %s", err, errb.String())
	}
	if !strings.Contains(out.String(), "https://claude.com/cai/oauth/authorize?") || !strings.Contains(out.String(), "signed in: authenticated: true") {
		t.Fatalf("output %q", out.String())
	}
	if strings.Contains(out.String()+errb.String(), code) {
		t.Fatal("the code was printed")
	}
	out.Reset()
	if err := run([]string{"-root", root, "-environment", "env-1", "-status"}, stdinWith(t, ""), &out, &errb); err != nil || out.String() != "authenticated: true\n" {
		t.Fatalf("status %q, %v", out.String(), err)
	}
}

// The scripted form: -start prints JSON, -complete takes the code on stdin;
// running it again renews.
func TestScriptedSignInAndRenewal(t *testing.T) {
	root, config := host(t)
	for round := 0; round < 2; round++ {
		var out, errb bytes.Buffer
		if err := run([]string{"-root", root, "-environment", "env-1", "-start", "-operation-id", fmt.Sprintf("renew-start.%d", round)}, stdinWith(t, ""), &out, &errb); err != nil {
			t.Fatalf("start: %v; %s", err, errb.String())
		}
		var started struct {
			LoginID string `json:"login_id"`
			AuthURL string `json:"auth_url"`
		}
		if err := json.Unmarshal(out.Bytes(), &started); err != nil || started.LoginID == "" || !strings.HasPrefix(started.AuthURL, "https://") {
			t.Fatalf("start output %q", out.String())
		}
		os.Remove(filepath.Join(config, ".credentials.json"))
		out.Reset()
		if err := run([]string{"-root", root, "-environment", "env-1", "-complete", started.LoginID, "-operation-id", fmt.Sprintf("renew-complete.%d", round)}, stdinWith(t, code+"\n"), &out, &errb); err != nil {
			t.Fatalf("complete (round %d): %v; %s", round, err, errb.String())
		}
		if !strings.Contains(out.String(), "authenticated: true") {
			t.Fatalf("complete output %q", out.String())
		}
	}
}

// A wrong code fails, and says to run it again for a fresh URL.
func TestWrongCodeFails(t *testing.T) {
	root, _ := host(t)
	var out, errb bytes.Buffer
	err := run([]string{"-root", root, "-environment", "env-1"}, stdinWith(t, "wrong-code-0000000\n"), &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("a wrong code: %v", err)
	}
}
