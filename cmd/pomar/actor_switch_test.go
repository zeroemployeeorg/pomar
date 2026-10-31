package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type switchTransport func(*http.Request) (*http.Response, error)

func (f switchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestActorSwitchClientBindsExactRequest(t *testing.T) {
	for _, status := range []bool{false, true} {
		var out, errout bytes.Buffer
		c := &http.Client{Transport: switchTransport(func(r *http.Request) (*http.Response, error) {
			action := `{"kind":"replace","expected_incarnation":"old","state":"completed","harness_switch":{"session_id":"session","target_profile":"codex-seat","phase":"selected"}}`
			reply := `{"actions":{"op":` + action + `}}`
			if status {
				if r.Method != "GET" || r.URL.Path != "/v1/environments/env/operations/op" || r.URL.Query().Get("session_id") != "session" || r.URL.Query().Get("expected_incarnation") != "old" {
					t.Fatal("bad inspection", r.URL)
				}
				reply = `{"action":` + action + `}`
			} else {
				var body actorSwitchRequest
				if r.Method != "POST" || r.URL.Path != "/v1/environments/env/replace" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Profile != "codex-seat" || body.Session != "session" || body.Incarnation != "old" || body.Operation != "op" {
					t.Fatal("request not bound")
				}
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply))}, nil
		})}
		if rc := actorSwitchCall(c, "env", actorSwitchRequest{"op", "session", "old", "codex-seat"}, status, &out, &errout); rc != 0 {
			t.Fatal(rc, errout.String())
		}
	}
}
func TestActorSwitchClientNeverTreatsUncertainAsSuccess(t *testing.T) {
	for _, reply := range []string{"transport", `{"actions":{"op":{"state":"acceptance_unknown"}}}`, `{}`, `{broken`} {
		calls := 0
		var out, errout bytes.Buffer
		c := &http.Client{Transport: switchTransport(func(*http.Request) (*http.Response, error) {
			calls++
			if reply == "transport" {
				return nil, errors.New("reply lost")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply))}, nil
		})}
		if rc := actorSwitchCall(c, "env", actorSwitchRequest{"op", "session", "old", "codex-seat"}, false, &out, &errout); rc != 1 || calls != 1 {
			t.Fatalf("uncertainty retried or accepted: %d %d", rc, calls)
		}
	}
}
func TestActorSwitchCLIRequiresOriginalIdentity(t *testing.T) {
	var out, errout bytes.Buffer
	if rc := run([]string{"actor-switch", "-root", "/no-service", "-environment", "env", "-profile", "codex-seat"}, &out, &errout); rc != 2 {
		t.Fatal(rc)
	}
}
