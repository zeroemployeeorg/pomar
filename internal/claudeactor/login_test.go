package claudeactor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

const goodCode = "synthetic-auth-code-0123456789"

// fakeLogin behaves like `claude auth login --claudeai` with no terminal, as
// probed on 2.1.280: it prints the authorization URL, waits for a code on
// stdin, and on the right code writes the credential file and exits 0.
type fakeLogin struct {
	inR, outR *io.PipeReader
	inW, outW *io.PipeWriter
	exit      chan error
	cred      string
}

func newFakeLogin(cred string) *fakeLogin {
	f := &fakeLogin{exit: make(chan error, 1), cred: cred}
	f.inR, f.inW = io.Pipe()
	f.outR, f.outW = io.Pipe()
	go func() {
		defer f.outW.Close()
		fmt.Fprintln(f.outW, "Opening browser to sign in…")
		fmt.Fprintln(f.outW, "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=c&state=s")
		fmt.Fprint(f.outW, "Paste code here if prompted > ")
		code, err := bufio.NewReader(f.inR).ReadString('\n')
		if err != nil || strings.TrimSpace(code) != goodCode {
			f.exit <- errors.New("exit status 1")
			return
		}
		os.WriteFile(f.cred, []byte("synthetic-credential"), 0o600)
		f.exit <- nil
	}()
	return f
}

func (f *fakeLogin) Stdin() io.WriteCloser { return f.inW }
func (f *fakeLogin) Stdout() io.Reader     { return f.outR }
func (f *fakeLogin) Interrupt() error      { f.inW.CloseWithError(errors.New("interrupted")); return nil }
func (f *fakeLogin) Wait() error           { return <-f.exit }

type loginHarness struct {
	store  *agentenv.Store
	broker *agentenv.Broker
	srv    *httptest.Server
	config string
	starts int
}

func openLogin(t *testing.T) *loginHarness {
	t.Helper()
	store, err := agentenv.Open(filepath.Join(t.TempDir(), "state"), "env-1", "session-1", "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &loginHarness{store: store, broker: agentenv.NewBroker(store, "/work"), config: t.TempDir()}
	actor := New(Config{
		Version:       "2.1.280",
		Start:         func([]string) (Process, error) { return nil, errors.New("no turns in these tests") },
		Authenticated: CredentialPresent(h.config),
		Login: func() (Process, error) {
			h.starts++
			return newFakeLogin(filepath.Join(h.config, ".credentials.json")), nil
		},
	}, h.broker.Observe)
	if err := h.broker.Initialize(t.Context(), actor); err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(h.broker.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *loginHarness) post(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(h.srv.URL+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func journal(h *loginHarness) string {
	b, _ := json.Marshal(h.store.Snapshot())
	return string(b)
}

// Start returns the URL; completing with the code signs in; neither the URL
// nor the code is journaled or echoed; a new sign-in can start afterwards.
func TestSignInThroughTheBroker(t *testing.T) {
	h := openLogin(t)
	code, out := h.post(t, "/v1/login", `{"expected_incarnation":"inc-1","operation_id":"login-1"}`)
	if code != 200 {
		t.Fatalf("login: HTTP %d %v", code, out)
	}
	login := out["login"].(map[string]any)
	if login["type"] != "claudeAiOAuth" || !strings.HasPrefix(login["authUrl"].(string), "https://claude.com/cai/oauth/authorize?") {
		t.Fatalf("login result %v", login)
	}
	if strings.Contains(journal(h), "oauth/authorize") {
		t.Fatal("the authorization URL was journaled")
	}
	body := fmt.Sprintf(`{"expected_incarnation":"inc-1","operation_id":"login-1-code","login_id":%q,"code":%q}`, login["loginId"], goodCode)
	code, out = h.post(t, "/v1/login/complete", body)
	if code != 200 || out["login"].(map[string]any)["success"] != true {
		t.Fatalf("complete: HTTP %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(journal(h), goodCode) || strings.Contains(string(raw), goodCode) {
		t.Fatal("the authorization code was journaled or echoed")
	}
	if !CredentialPresent(h.config)() {
		t.Fatal("no credential after a successful sign-in")
	}
	// Renewal: the same command again.
	if code, out = h.post(t, "/v1/login", `{"expected_incarnation":"inc-1","operation_id":"login-2"}`); code != 200 {
		t.Fatalf("a second sign-in after completion: HTTP %d %v", code, out)
	}
	if h.starts != 2 {
		t.Fatalf("%d sign-in processes, want 2", h.starts)
	}
}

// A malformed code never reaches the process and fails the sign-in.
func TestSignInRefusesAMalformedCode(t *testing.T) {
	h := openLogin(t)
	_, out := h.post(t, "/v1/login", `{"expected_incarnation":"inc-1","operation_id":"login-1"}`)
	id := out["login"].(map[string]any)["loginId"]
	body := fmt.Sprintf(`{"expected_incarnation":"inc-1","operation_id":"login-1-code","login_id":%q,"code":"bad code; rm -rf /"}`, id)
	code, out := h.post(t, "/v1/login/complete", body)
	if code != 200 || out["login"].(map[string]any)["success"] != false {
		t.Fatalf("a malformed code: HTTP %d %v", code, out)
	}
	if CredentialPresent(h.config)() {
		t.Fatal("a credential appeared after a malformed code")
	}
}

// One sign-in waits at a time; a wrong login ID completes nothing.
func TestOneSignInAtATime(t *testing.T) {
	h := openLogin(t)
	if code, _ := h.post(t, "/v1/login", `{"expected_incarnation":"inc-1","operation_id":"login-1"}`); code != 200 {
		t.Fatal("first sign-in refused")
	}
	if code, _ := h.post(t, "/v1/login", `{"expected_incarnation":"inc-1","operation_id":"login-2"}`); code == 200 {
		t.Fatal("a second sign-in started while one waited")
	}
	code, _ := h.post(t, "/v1/login/complete", fmt.Sprintf(`{"expected_incarnation":"inc-1","operation_id":"c-1","login_id":"not-it","code":%q}`, goodCode))
	if code == 200 {
		t.Fatal("a wrong login id completed a sign-in")
	}
	if code, _ := h.post(t, "/v1/login/complete", `{"expected_incarnation":"inc-0","operation_id":"c-2","login_id":"x","code":"y"}`); code != 403 {
		t.Fatalf("a stale incarnation: HTTP %d, want 403", code)
	}
}
