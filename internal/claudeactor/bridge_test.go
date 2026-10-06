package claudeactor

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// holdTurn keeps a turn active until the fake is interrupted.
func holdTurn(_ *fakeClaude, session string, _ int, _ string) []string {
	return []string{
		line(map[string]any{"type": "system", "subtype": "init", "session_id": session}),
		"WAIT-INTERRUPT",
		line(map[string]any{"type": "result", "subtype": "error_during_execution", "session_id": session, "is_error": true}),
	}
}

// bridge starts the actor's bridge on a short private socket path.
func bridge(t *testing.T, a *Actor) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go a.ServeBridge(ln)
	return sock
}

func permissionEvent(t *testing.T, store *agentenv.Store) agentenv.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range store.Snapshot().Events {
			if strings.HasSuffix(e.Method, "/requestApproval") {
				return e
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no permission request was journaled")
	return agentenv.Event{}
}

func decide(t *testing.T, b *agentenv.Broker, permission, decision, op string) int {
	t.Helper()
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	body := `{"expected_incarnation":"inc-1","decision":"` + decision + `","operation_id":"` + op + `"}`
	resp, err := http.Post(srv.URL+"/v1/permissions/"+permission, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

type forwarded struct {
	reply bridgeReply
	err   error
}

func forwardAsync(sock, tool string, args string) chan forwarded {
	ch := make(chan forwarded, 1)
	go func() {
		r, err := Forward(sock, tool, json.RawMessage(args))
		ch <- forwarded{r, err}
	}()
	return ch
}

func behaviour(t *testing.T, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("permission reply %q: %v", text, err)
	}
	return m
}

// A Bash permission becomes the broker's command approval; the controller's
// accept through the HTTP route allows it with the original input.
func TestPermissionAcceptThroughTheBroker(t *testing.T) {
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", holdTurn, true)
	sock := bridge(t, h.actor)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-1", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	ch := forwardAsync(sock, PermissionTool, `{"tool_name":"Bash","input":{"command":"go test ./..."}}`)
	e := permissionEvent(t, h.store)
	if e.Method != "item/commandExecution/requestApproval" || e.PermissionID == "" {
		t.Fatalf("journaled %+v", e)
	}
	if code := decide(t, h.broker, e.PermissionID, "accept", "perm-1"); code != 200 {
		t.Fatalf("accept: HTTP %d", code)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	m := behaviour(t, r.reply.Text)
	if m["behavior"] != "allow" || m["updatedInput"].(map[string]any)["command"] != "go test ./..." {
		t.Fatalf("accept became %v", m)
	}
	h.fakes[0].Interrupt()
	h.waitOp("op-1", finished)
}

// A file edit is a file-change approval; decline denies it.
func TestPermissionDeclineIsDeny(t *testing.T) {
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", holdTurn, true)
	sock := bridge(t, h.actor)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-1", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	ch := forwardAsync(sock, PermissionTool, `{"tool_name":"Edit","input":{"file_path":"/work/a.go"}}`)
	e := permissionEvent(t, h.store)
	if e.Method != "item/fileChange/requestApproval" {
		t.Fatalf("an Edit raised %s", e.Method)
	}
	if code := decide(t, h.broker, e.PermissionID, "decline", "perm-1"); code != 200 {
		t.Fatalf("decline: HTTP %d", code)
	}
	if m := behaviour(t, (<-ch).reply.Text); m["behavior"] != "deny" {
		t.Fatalf("decline became %v", m)
	}
	h.fakes[0].Interrupt()
	h.waitOp("op-1", finished)
}

// Outside a turn, a permission call is denied at once and nothing is journaled.
func TestPermissionWithoutATurnIsDenied(t *testing.T) {
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", holdTurn, true)
	sock := bridge(t, h.actor)
	r, err := Forward(sock, PermissionTool, json.RawMessage(`{"tool_name":"Bash","input":{"command":"true"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := behaviour(t, r.Text); m["behavior"] != "deny" {
		t.Fatalf("outside a turn: %v", m)
	}
	if n := len(h.store.Snapshot().Events); n != 0 {
		t.Fatalf("%d events journaled", n)
	}
}

// A call still waiting when its turn ends is denied, never left to approve later.
func TestPermissionWaitingWhenTheTurnEndsIsDenied(t *testing.T) {
	h := open(t, filepath.Join(t.TempDir(), "state"), "inc-1", holdTurn, true)
	sock := bridge(t, h.actor)
	if _, err := h.broker.Submit(agentenv.Task{OperationID: "op-1", Incarnation: "inc-1", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h.waitOp("op-1", func(op agentenv.Operation) bool { return op.ActorAcknowledged })
	ch := forwardAsync(sock, PermissionTool, `{"tool_name":"Bash","input":{"command":"true"}}`)
	permissionEvent(t, h.store)
	h.fakes[0].Interrupt()
	h.waitOp("op-1", finished)
	select {
	case r := <-ch:
		if m := behaviour(t, r.reply.Text); m["behavior"] != "deny" {
			t.Fatalf("after the turn ended: %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting call was not released when its turn ended")
	}
}

// The MCP server: initialize, exactly one tool, forwarding, unknown tools
// refused, and an unreachable bridge denied rather than approved.
func TestServeMCP(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	call := func(tool string, args json.RawMessage) (bridgeReply, error) {
		mu.Lock()
		calls = append(calls, tool+" "+string(args))
		mu.Unlock()
		if strings.Contains(string(args), "unreachable") {
			return bridgeReply{}, io.ErrClosedPipe
		}
		return bridgeReply{Text: allowText(json.RawMessage(`{"command":"true"}`))}, nil
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"permission","arguments":{"tool_name":"Bash","input":{"command":"true"}}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"other","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"permission","arguments":{"tool_name":"unreachable","input":{}}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := ServeMCP(strings.NewReader(in), &out, call); err != nil {
		t.Fatal(err)
	}
	replies := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		json.Unmarshal([]byte(l), &m)
		id, _ := json.Marshal(m["id"])
		replies[string(id)] = m
	}
	if v := replies["1"]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("initialize: %v", replies["1"])
	}
	tools := replies["2"]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "permission" {
		t.Errorf("tools/list: %v", tools)
	}
	text := replies["3"]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if behaviour(t, text)["behavior"] != "allow" {
		t.Errorf("forwarded call: %s", text)
	}
	if replies["4"]["error"] == nil {
		t.Errorf("an unknown tool was not refused: %v", replies["4"])
	}
	text = replies["5"]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if behaviour(t, text)["behavior"] != "deny" {
		t.Errorf("an unreachable bridge was not denied: %s", text)
	}
	if len(calls) != 2 {
		t.Errorf("forwarded %d calls, want 2: %v", len(calls), calls)
	}
}
