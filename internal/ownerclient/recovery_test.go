//go:build darwin

package ownerclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUncertainRetainedStatesNeverSucceedAndInspectionRefreshes(t *testing.T) {
	for _, state := range []string{"dispatching", "acceptance_unknown", "unrecognised"} {
		t.Run(state, func(t *testing.T) {
			var posts, gets atomic.Int32
			c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
					json.NewEncoder(w).Encode(map[string]string{"state": state})
					return
				}
				if gets.Add(1) == 1 {
					w.Write([]byte(`{"action":{"state":"acceptance_unknown"}}`))
				} else {
					w.Write([]byte(`{"action":{"state":"completed"}}`))
				}
			})
			if _, err := Run(c); !errors.Is(err, ErrUncertain) {
				t.Fatal("retained state succeeded", err)
			}
			if v, err := Run(c); !errors.Is(err, ErrUncertain) || !v.Cached {
				t.Fatal("uncertainty cache became success", v, err)
			}
			c.Reconcile = true
			a, err := Run(c)
			if !errors.Is(err, ErrUncertain) || a.Cached {
				t.Fatal(a, err)
			}
			b, err := Run(c)
			if err != nil || b.Cached || b.EventID == a.EventID || b.OperationState != "completed" || posts.Load() != 1 {
				t.Fatal(b, err)
			}
			old, _ := os.ReadFile(filepath.Join(c.Records, c.OperationID, a.EventID+".json"))
			var prior struct {
				Value response `json:"value"`
			}
			json.Unmarshal(old, &prior)
			if !bytes.Contains(prior.Value.Body, []byte("acceptance_unknown")) {
				t.Fatal("earlier evidence overwritten")
			}
		})
	}
}

func Test409IsNotFinalRefusalOrRetryEvidence(t *testing.T) {
	var posts atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			w.WriteHeader(409)
			return
		}
		w.Write([]byte(`{"action":{"state":"completed"}}`))
	})
	if _, err := Run(c); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	c.Reconcile = true
	c.RetryKnownUnaccepted = true
	if _, err := Run(c); err != nil || posts.Load() != 1 {
		t.Fatal("409 allowed redispatch or blocked fresh resolution", err)
	}
}

func TestOneTimeLoginReplySurvivesPermittedRetry(t *testing.T) {
	var posts, gets atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if posts.Add(1) == 1 {
				w.WriteHeader(502)
				return
			}
			w.Write([]byte(`{"operation":{"state":"sent"},"login":{"authUrl":"https://fixture.invalid/ONCE"}}`))
			return
		}
		if gets.Add(1) > 1 {
			w.Write([]byte(`{"state":"sent"}`))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"evidence":"durable_non_acceptance"}`))
	})
	c.Action = "login"
	Run(c)
	c.Reconcile = true
	c.RetryKnownUnaccepted = true
	a, err := Run(c)
	if err != nil || !a.ResponseAvailable || posts.Load() != 2 || gets.Load() != 1 {
		t.Fatal(a, err)
	}
	c.Reconcile = false
	b, err := Run(c)
	if err != nil || !b.Cached || !b.ResponseAvailable || posts.Load() != 2 {
		t.Fatal(b, err)
	}
	c.Reconcile = true
	c.RetryKnownUnaccepted = false
	observed, e := Run(c)
	if !errors.Is(e, ErrUncertain) || observed.ResponseAvailable || !observed.RetainedResponseAvailable || observed.RetainedResponseEventID != a.EventID || observed.EventID == a.EventID || posts.Load() != 2 {
		t.Fatal("fresh control observation hid retained one-time reply", observed, e)
	}
	if status, body, e := RetainedResponse(c.Records, c.OperationID); e != nil || status != 200 || !bytes.Contains(body, []byte("ONCE")) || posts.Load() != 2 {
		t.Fatal("retained one-time reply not readable after a fresh observation", status, e)
	}
	if _, _, e := RetainedResponse(c.Records, "never-dispatched"); e == nil {
		t.Fatal("a response was invented for an operation never dispatched")
	}
	dir := filepath.Join(c.Records, c.OperationID)
	old, _ := os.ReadFile(filepath.Join(dir, "event-00000000002.json"))
	if !bytes.Contains(old, []byte(`"Status":502`)) {
		t.Fatal("original refusal lost")
	}
	saved, _ := os.ReadFile(filepath.Join(dir, a.EventID+".json"))
	var event struct {
		Value response `json:"value"`
	}
	json.Unmarshal(saved, &event)
	if !bytes.Contains(event.Value.Body, []byte("ONCE")) {
		t.Fatal("one-time reply lost")
	}
}

func TestResponsePersistenceFailureNeverAcknowledgesOrRecreatesLogin(t *testing.T) {
	var c Config
	var posts atomic.Int32
	c = fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			os.Chmod(filepath.Join(c.Records, c.OperationID), 0500)
			w.Write([]byte(`{"operation":{"state":"sent"},"login":{"authUrl":"https://fixture.invalid/ONE-TIME"}}`))
			return
		}
		w.Write([]byte(`{"operation_id":"original-stop","state":"sent"}`))
	})
	c.Action = "login"
	a, err := Run(c)
	if !errors.Is(err, ErrUncertain) || a.ResponseAvailable || a.Transport != "response-not-durable" {
		t.Fatal(a, err)
	}
	os.Chmod(filepath.Join(c.Records, c.OperationID), 0700)
	if _, err = Run(c); !errors.Is(err, ErrUncertain) {
		t.Fatal("restart acknowledged missing reply", err)
	}
	c.Reconcile = true
	c.RetryKnownUnaccepted = true
	b, err := Run(c)
	if !errors.Is(err, ErrUncertain) || b.ResponseAvailable || b.OperationState != "sent" || posts.Load() != 1 {
		t.Fatal("one-time login was recreated", b, err)
	}
}

func TestConcurrentOriginalOperationHasOneDispatch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		close(entered)
		<-release
		w.Write([]byte(`{"action":{"state":"completed"}}`))
	})
	done := make(chan error, 1)
	go func() { _, e := Run(c); done <- e }()
	<-entered
	if _, err := Run(c); err == nil || posts.Load() != 1 {
		t.Error("concurrent caller dispatched", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConsumedCompletionInputRecoversWithoutCode(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"operation":{"state":"sent"},"login":{"success":true}}`))
	})
	c.Action = "login-complete"
	c.ConsumeRequest = true
	os.WriteFile(c.Request, []byte(`{"operation_id":"original-stop","expected_incarnation":"inc-original","login_id":"fixture","code":"PRIVATE-CODE"}`), 0600)
	if _, e := Run(c); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(c.Request); !os.IsNotExist(e) {
		t.Fatal("code retained")
	}
	c.Request = ""
	if v, e := Run(c); e != nil || !v.Cached {
		t.Fatal(v, e)
	}
}

func TestRestartChild(t *testing.T) {
	path := os.Getenv("POMAR_OWNER_TEST_CONFIG")
	if path == "" {
		return
	}
	b, e := os.ReadFile(path)
	if e != nil {
		os.Exit(4)
	}
	var c Config
	if json.Unmarshal(b, &c) != nil {
		os.Exit(4)
	}
	_, e = Run(c)
	if e != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
func TestRealProcessRestartKeepsOriginalIntent(t *testing.T) {
	var posts atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Write([]byte(`{"action":{"state":"completed"}}`))
	})
	config := filepath.Join(c.Root, "child.json")
	invoke := func(want int) {
		b, _ := json.Marshal(c)
		os.WriteFile(config, b, 0600)
		cmd := exec.Command(os.Args[0], "-test.run=^TestRestartChild$")
		cmd.Env = append(os.Environ(), "POMAR_OWNER_TEST_CONFIG="+config)
		out, e := cmd.CombinedOutput()
		code := 0
		if e != nil {
			if ex, ok := e.(*exec.ExitError); ok {
				code = ex.ExitCode()
			} else {
				t.Fatal(e)
			}
		}
		if code != want {
			t.Fatalf("child exit%d want%d: %s", code, want, out)
		}
	}
	invoke(3)
	invoke(3)
	c.Reconcile = true
	invoke(0)
	if posts.Load() != 1 {
		t.Fatal("restart repeated dispatch")
	}
	if strings.Contains(c.OperationID, ".") {
		t.Fatal("fixture unexpectedly altered")
	}
}
