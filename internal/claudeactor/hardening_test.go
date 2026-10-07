package claudeactor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// The cap counts requests already raised and waiting for the controller, not
// only those waiting for their tool_use line: raised permission requests fill
// it, and one more is refused unrecorded.
func TestBridgeCapCountsRaisedRequests(t *testing.T) {
	h, sock := startHeld(t)
	defer func() { h.fakes[0].Interrupt(); h.waitOp("op-1", finished) }()
	var chans []chan forwarded
	for i := 0; i < maxBridgeWaiting; i++ {
		id := fmt.Sprintf("toolu_%d", i)
		input := fmt.Sprintf(`{"command":"echo %d"}`, i)
		emitToolUse(t, h, id, "Bash", input)
		chans = append(chans, forwardAsync(sock, PermissionTool, `{"tool_name":"Bash","input":`+input+`,"tool_use_id":"`+id+`"}`))
	}
	deadline := time.Now().Add(5 * time.Second)
	for approvals(h) < maxBridgeWaiting && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := approvals(h); n != maxBridgeWaiting {
		t.Fatalf("%d requests raised", n)
	}
	emitToolUse(t, h, "toolu_more", "Bash", `{"command":"echo more"}`)
	// A request admitted over the cap would wait for a decision; the refusal
	// must come at once.
	r := waitForward(t, forwardAsync(sock, PermissionTool, `{"tool_name":"Bash","input":{"command":"echo more"},"tool_use_id":"toolu_more"}`))
	if behaviour(t, r.Text)["message"] != "too many waiting requests" {
		t.Fatalf("over the cap: %v", r)
	}
	if n := approvals(h); n != maxBridgeWaiting {
		t.Fatalf("the refused request was journaled (%d)", n)
	}
	h.fakes[0].Interrupt()
	for _, ch := range chans {
		<-ch
	}
}

// A bridge connection's request line is bounded in size and in time: an
// oversized or silent connection is closed unanswered and nothing is raised.
func TestBridgeReadsAreBounded(t *testing.T) {
	defer func(w time.Duration) { bridgeReadWait = w }(bridgeReadWait)
	bridgeReadWait = 300 * time.Millisecond
	h, sock := startHeld(t)
	defer func() { h.fakes[0].Interrupt(); h.waitOp("op-1", finished) }()
	unanswered := func(name string, send func(net.Conn)) {
		t.Helper()
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		go send(c)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var b [1]byte
		if n, _ := c.Read(b[:]); n != 0 {
			t.Fatalf("%s: answered", name)
		}
	}
	unanswered("oversized", func(c net.Conn) {
		big := bytes.Repeat([]byte("a"), maxBridgeLine+16)
		c.Write([]byte(`{"tool":"permission","arguments":"`))
		c.Write(big)
	})
	unanswered("silent", func(net.Conn) {})
	if n := approvals(h); n != 0 {
		t.Fatalf("%d requests raised", n)
	}
}

// Bound input is compared canonically: key order does not matter, numbers
// keep their exact text, and a duplicate key or trailing data matches nothing.
func TestCanonicalBoundInput(t *testing.T) {
	if canonical(json.RawMessage(`{"b":1,"a":[2,{"c":3}]}`)) != canonical(json.RawMessage(`{"a":[2,{"c":3}],"b":1}`)) {
		t.Error("key order changed the canonical form")
	}
	if canonical(json.RawMessage(`{"n":9007199254740993}`)) == canonical(json.RawMessage(`{"n":9007199254740992}`)) {
		t.Error("distinct large integers compared equal")
	}
	for _, bad := range []string{`{"command":"ls","command":"rm -r x"}`, `{"a":{"b":1,"b":2}}`, `[{"k":1,"k":1}]`, `{"a":1} {"b":2}`, `{"a":`, ``} {
		if c := canonical(json.RawMessage(bad)); c != "" {
			t.Errorf("%q canonicalised to %q", bad, c)
		}
	}
}

// A request whose input repeats a key is refused even when one reading of it
// matches Claude Code's tool call.
func TestBridgeRefusesDuplicateKeyInput(t *testing.T) {
	defer func(w time.Duration) { toolUseWait = w }(toolUseWait)
	toolUseWait = 200 * time.Millisecond
	h, sock := startHeld(t)
	defer func() { h.fakes[0].Interrupt(); h.waitOp("op-1", finished) }()
	emitToolUse(t, h, "toolu_1", "Bash", `{"command":"ls"}`)
	// Bound by mistake, the request would wait for a decision; the refusal
	// must come at once.
	r := waitForward(t, forwardAsync(sock, PermissionTool, `{"tool_name":"Bash","input":{"command":"rm -r x","command":"ls"},"tool_use_id":"toolu_1"}`))
	if behaviour(t, r.Text)["behavior"] != "deny" || approvals(h) != 0 {
		t.Fatalf("duplicate-key input: %v (%d raised)", r, approvals(h))
	}
}

// authFailedTurn is a turn whose API call failed to authenticate, as Claude
// Code 2.1.280 reports it in stream-json.
func authFailedTurn(_ *fakeClaude, session string, _ int, _ string) []string {
	return []string{
		line(map[string]any{"type": "system", "subtype": "init", "session_id": session}),
		line(map[string]any{"type": "assistant", "session_id": session, "error": "authentication_failed", "is_api_error_message": true, "message": map[string]any{"role": "assistant", "model": "<synthetic>", "content": []any{map[string]string{"type": "text", "text": "Failed to authenticate: OAuth session expired and could not be refreshed"}}}}),
		line(map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": true, "terminal_reason": "api_error"}),
	}
}

func sessionAuthenticated(t *testing.T, b *agentenv.Broker) bool {
	t.Helper()
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s struct {
		Authenticated bool `json:"authenticated"`
	}
	json.NewDecoder(resp.Body).Decode(&s)
	return s.Authenticated
}

// Sign-in validity is observed, not assumed from the credential file: a turn
// that fails to authenticate leaves the environment reporting no account, so
// the next assignment is refused and its controller can see a sign-in is due.
// A completed sign-in clears it and retires the process that held the old
// credential, so the next turn resumes the session in a new one.
func TestAuthenticationFailureIsObserved(t *testing.T) {
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", authFailedTurn, true)
	if !sessionAuthenticated(t, h.broker) {
		t.Fatal("not authenticated before any turn")
	}
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if op := h.waitOp("op-1", finished); op.Completion != "failed" {
		t.Fatalf("an authentication failure completed as %q", op.Completion)
	}
	if sessionAuthenticated(t, h.broker) {
		t.Fatal("still reported authenticated after Claude Code failed to authenticate")
	}
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-2", Incarnation: "inc-1", Text: "y"}); err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("an assignment after the failure: %v", err)
	}
	h.actor.signedInAgain()
	if !sessionAuthenticated(t, h.broker) {
		t.Fatal("not authenticated after a completed sign-in")
	}
	h.mu.Lock()
	first := h.fakes[0]
	h.mu.Unlock()
	select {
	case <-first.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the process holding the old credential was not retired")
	}
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-3", Incarnation: "inc-1", Text: "z"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-3", finished)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.fakes) != 2 || !strings.Contains(strings.Join(h.fakes[1].args, " "), "--resume") {
		t.Fatalf("the next turn did not resume in a new process (%d processes)", len(h.fakes))
	}
}

// The MCP server bounds tool calls in flight: beyond it a call is answered
// with an error at once, never queued without limit.
func TestServeMCPBoundsCallsInFlight(t *testing.T) {
	release := make(chan struct{})
	var in strings.Builder
	for i := 0; i <= maxMCPInFlight; i++ {
		fmt.Fprintf(&in, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"permission","arguments":{}}}`+"\n", i)
	}
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- ServeMCPController(strings.NewReader(in.String()), &out, nil, func(string, json.RawMessage) (bridgeReply, error) {
			<-release
			return bridgeReply{Text: denyText("x")}, nil
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "too many tool calls in flight") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out.String(), `"id":`+fmt.Sprint(maxMCPInFlight)+`,`) || !strings.Contains(out.String(), "too many tool calls in flight") {
		t.Fatalf("the call over the bound was not refused: %s", out.String())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// syncBuffer is a bytes.Buffer safe for the MCP server's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
