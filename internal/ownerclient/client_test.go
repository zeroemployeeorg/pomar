//go:build darwin

package ownerclient

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func fixture(t *testing.T, handler http.HandlerFunc) Config {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "oc-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	records := filepath.Join(root, "records")
	os.Mkdir(records, 0700)
	request := filepath.Join(root, "request.json")
	os.WriteFile(request, []byte(`{"operation_id":"original-stop","expected_incarnation":"inc-original"}`), 0600)
	ln, err := net.Listen("unix", filepath.Join(root, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(root, "host.sock"), 0600)
	s := &http.Server{Handler: handler}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return Config{Root: root, Records: records, Environment: "fixture", Action: "stop", OperationID: "original-stop", Request: request, Session: "session-original", Incarnation: "inc-original"}
}

func TestLostResponseReconcilesWithoutRepeatingEffect(t *testing.T) {
	var posts, gets atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		gets.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/operations/original-stop") || r.URL.Query().Get("session_id") != "session-original" || r.URL.Query().Get("expected_incarnation") != "inc-original" {
			t.Error("original operation binding missing")
		}
		w.Write([]byte(`{"action":{"state":"completed"}}`))
	})
	if _, err := Run(c); !errors.Is(err, ErrUncertain) {
		t.Fatalf("lost response: %v", err)
	}
	if _, err := Run(c); !errors.Is(err, ErrUncertain) {
		t.Fatalf("blind retry: %v", err)
	}
	if posts.Load() != 1 || gets.Load() != 0 {
		t.Fatal("blind retry reached API")
	}
	c.Reconcile = true
	r, err := Run(c)
	if err != nil || r.Outcome != "completed" {
		t.Fatalf("reconcile: %+v %v", r, err)
	}
	if posts.Load() != 1 || gets.Load() != 1 {
		t.Fatal("effect repeated during reconciliation")
	}
	c.Reconcile = false
	r, err = Run(c)
	if err != nil || !r.Cached || posts.Load() != 1 || gets.Load() != 1 {
		t.Fatal("recorded response was not recovered locally")
	}
}

func TestChangedInputConflictsBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"action":{"state":"completed"}}`))
	})
	if _, err := Run(c); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(c.Request, []byte("{\n\"operation_id\":\"original-stop\",\"expected_incarnation\":\"inc-original\"}"), 0600)
	if _, err := Run(c); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed bytes: %v", err)
	}
	c.Action = "start"
	if _, err := Run(c); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed action: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("conflicting input dispatched")
	}
}

func TestRetryNeedsFreshDurableNonAcceptance(t *testing.T) {
	var posts, gets atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if posts.Add(1) == 1 {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			w.Write([]byte(`{"action":{"state":"completed"}}`))
			return
		}
		gets.Add(1)
		w.WriteHeader(404)
		w.Write([]byte(`{"evidence":"durable_non_acceptance"}`))
	})
	Run(c)
	c.Reconcile = true
	if _, err := Run(c); err == nil {
		t.Fatal("non-acceptance alone retried effect")
	}
	if posts.Load() != 1 {
		t.Fatal("mutation repeated without explicit retry")
	}
	c.RetryKnownUnaccepted = true
	if _, err := Run(c); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 || posts.Load() != 2 {
		t.Fatal("retry did not inspect again and preserve original request")
	}
}

func TestProxyFailureDoesNotMeanNonAcceptance(t *testing.T) {
	var posts atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			w.WriteHeader(502)
			w.Write([]byte(`{"error":"sensitive-provider-value"}`))
			return
		}
		w.Write([]byte(`{"action":{"state":"acceptance_unknown"}}`))
	})
	r, err := Run(c)
	if err == nil || strings.Contains(err.Error(), "sensitive-provider-value") {
		t.Fatal("provider detail escaped or refusal hidden")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "sensitive-provider-value") {
		t.Fatal("response escaped metadata")
	}
	c.Reconcile = true
	r, err = Run(c)
	if !errors.Is(err, ErrUncertain) || r.OperationState != "acceptance_unknown" || posts.Load() != 1 {
		t.Fatal("502 incorrectly established non-acceptance")
	}
}

func TestPrivateInputAndSocketRefusals(t *testing.T) {
	var calls atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	os.Chmod(c.Request, 0644)
	if _, err := Run(c); err == nil {
		t.Fatal("public request accepted")
	}
	os.Chmod(c.Request, 0600)
	real := c.Request + "-real"
	os.Rename(c.Request, real)
	os.Symlink(real, c.Request)
	if _, err := Run(c); err == nil {
		t.Fatal("symlink request accepted")
	}
	os.Remove(c.Request)
	os.Rename(real, c.Request)
	os.Chmod(filepath.Join(c.Root, "host.sock"), 0666)
	if _, err := Run(c); err == nil {
		t.Fatal("public socket accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("refused request reached server")
	}
}

func TestLoginResponseAndInputRemainPrivate(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":{"success":true,"authUrl":"https://fixture.invalid/PRIVATE-CHALLENGE"}}`))
	})
	c.Action = "login-complete"
	os.WriteFile(c.Request, []byte(`{"operation_id":"original-stop","expected_incarnation":"inc-original","login_id":"fixture-login","code":"PRIVATE-CODE"}`), 0600)
	r, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("login content in diagnostic metadata")
	}
	intent, _ := os.ReadFile(filepath.Join(c.Records, c.OperationID, "intent.json"))
	if strings.Contains(string(intent), "PRIVATE") {
		t.Fatal("login content journaled")
	}
	response, _ := os.Stat(filepath.Join(c.Records, c.OperationID, "event-00000000002.json"))
	if response.Mode().Perm() != 0600 {
		t.Fatal("login response not private")
	}
}
