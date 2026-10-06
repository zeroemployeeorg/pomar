package claudeactor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// The live qualification runs only when POMAR_LIVE_CLAUDE names an installed,
// already signed-in Claude Code binary. It reads and copies no credential: the
// binary uses its own sign-in. It runs on whatever platform executes it; a
// host run does not qualify the Linux guest. Run:
//
//	POMAR_LIVE_CLAUDE=$(command -v claude) go test -count=1 -v -run '^TestLive' ./internal/claudeactor
func liveBinary(t *testing.T) string {
	b := os.Getenv("POMAR_LIVE_CLAUDE")
	if b == "" {
		t.Skip("POMAR_LIVE_CLAUDE not set")
	}
	return b
}

type liveProc struct {
	t     *testing.T
	p     Process
	lines chan map[string]any
}

func startLive(t *testing.T, binary, dir string, args ...string) *liveProc {
	t.Helper()
	full := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--setting-sources", "user", "--strict-mcp-config", "--model", "haiku", "--tools", ""}, args...)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "DISABLE_AUTOUPDATER=1"}
	p, err := Exec{Binary: binary, Dir: dir, Env: env, UID: -1, GID: -1}.Start(full)
	if err != nil {
		t.Fatal(err)
	}
	lp := &liveProc{t: t, p: p, lines: make(chan map[string]any, 256)}
	go func() {
		s := bufio.NewScanner(p.Stdout())
		s.Buffer(make([]byte, 4096), 2<<20)
		for s.Scan() {
			var m map[string]any
			if json.Unmarshal(s.Bytes(), &m) == nil {
				lp.lines <- m
			}
		}
		close(lp.lines)
	}()
	return lp
}

func (lp *liveProc) send(text string) {
	lp.t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}}})
	if _, err := lp.p.Stdin().Write(append(b, '\n')); err != nil {
		lp.t.Fatal(err)
	}
}

// until reads lines until a result (returned) or the stream ends (nil).
func (lp *liveProc) untilResult(timeout time.Duration) (map[string]any, []string) {
	var seen []string
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-lp.lines:
			if !ok {
				return nil, seen
			}
			kind, _ := m["type"].(string)
			sub, _ := m["subtype"].(string)
			seen = append(seen, strings.TrimSuffix(kind+"/"+sub, "/"))
			if kind == "result" {
				return m, seen
			}
		case <-deadline:
			lp.t.Fatalf("no result within %s; saw %v", timeout, seen)
		}
	}
}

// One process takes two turns over stdin, each ending in a result for the
// session named by --session-id; a new process resumes the session and
// remembers the first turn.
func TestLiveTwoTurnsThenResume(t *testing.T) {
	binary := liveBinary(t)
	dir := t.TempDir()
	session := newUUID()
	lp := startLive(t, binary, dir, "--session-id", session)
	lp.send("Remember the word PERSIMMON. Reply with exactly: ok")
	r1, seen1 := lp.untilResult(2 * time.Minute)
	t.Logf("turn 1 stream: %v", seen1)
	if r1 == nil || r1["session_id"] != session || r1["is_error"] == true {
		t.Fatalf("turn 1 result: %v", r1)
	}
	lp.send("Reply with exactly: two")
	r2, seen2 := lp.untilResult(2 * time.Minute)
	t.Logf("turn 2 stream: %v", seen2)
	if r2 == nil || r2["session_id"] != session {
		t.Fatalf("turn 2 result: %v", r2)
	}
	lp.p.Stdin().Close()
	lp.p.Wait()

	again := startLive(t, binary, dir, "--resume", session)
	again.send("What word did I ask you to remember? Reply with that word only.")
	r3, seen3 := again.untilResult(2 * time.Minute)
	t.Logf("resumed stream: %v", seen3)
	text, _ := r3["result"].(string)
	if r3 == nil || r3["session_id"] != session || !strings.Contains(strings.ToUpper(text), "PERSIMMON") {
		t.Fatalf("resumed result: session %v, text %q", r3["session_id"], text)
	}
	again.p.Stdin().Close()
	again.p.Wait()
}

// What SIGINT to the process group does during a turn, recorded rather than
// assumed: whether a result arrives, and whether the process survives.
func TestLiveInterruptBehaviour(t *testing.T) {
	binary := liveBinary(t)
	lp := startLive(t, binary, t.TempDir(), "--session-id", newUUID())
	lp.send("Write the numbers from 1 to 400, one per line, with no other text.")
	// Wait for the turn to begin producing output.
	select {
	case m := <-lp.lines:
		t.Logf("first line: %v/%v", m["type"], m["subtype"])
	case <-time.After(time.Minute):
		t.Fatal("the turn never started")
	}
	if err := lp.p.Interrupt(); err != nil {
		t.Fatal(err)
	}
	r, seen := lp.untilResult(time.Minute)
	t.Logf("after SIGINT: stream %v; result %v", seen, r != nil)
	if r != nil {
		t.Logf("result subtype %v is_error %v", r["subtype"], r["is_error"])
	}
	done := make(chan error, 1)
	go func() { done <- lp.p.Wait() }()
	select {
	case err := <-done:
		code := -1
		if ee, ok := err.(interface{ Sys() any }); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				code = ws.ExitStatus()
			}
		}
		t.Logf("the process exited after SIGINT (err %v, status %d)", err, code)
	case <-time.After(5 * time.Second):
		t.Log("the process survived SIGINT; closing its input")
		lp.p.Stdin().Close()
		<-done
	}
}

// A real Claude Code asks permission through Pomar's MCP bridge; the shared
// broker journals it; the controller's accept lets the command run, and its
// decline stops it.
func TestLivePermissionBridge(t *testing.T) {
	binary := liveBinary(t)
	for _, decision := range []string{"accept", "decline"} {
		t.Run(decision, func(t *testing.T) {
			work := t.TempDir()
			store, err := agentenv.Open(filepath.Join(t.TempDir(), "state"), "env-1", "session-1", "inc-1")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			broker := agentenv.NewBroker(store, work)
			sockDir, _ := os.MkdirTemp("/tmp", "pccl")
			defer os.RemoveAll(sockDir)
			sock := filepath.Join(sockDir, "b.sock")
			mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{
				"type": "stdio", "command": os.Args[0], "env": map[string]string{"POMAR_CC_MCP_SOCKET": sock},
			}}})
			env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "DISABLE_AUTOUPDATER=1"}
			actor := New(Config{
				Version:       "2.1.280",
				Args:          []string{"--setting-sources", "user", "--strict-mcp-config", "--mcp-config", string(mcp), "--permission-prompt-tool", PermissionToolName, "--model", "haiku", "--tools", "Bash"},
				Start:         Exec{Binary: binary, Dir: work, Env: env, UID: -1, GID: -1}.Start,
				Authenticated: func() bool { return true },
			}, broker.Observe)
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go actor.ServeBridge(ln)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := broker.Initialize(ctx, actor); err != nil {
				t.Fatal(err)
			}
			if _, err := broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "Use the Bash tool to run exactly this command: touch pomar-bridge-ok.txt\nThen reply with the single word: done"}); err != nil {
				t.Fatal(err)
			}
			var perm agentenv.Event
			deadline := time.Now().Add(2 * time.Minute)
			for perm.PermissionID == "" && time.Now().Before(deadline) {
				for _, e := range store.Snapshot().Events {
					if strings.HasSuffix(e.Method, "/requestApproval") {
						perm = e
					}
				}
				if op := store.Snapshot().Operations["op-1"]; op.Completion != "" {
					t.Fatalf("the turn ended (%s) without asking permission", op.Completion)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if perm.PermissionID == "" {
				t.Fatal("no permission request within 2 minutes")
			}
			t.Logf("permission request %s: %s", perm.Method, string(perm.Params))
			srv := httptest.NewServer(broker.Handler())
			defer srv.Close()
			resp, err := http.Post(srv.URL+"/v1/permissions/"+perm.PermissionID, "application/json", strings.NewReader(`{"expected_incarnation":"inc-1","decision":"`+decision+`","operation_id":"perm-1"}`))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("decision: HTTP %d", resp.StatusCode)
			}
			for time.Now().Before(deadline) {
				if op := store.Snapshot().Operations["op-1"]; op.Completion != "" {
					_, statErr := os.Stat(filepath.Join(work, "pomar-bridge-ok.txt"))
					t.Logf("completion %s; file exists: %v", op.Completion, statErr == nil)
					if decision == "accept" && statErr != nil {
						t.Fatal("accepted, but the command did not run")
					}
					if decision == "decline" && statErr == nil {
						t.Fatal("declined, but the command ran")
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("the turn did not complete after the decision")
		})
	}
}

// The real `claude auth login --claudeai`, with no terminal and an empty,
// throwaway config directory, yields an authorization URL through the actor.
// The sign-in is then abandoned: no code is sent and no credential is made.
func TestLiveSignInStartsWithAURL(t *testing.T) {
	binary := liveBinary(t)
	home, _ := os.MkdirTemp("/tmp", "pccsi")
	defer os.RemoveAll(home)
	config := filepath.Join(home, "cfg")
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "CLAUDE_CONFIG_DIR=" + config, "BROWSER=/usr/bin/false", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	var proc Process
	actor := New(Config{
		Version:       "2.1.280",
		Start:         func([]string) (Process, error) { t.Fatal("no turn in this test"); return nil, nil },
		Authenticated: CredentialPresent(config),
		Login: func() (Process, error) {
			p, err := Exec{Binary: binary, Dir: home, Env: env, UID: -1, GID: -1}.Start([]string{"auth", "login", "--claudeai"})
			proc = p
			return p, err
		},
	}, func(agentenv.Message) error { return nil })
	raw, err := actor.Call(context.Background(), "account/login/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Type    string `json:"type"`
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
	}
	json.Unmarshal(raw, &out)
	if out.Type != "claudeAiOAuth" || out.LoginID == "" || !strings.HasPrefix(out.AuthURL, "https://") || !strings.Contains(out.AuthURL, "/oauth/authorize?") {
		t.Fatalf("login start: %s", raw)
	}
	host := out.AuthURL[len("https://"):]
	host = host[:strings.Index(host, "/")]
	t.Logf("authorization URL host %s, %d bytes (not logged in full)", host, len(out.AuthURL))
	proc.Interrupt()
	proc.Wait()
	if CredentialPresent(config)() {
		t.Fatal("a credential exists after an abandoned sign-in")
	}
}

// With the guest's flags (--setting-sources user --strict-mcp-config), does a
// task repository's CLAUDE.md still load, and does its .mcp.json? Recorded
// for the adviser's note r41 §3.1; .mcp.json must not load.
func TestLiveRepositoryContextFiles(t *testing.T) {
	binary := liveBinary(t)
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "CLAUDE.md"), []byte("Always end every reply with the single word KIWI.\n"), 0o644)
	os.WriteFile(filepath.Join(work, ".mcp.json"), []byte(`{"mcpServers":{"repo-server":{"type":"stdio","command":"/usr/bin/true"}}}`), 0o644)
	lp := startLive(t, binary, work, "--session-id", newUUID())
	lp.send("Reply with the word: hello")
	var servers []string
	var text string
	deadline := time.After(2 * time.Minute)
	for done := false; !done; {
		select {
		case m, ok := <-lp.lines:
			if !ok {
				done = true
				break
			}
			if m["type"] == "system" && m["subtype"] == "init" {
				if s, ok := m["mcp_servers"].([]any); ok {
					for _, v := range s {
						if mm, ok := v.(map[string]any); ok {
							servers = append(servers, fmt.Sprint(mm["name"]))
						}
					}
				}
			}
			if m["type"] == "result" {
				text, _ = m["result"].(string)
				done = true
			}
		case <-deadline:
			t.Fatal("no result")
		}
	}
	lp.p.Stdin().Close()
	lp.p.Wait()
	t.Logf("MCP servers at init: %v", servers)
	t.Logf("reply %q; CLAUDE.md instruction followed: %v", text, strings.Contains(strings.ToUpper(text), "KIWI"))
	for _, s := range servers {
		if s == "repo-server" {
			t.Fatal("the repository's .mcp.json loaded despite --strict-mcp-config")
		}
	}
}
