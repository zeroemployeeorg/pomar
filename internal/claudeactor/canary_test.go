package claudeactor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// The adviser's §3.5 item 1 (r40 rev 2), proposal P3: a canary credential in
// the environment never leaves through Pomar's own outputs.

const canary = "CANARY-claude-credential-7f3a91"

type canaryEnv struct {
	h      *harness
	work   string
	config string
	state  string
	srv    *httptest.Server
}

func setupCanary(t *testing.T) *canaryEnv {
	t.Helper()
	work := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", work}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(work, "tracked.go"), []byte("package x\n"), 0o644)
	exec.Command("git", "-C", work, "add", "tracked.go").Run()
	exec.Command("git", "-C", work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "tracked").Run()

	config := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	// The credential the sign-in leaves, holding the canary.
	script := func(_ *fakeClaude, session string, _ int, _ string) []string {
		return []string{
			line(map[string]any{"type": "system", "subtype": "init", "session_id": session}),
			line(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_c1", "name": "Bash", "input": map[string]string{"command": "go test ./..."}}}}}),
			"WAIT-INTERRUPT",
			line(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"role": "assistant", "content": []any{map[string]string{"type": "text", "text": "tests pass"}}}}),
			line(map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false}),
		}
	}
	store, err := agentenv.Open(state, "env-1", "session-1", "inc-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &harness{t: t, store: store, broker: agentenv.NewBroker(store, work)}
	h.actor = New(Config{
		Version:       "2.1.280",
		Args:          []string{"--setting-sources", "user", "--strict-mcp-config"},
		Authenticated: CredentialPresent(config),
		Start: func(args []string) (Process, error) {
			f := newFake(args, script)
			h.mu.Lock()
			h.fakes = append(h.fakes, f)
			h.mu.Unlock()
			return f, nil
		},
		Login: func() (Process, error) {
			f := newFakeLogin(filepath.Join(config, ".credentials.json"))
			return &canaryLogin{f}, nil
		},
	}, h.broker.Observe)
	if err := h.broker.Initialize(t.Context(), h.actor); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.broker.Handler())
	t.Cleanup(srv.Close)
	return &canaryEnv{h: h, work: work, config: config, state: state, srv: srv}
}

// canaryLogin is the fake sign-in, writing the canary as the credential.
type canaryLogin struct{ *fakeLogin }

func (c *canaryLogin) Wait() error {
	err := c.fakeLogin.Wait()
	if err == nil {
		os.WriteFile(c.cred, []byte(`{"claudeAiOauth":{"accessToken":"`+canary+`"}}`), 0o600)
	}
	return err
}

func (c *canaryEnv) call(t *testing.T, method, path, body string) string {
	t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// noCanary fails if the canary is in any output, or any file of the journal.
func (c *canaryEnv) noCanary(t *testing.T, outputs map[string]string) {
	t.Helper()
	for name, out := range outputs {
		if strings.Contains(out, canary) {
			t.Errorf("the canary left through %s", name)
		}
	}
	filepath.WalkDir(c.state, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(canary)) {
				t.Errorf("the canary is in the journal file %s", p)
			}
		}
		return nil
	})
}

// A full flow (sign-in, a task, a permission request, a change in the
// workspace) never carries the credential out through Pomar.
func TestCanaryCredentialNeverLeavesThroughPomar(t *testing.T) {
	c := setupCanary(t)
	outputs := map[string]string{}
	outputs["login"] = c.call(t, "POST", "/v1/login", `{"expected_incarnation":"inc-1","operation_id":"login-1"}`)
	var started struct {
		Login struct {
			LoginID string `json:"loginId"`
		} `json:"login"`
	}
	json.Unmarshal([]byte(outputs["login"]), &started)
	outputs["login/complete"] = c.call(t, "POST", "/v1/login/complete", fmt.Sprintf(`{"expected_incarnation":"inc-1","operation_id":"login-1-code","login_id":%q,"code":%q}`, started.Login.LoginID, goodCode))
	if !CredentialPresent(c.config)() {
		t.Fatal("the sign-in left no credential")
	}
	b, _ := os.ReadFile(filepath.Join(c.config, ".credentials.json"))
	if !bytes.Contains(b, []byte(canary)) {
		t.Fatal("the canary credential was not placed")
	}

	sock := bridge(t, c.h.actor)
	outputs["task"] = c.call(t, "POST", "/v1/tasks", `{"operation_id":"op-1","expected_incarnation":"inc-1","text":"run the tests"}`)
	c.h.waitOp("op-1", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	ch := forwardAsync(sock, PermissionTool, `{"tool_name":"Bash","input":{"command":"go test ./..."},"tool_use_id":"toolu_c1"}`)
	e := permissionEvent(t, c.h.store)
	decide(t, c.h.broker, e.PermissionID, "accept", "perm-1")
	outputs["permission reply"] = (<-ch).reply.Text
	// The agent's change: a tracked edit and a new file.
	os.WriteFile(filepath.Join(c.work, "tracked.go"), []byte("package x\n\nfunc F() {}\n"), 0o644)
	os.WriteFile(filepath.Join(c.work, "new.go"), []byte("package x\n"), 0o644)
	c.h.fakes[0].Interrupt()
	c.h.waitOp("op-1", finished)

	outputs["session"] = c.call(t, "GET", "/v1/session", "")
	outputs["events"] = c.call(t, "GET", "/v1/events?after=0", "")
	outputs["operation"] = c.call(t, "GET", "/v1/operations/op-1?session_id=session-1&expected_incarnation=inc-1", "")
	outputs["result"] = c.call(t, "GET", "/v1/result", "")
	if !strings.Contains(outputs["result"], "func F()") || !strings.Contains(outputs["result"], "new.go") {
		t.Fatalf("the result does not carry the workspace change: %.300s", outputs["result"])
	}
	c.noCanary(t, outputs)
}

// The known limit, stated rather than hidden: the agent can read its own
// credential (it lives in its environment), so a credential it copies into
// /work is exported with the workspace. Pomar's containment for that case is
// the egress allowlist, not the export.
func TestCanaryCopiedIntoTheWorkspaceIsExported(t *testing.T) {
	c := setupCanary(t)
	os.WriteFile(filepath.Join(c.work, "leaked.txt"), []byte(canary), 0o644)
	if out := c.call(t, "GET", "/v1/result", ""); !strings.Contains(out, canary) {
		t.Fatal("expected the export to carry a file the agent wrote; if this changes, record the new containment")
	}
}
