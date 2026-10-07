package claudeactor

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Qualification for adviser note r42 §6, on the guest's own launch arguments
// (--restricted, GuestTools, --settings BashRules), to re-run on every pin
// change: a read Pomar allows runs unasked, a network tool Pomar refuses is
// refused without asking, and a remote git operation and an unlisted write
// reach the permission tool. An unlisted read (echo) follows Claude Code's own
// default and is recorded, not asserted. The bridge allows what it is asked,
// so later commands are reached; the sign-in must still work.
func TestLiveBashRules(t *testing.T) {
	binary := liveBinary(t)
	work := t.TempDir()
	if out, err := exec.Command("git", "-C", work, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	sockDir, _ := os.MkdirTemp("/tmp", "pccb")
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "b.sock")
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req bridgeRequest
			json.Unmarshal(line, &req)
			var a struct {
				Input json.RawMessage `json:"input"`
			}
			json.Unmarshal(req.Arguments, &a)
			note("ASKED " + string(a.Input))
			b, _ := json.Marshal(bridgeReply{Text: allowText(a.Input)})
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{"type": "stdio", "command": os.Args[0], "env": map[string]string{"POMAR_CC_MCP_SOCKET": sock}}}})
	full := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--restricted", "--tools", GuestTools, "--strict-mcp-config", "--model", "haiku",
		"--settings", BashRules, "--mcp-config", string(mcp), "--permission-prompt-tool", PermissionToolName, "--session-id", newUUID()}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "DISABLE_AUTOUPDATER=1"}
	p, err := Exec{Binary: binary, Dir: work, Env: env, UID: -1, GID: -1}.Start(full)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan map[string]any, 1)
	go func() {
		s := bufio.NewScanner(p.Stdout())
		s.Buffer(make([]byte, 4096), 2<<20)
		for s.Scan() {
			var m map[string]any
			if json.Unmarshal(s.Bytes(), &m) != nil {
				continue
			}
			b, _ := json.Marshal(m["message"])
			switch m["type"] {
			case "assistant":
				var msg struct {
					Content []struct {
						Type  string          `json:"type"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
						Text  string          `json:"text"`
					} `json:"content"`
				}
				json.Unmarshal(b, &msg)
				for _, c := range msg.Content {
					if c.Type == "tool_use" {
						note("TOOL_USE " + c.Name + " " + string(c.Input))
					}
				}
				if e, _ := m["error"].(string); e != "" {
					note("API_ERROR " + e)
				}
			case "user":
				var msg struct {
					Content []struct {
						Type    string `json:"type"`
						IsError bool   `json:"is_error"`
						Content any    `json:"content"`
					} `json:"content"`
				}
				json.Unmarshal(b, &msg)
				for _, c := range msg.Content {
					if c.Type == "tool_result" {
						r, _ := json.Marshal(c.Content)
						s := string(r)
						if len(s) > 160 {
							s = s[:160]
						}
						note("RESULT is_error=" + map[bool]string{true: "true", false: "false"}[c.IsError] + " " + s)
					}
				}
			case "result":
				done <- m
				return
			}
		}
		close(done)
	}()
	msg := `Run each of these shell commands with the Bash tool, exactly as written, one tool call per command, in this order, continuing even if one fails or is refused: ` +
		`(1) ls (2) git status (3) echo hi (4) curl -sI https://example.com (5) git fetch origin (6) touch probe.txt. Then reply: done.`
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": msg}}}})
	p.Stdin().Write(append(line, '\n'))
	var result map[string]any
	select {
	case result = <-done:
	case <-time.After(4 * time.Minute):
		t.Fatal("no result")
	}
	p.Stdin().Close()
	p.Wait()
	mu.Lock()
	defer mu.Unlock()
	for _, o := range order {
		t.Log(o)
	}
	t.Logf("result: is_error=%v subtype=%v", result["is_error"], result["subtype"])
	if strings.Contains(strings.Join(order, "\n"), "API_ERROR") {
		t.Fatal("the API call failed under --restricted (sign-in not read?)")
	}
	// For each command: whether it reached the permission tool, and its result.
	asked := map[string]bool{}
	results := map[string]string{}
	var last string
	for _, o := range order {
		switch {
		case strings.HasPrefix(o, "TOOL_USE Bash "):
			var in struct {
				Command string `json:"command"`
			}
			json.Unmarshal([]byte(strings.TrimPrefix(o, "TOOL_USE Bash ")), &in)
			last = in.Command
		case strings.HasPrefix(o, "ASKED "):
			asked[last] = true
		case strings.HasPrefix(o, "RESULT "):
			results[last] = o
		}
	}
	for _, c := range []string{"ls", "git status"} {
		if _, ran := results[c]; !ran || asked[c] || strings.Contains(results[c], "is_error=true") {
			t.Errorf("%s: an allowed read should run unasked (asked %v, %q)", c, asked[c], results[c])
		}
	}
	if c := "curl -sI https://example.com"; asked[c] || !strings.Contains(results[c], "denied") {
		t.Errorf("%s: a refused network tool reached the controller or ran (asked %v, %q)", c, asked[c], results[c])
	}
	for _, c := range []string{"git fetch origin", "touch probe.txt"} {
		if !asked[c] {
			t.Errorf("%s: did not reach the permission tool", c)
		}
	}
	t.Logf("Claude Code's own default for an unlisted read (echo hi): asked=%v", asked["echo hi"])
}
