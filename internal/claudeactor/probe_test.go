package claudeactor

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Probe for the bridge binding (adviser r41 §3.2): what Claude Code sends to
// the permission tool, and whether the matching tool_use appears on stdout
// before it. Records the order; answers allow.
func TestLiveProbePermissionArguments(t *testing.T) {
	binary := liveBinary(t)
	work := t.TempDir()
	sockDir, _ := os.MkdirTemp("/tmp", "pccp")
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "b.sock")
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, time.Now().Format("15:04:05.000")+" "+s); mu.Unlock() }
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
			note("BRIDGE " + string(req.Arguments))
			var a struct {
				Input json.RawMessage `json:"input"`
			}
			json.Unmarshal(req.Arguments, &a)
			b, _ := json.Marshal(bridgeReply{Text: allowText(a.Input)})
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{"type": "stdio", "command": os.Args[0], "env": map[string]string{"POMAR_CC_MCP_SOCKET": sock}}}})
	lp := startLive(t, binary, work, "--session-id", newUUID(), "--mcp-config", string(mcp), "--permission-prompt-tool", PermissionToolName, "--tools", "Bash")
	go func() {
		for m := range lp.lines {
			if m["type"] == "assistant" {
				b, _ := json.Marshal(m["message"])
				if strings.Contains(string(b), `"tool_use"`) {
					note("STDOUT assistant tool_use " + string(b))
				}
			}
			if m["type"] == "result" {
				note("STDOUT result")
			}
		}
	}()
	lp.send("Use the Bash tool to run exactly: touch probe.txt\nThen reply: done")
	time.Sleep(30 * time.Second)
	lp.p.Stdin().Close()
	lp.p.Wait()
	mu.Lock()
	defer mu.Unlock()
	assertToolUseFirst(t, order)
	for _, o := range order {
		t.Log(o)
	}
}

// The binding's premise, asserted on the probe's record: the tool_use line
// precedes the bridge request, which names its id.
func assertToolUseFirst(t *testing.T, order []string) {
	t.Helper()
	use, bridge := -1, -1
	var id string
	for i, o := range order {
		if use < 0 && strings.Contains(o, "STDOUT assistant tool_use") {
			use = i
			if j := strings.Index(o, `"id":"toolu_`); j >= 0 {
				id = o[j+6 : j+6+strings.Index(o[j+6:], `"`)]
			}
		}
		if bridge < 0 && strings.Contains(o, "BRIDGE ") {
			bridge = i
		}
	}
	if use < 0 || bridge < 0 || use > bridge || id == "" || !strings.Contains(order[bridge], `"tool_use_id":"`+id+`"`) {
		t.Fatalf("the tool_use line did not precede a bridge request naming it (use %d, bridge %d, id %q)", use, bridge, id)
	}
}
