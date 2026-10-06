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

// The controller request tool, qualified against a live Claude Code
// (POMAR_LIVE_CLAUDE; see liveBinary). What it establishes, on the pinned
// version, for the binding and the launch the guest uses:
//   - the assistant tool_use line for mcp__pomar__pomar_controller_request,
//     with the call's exact input, precedes the MCP tools/call;
//   - --allowedTools keeps the call from the permission tool;
//   - a reply that comes minutes later still reaches the model
//     (POMAR_LIVE_CONTROLLER_DELAY, default 150s);
//   - an error reply reaches it as an error tool result.
func TestLiveControllerTool(t *testing.T) {
	binary := liveBinary(t)
	delay := 150 * time.Second
	if d, err := time.ParseDuration(os.Getenv("POMAR_LIVE_CONTROLLER_DELAY")); err == nil {
		delay = d
	}
	work := t.TempDir()
	sockDir, _ := os.MkdirTemp("/tmp", "pccc")
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "b.sock")
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		order = append(order, time.Now().Format("15:04:05.000")+" "+s)
		mu.Unlock()
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	calls := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var req bridgeRequest
			json.Unmarshal(line, &req)
			note("BRIDGE " + req.Tool + " " + string(req.Arguments))
			reply := bridgeReply{Text: denyText("not expected in this test")}
			if req.Tool == ControllerTool {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				if n == 1 {
					time.Sleep(delay)
					reply = bridgeReply{Text: "post 41 is waiting. Its code word is ORCHARD-41."}
				} else {
					reply = bridgeReply{Text: "post 99 is not the current command.", IsError: true}
				}
			}
			b, _ := json.Marshal(reply)
			c.Write(append(b, '\n'))
			c.Close()
		}
	}()
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{"type": "stdio", "command": os.Args[0], "env": map[string]string{"POMAR_CC_MCP_SOCKET": sock, "POMAR_CC_MCP_CONTROLLER": "inbox,ack"}}}})
	lp := startLive(t, binary, work, "--session-id", newUUID(), "--mcp-config", string(mcp), "--permission-prompt-tool", PermissionToolName, "--allowedTools", ControllerToolName)
	var texts []string
	var toolErrors []bool
	var resultsMu sync.Mutex
	results := make(chan map[string]any, 4)
	go func() {
		for m := range lp.lines {
			b, _ := json.Marshal(m["message"])
			switch m["type"] {
			case "assistant":
				if strings.Contains(string(b), `"tool_use"`) {
					note("STDOUT assistant tool_use " + string(b))
				}
				var msg struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				json.Unmarshal(b, &msg)
				for _, c := range msg.Content {
					if c.Type == "text" {
						resultsMu.Lock()
						texts = append(texts, c.Text)
						resultsMu.Unlock()
					}
				}
			case "user":
				var msg struct {
					Content []struct {
						Type    string `json:"type"`
						IsError bool   `json:"is_error"`
					} `json:"content"`
				}
				json.Unmarshal(b, &msg)
				for _, c := range msg.Content {
					if c.Type == "tool_result" {
						note("STDOUT tool_result " + string(b))
						resultsMu.Lock()
						toolErrors = append(toolErrors, c.IsError)
						resultsMu.Unlock()
					}
				}
			case "result":
				note("STDOUT result")
				results <- m
			}
		}
		close(results)
	}()
	waitResult := func(timeout time.Duration) map[string]any {
		select {
		case r := <-results:
			return r
		case <-time.After(timeout):
			t.Fatal("no result")
		}
		return nil
	}

	lp.send(`Call the tool mcp__pomar__pomar_controller_request exactly once with capability "inbox" and data "next". It may take a few minutes to answer; wait for it. Then reply with only the code word it returned.`)
	r1 := waitResult(delay + 3*time.Minute)
	lp.send(`Call the tool mcp__pomar__pomar_controller_request exactly once with capability "ack" and data "post 99". Then reply with only the word ERROR if the tool reported an error, or OK if it did not.`)
	r2 := waitResult(3 * time.Minute)
	lp.p.Stdin().Close()
	lp.p.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, o := range order {
		t.Log(o)
	}
	resultsMu.Lock()
	defer resultsMu.Unlock()
	t.Logf("texts %q, tool result errors %v, results %v / %v", texts, toolErrors, r1["subtype"], r2["subtype"])
	use, bridge := -1, -1
	for i, o := range order {
		if use < 0 && strings.Contains(o, `"name":"`+ControllerToolName+`"`) && strings.Contains(o, `"input":{"capability":"inbox","data":"next"}`) {
			use = i
		}
		if bridge < 0 && strings.Contains(o, "BRIDGE "+ControllerTool+" ") {
			bridge = i
		}
		if strings.Contains(o, "BRIDGE "+PermissionTool) {
			t.Errorf("the controller call reached the permission tool: %s", o)
		}
	}
	if use < 0 || bridge < 0 || use > bridge {
		t.Fatalf("the controller tool_use line did not precede its bridge call (use %d, bridge %d)", use, bridge)
	}
	if !strings.Contains(strings.Join(texts, " "), "ORCHARD-41") {
		t.Fatalf("the late reply did not reach the model: %q", texts)
	}
	if len(toolErrors) < 2 || toolErrors[0] || !toolErrors[len(toolErrors)-1] || !strings.Contains(texts[len(texts)-1], "ERROR") {
		t.Fatalf("tool results %v, last text %q", toolErrors, texts[len(texts)-1])
	}
}
