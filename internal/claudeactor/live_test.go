package claudeactor

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
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
