package claudeactor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// startController is startHeld with the environment's controller
// capabilities configured, as the guest configures them from its profile.
func startController(t *testing.T, capabilities ...string) (*harness, string) {
	t.Helper()
	h, err := openController(t, filepath.Join(t.TempDir(), "state"), "inc-1", holdTurn, true, "2.1.280", capabilities)
	if err != nil {
		t.Fatal(err)
	}
	sock := bridge(t, h.actor)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-1", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	return h, sock
}

type controllerList struct {
	Contract  string                                `json:"contract"`
	Native    string                                `json:"native_method"`
	Supported bool                                  `json:"supported"`
	Requests  map[string]agentenv.ControllerRequest `json:"requests"`
}

func controllerRequests(t *testing.T, b *agentenv.Broker) controllerList {
	t.Helper()
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/controller/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var l controllerList
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

// waitControllerRequest waits for the one journaled controller request.
func waitControllerRequest(t *testing.T, b *agentenv.Broker) (string, agentenv.ControllerRequest) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for id, r := range controllerRequests(t, b).Requests {
			return id, r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no controller request was journaled")
	return "", agentenv.ControllerRequest{}
}

func replyController(t *testing.T, b *agentenv.Broker, id string, r agentenv.ControllerRequest, replyID, text string, success bool) int {
	t.Helper()
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	body, _ := json.Marshal(map[string]any{"reply_id": replyID, "binding": r.Binding, "text": text, "success": success})
	resp, err := http.Post(srv.URL+"/v1/controller/requests/"+id+"/reply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func waitForward(t *testing.T, ch chan forwarded) bridgeReply {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.reply
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge call did not return")
	}
	return bridgeReply{}
}

// A controller request Claude Code makes through the MCP tool is raised as
// the shared broker's native controller request (the item/tool/call a Codex
// dynamic tool makes), bound to the operation, turn and Claude's own tool_use
// id; the controller's reply through the broker's route is the tool result.
func TestControllerRequestThroughTheBroker(t *testing.T) {
	h, sock := startController(t, "inbox", "answer")
	if l := controllerRequests(t, h.broker); !l.Supported || l.Native != "item/tool/call" || l.Contract != "pomar.controller/v1" {
		t.Fatalf("controller route %+v", l)
	}
	args := `{"capability":"inbox","data":"next post"}`
	emitToolUse(t, h, "toolu_c1", ControllerToolName, args)
	ch := forwardAsync(sock, ControllerTool, args)
	id, r := waitControllerRequest(t, h.broker)
	op := h.store.Snapshot().Operations["op-1"]
	if r.Capability != "inbox" || r.Data != "next post" || r.State != "pending" || r.Binding.CallID != "toolu_c1" || r.Binding.OperationID != "op-1" || r.Binding.TurnID != op.TurnID || r.Binding.Incarnation != "inc-1" {
		t.Fatalf("journaled %+v", r)
	}
	if code := replyController(t, h.broker, id, r, "reply-1", "post 42: deploy window at 21:00", true); code != 200 {
		t.Fatalf("reply: HTTP %d", code)
	}
	got := waitForward(t, ch)
	if got.IsError || got.Text != "post 42: deploy window at 21:00" {
		t.Fatalf("tool result %+v", got)
	}
	if s := controllerRequests(t, h.broker).Requests[id]; s.State != "written" || s.Reply == nil || s.Reply.ReplyID != "reply-1" {
		t.Fatalf("after the reply %+v", s)
	}
	// The same reply again is the retained outcome, not a second write.
	if code := replyController(t, h.broker, id, r, "reply-1", "post 42: deploy window at 21:00", true); code != 200 {
		t.Fatalf("identical retry: HTTP %d", code)
	}
}

// An unsuccessful reply is an error result for Claude Code.
func TestControllerFailureReplyIsAnErrorResult(t *testing.T) {
	h, sock := startController(t, "ack")
	args := `{"capability":"ack","data":"post 7"}`
	emitToolUse(t, h, "toolu_c1", ControllerToolName, args)
	ch := forwardAsync(sock, ControllerTool, args)
	id, r := waitControllerRequest(t, h.broker)
	replyController(t, h.broker, id, r, "reply-1", "post 7 is not the current command", false)
	if got := waitForward(t, ch); !got.IsError || got.Text != "post 7 is not the current command" {
		t.Fatalf("tool result %+v", got)
	}
}

// A request is refused unrecorded unless it matches a tool call Claude Code
// emitted in this turn, each call once, for a configured capability.
func TestControllerRefusesRequestsNotBoundToAToolCall(t *testing.T) {
	saved := toolUseWait
	toolUseWait = 200 * time.Millisecond
	t.Cleanup(func() { toolUseWait = saved })
	h, sock := startController(t, "inbox")
	refused := func(name, args string) {
		t.Helper()
		if got := waitForward(t, forwardAsync(sock, ControllerTool, args)); !got.IsError {
			t.Fatalf("%s: %+v", name, got)
		}
		if n := len(controllerRequests(t, h.broker).Requests); n != 0 {
			t.Fatalf("%s: %d requests journaled", name, n)
		}
	}
	refused("no tool call", `{"capability":"inbox","data":"x"}`)
	emitToolUse(t, h, "toolu_c1", ControllerToolName, `{"capability":"inbox","data":"x"}`)
	refused("other data", `{"capability":"inbox","data":"y"}`)
	refused("unconfigured capability", `{"capability":"answer","data":"x"}`)
	refused("an extra field", `{"capability":"inbox","data":"x","to":"elsewhere"}`)
	emitToolUse(t, h, "toolu_b1", "Bash", `{"capability":"inbox","data":"z"}`)
	refused("another tool's call", `{"capability":"inbox","data":"z"}`)
	// The bound call passes once; the same call again is refused.
	ch := forwardAsync(sock, ControllerTool, `{"capability":"inbox","data":"x"}`)
	id, r := waitControllerRequest(t, h.broker)
	replyController(t, h.broker, id, r, "reply-1", "ok", true)
	waitForward(t, ch)
	if got := waitForward(t, forwardAsync(sock, ControllerTool, `{"capability":"inbox","data":"x"}`)); !got.IsError {
		t.Fatalf("a repeat: %+v", got)
	}
	if n := len(controllerRequests(t, h.broker).Requests); n != 1 {
		t.Fatalf("%d requests journaled", n)
	}
}

// A request still waiting when its turn ends gets an error result, and the
// controller's late reply is refused as stale: it never reaches Claude Code.
func TestControllerWaitingWhenTheTurnEndsIsUnanswered(t *testing.T) {
	h, sock := startController(t, "inbox")
	args := `{"capability":"inbox","data":"x"}`
	emitToolUse(t, h, "toolu_c1", ControllerToolName, args)
	ch := forwardAsync(sock, ControllerTool, args)
	id, r := waitControllerRequest(t, h.broker)
	if _, err := h.actor.Call(context.Background(), "turn/interrupt", nil); err != nil {
		t.Fatal(err)
	}
	if got := waitForward(t, ch); !got.IsError || !strings.Contains(got.Text, "turn ended") {
		t.Fatalf("tool result %+v", got)
	}
	h.waitOp("op-1", finished)
	if code := replyController(t, h.broker, id, r, "reply-late", "too late", true); code == 200 {
		t.Fatal("a reply after the turn ended was accepted")
	}
}

// Controller capabilities are refused at Initialize for a Claude Code
// version the controller bridge is not qualified for.
func TestControllerUnqualifiedVersionIsRefused(t *testing.T) {
	if _, err := openController(t, filepath.Join(t.TempDir(), "state"), "inc-1", holdTurn, true, "2.1.279", []string{"inbox"}); err == nil {
		t.Fatal("an unqualified version was accepted with controller capabilities")
	}
}

// Claude Code's tools are fixed at launch, so thread/start refuses dynamic
// tools other than the ones the actor was launched with.
func TestThreadStartRefusesOtherControllerTools(t *testing.T) {
	a := New(Config{Version: "2.1.280", ControllerCapabilities: []string{"inbox"}, Start: func([]string) (Process, error) {
		t.Fatal("a process was started")
		return nil, nil
	}}, func(agentenv.Message) error { return nil })
	tool := func(name string, enum ...string) map[string]any {
		return map[string]any{"name": name, "inputSchema": map[string]any{"properties": map[string]any{"capability": map[string]any{"enum": enum}}}}
	}
	for name, tools := range map[string][]any{
		"more capabilities": {tool(ControllerTool, "inbox", "answer")},
		"none":              {},
		"another tool":      {tool("shell", "inbox")},
	} {
		if _, err := a.Call(context.Background(), "thread/start", map[string]any{"dynamicTools": tools}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The MCP server lists and serves the controller tool only when the
// environment configures capabilities.
func TestServeMCPControllerTool(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pomar_controller_request","arguments":{"capability":"inbox","data":"x"}}}
`
	run := func(capabilities []string) []map[string]any {
		var out bytes.Buffer
		call := func(tool string, args json.RawMessage) (bridgeReply, error) {
			return bridgeReply{Text: "via " + tool}, nil
		}
		if err := ServeMCPController(strings.NewReader(in), &out, capabilities, call); err != nil {
			t.Fatal(err)
		}
		var msgs []map[string]any
		for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var m map[string]any
			json.Unmarshal([]byte(l), &m)
			msgs = append(msgs, m)
		}
		return msgs
	}
	with := run([]string{"inbox", "ack"})
	b, _ := json.Marshal(with)
	if !strings.Contains(string(b), `"name":"pomar_controller_request"`) || !strings.Contains(string(b), `"enum":["inbox","ack"]`) || !strings.Contains(string(b), `"text":"via pomar_controller_request"`) {
		t.Fatalf("with capabilities: %s", b)
	}
	without := run(nil)
	b, _ = json.Marshal(without)
	if strings.Contains(string(b), `"name":"pomar_controller_request"`) || !strings.Contains(string(b), `"unknown tool"`) {
		t.Fatalf("without capabilities: %s", b)
	}
	cfg, _ := MCPConfig("/g", "/s", "inbox", "ack")
	if !strings.Contains(cfg, `"-controller-capabilities","inbox,ack"`) {
		t.Fatalf("mcp config %s", cfg)
	}
}

// Request IDs carry the actor's nonce, so the broker's permission IDs differ
// across actors (incarnations) for the same first request (SOW 06 finding 2).
func TestPermissionIDsDifferAcrossActors(t *testing.T) {
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		h, sock := startHeld(t)
		emitToolUse(t, h, "toolu_1", "Bash", `{"command":"go test ./..."}`)
		forwardAsync(sock, PermissionTool, `{"tool_name":"Bash","input":{"command":"go test ./..."},"tool_use_id":"toolu_1"}`)
		e := permissionEvent(t, h.store)
		if !regexp.MustCompile(`^"claude-permission-[0-9a-f]{12}-1"$`).Match(e.RequestID) {
			t.Fatalf("request id %s", e.RequestID)
		}
		ids[e.PermissionID] = true
	}
	if len(ids) != 2 {
		t.Fatal("two actors' first permission requests share a permission id")
	}
}
