package claudeactor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// unixListener listens on a short private socket path.
func unixListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pcct")
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
	return ln, sock
}

// Forward tells a request that never reached the bridge from one written in
// full whose reply was lost or late: only the latter is uncertain.
func TestForwardSeparatesNotDeliveredFromUncertain(t *testing.T) {
	_, err := Forward(filepath.Join(t.TempDir(), "absent.sock"), ControllerTool, json.RawMessage(`{}`))
	if err == nil || errors.Is(err, ErrDeliveryUncertain) {
		t.Fatalf("no bridge: %v", err)
	}

	ln, sock := unixListener(t)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		bufio.NewReader(c).ReadBytes('\n') // read in full, then lose the reply
		c.Close()
	}()
	if _, err := Forward(sock, ControllerTool, json.RawMessage(`{}`)); !errors.Is(err, ErrDeliveryUncertain) {
		t.Fatalf("a lost reply: %v", err)
	}

	defer func(w time.Duration) { forwardReplyWait = w }(forwardReplyWait)
	forwardReplyWait = 300 * time.Millisecond
	held := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		bufio.NewReader(c).ReadBytes('\n')
		held <- c // and never reply
	}()
	start := time.Now()
	if _, err := Forward(sock, ControllerTool, json.RawMessage(`{}`)); !errors.Is(err, ErrDeliveryUncertain) || time.Since(start) > 5*time.Second {
		t.Fatalf("a late reply: %v after %v", err, time.Since(start))
	}
	(<-held).Close()
}

func mcpReplies(t *testing.T, out string) map[string][]string {
	t.Helper()
	byID := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var m struct {
			ID json.RawMessage `json:"id"`
		}
		json.Unmarshal([]byte(l), &m)
		byID[string(m.ID)] = append(byID[string(m.ID)], l)
	}
	return byID
}

// An uncertain delivery is reported as uncertain, never as "not delivered",
// and never as an approval.
func TestServeMCPReportsUncertainDelivery(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pomar_controller_request","arguments":{"capability":"inbox","data":"x"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"permission","arguments":{"tool_name":"Bash","input":{}}}}
`
	var out syncBuffer
	err := ServeMCPController(strings.NewReader(in), &out, []string{"inbox"}, func(string, json.RawMessage) (bridgeReply, error) {
		return bridgeReply{}, ErrDeliveryUncertain
	})
	if err != nil {
		t.Fatal(err)
	}
	r := mcpReplies(t, out.String())
	if c := strings.Join(r["1"], ""); !strings.Contains(c, "uncertain") || !strings.Contains(c, "Do not repeat") || strings.Contains(c, "not delivered") || !strings.Contains(c, `"isError":true`) {
		t.Fatalf("controller reply %s", c)
	}
	if p := strings.Join(r["2"], ""); !strings.Contains(p, `\"behavior\":\"deny\"`) || !strings.Contains(p, "uncertain") {
		t.Fatalf("permission reply %s", p)
	}
}

// A reused JSON-RPC request ID with the same input is answered with the one
// outcome, in flight or completed, and never dispatched twice; with other
// input it is refused.
func TestServeMCPDuplicateRequestIDs(t *testing.T) {
	inR, inW := io.Pipe()
	var out syncBuffer
	var calls atomic.Int32
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeMCPController(inR, &out, []string{"inbox"}, func(string, json.RawMessage) (bridgeReply, error) {
			n := calls.Add(1)
			<-release
			return bridgeReply{Text: fmt.Sprintf("outcome %d", n)}, nil
		})
	}()
	send := func(id int, data string) {
		fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"pomar_controller_request","arguments":{"capability":"inbox","data":%q}}}`+"\n", id, data)
	}
	waitFor := func(cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	send(7, "a")
	waitFor(func() bool { return calls.Load() == 1 })
	send(7, "a") // the same call again, while the first is held
	send(7, "b") // the same ID, other input
	waitFor(func() bool { return strings.Contains(out.String(), "reused with different input") })
	close(release)
	waitFor(func() bool { return len(mcpReplies(t, out.String())["7"]) == 3 })
	send(7, "a") // after it completed
	waitFor(func() bool { return len(mcpReplies(t, out.String())["7"]) == 4 })
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("dispatched %d times", n)
	}
	var results []string
	for _, l := range mcpReplies(t, out.String())["7"] {
		if strings.Contains(l, "reused with different input") {
			continue
		}
		results = append(results, l)
	}
	if len(results) != 3 || results[0] != results[1] || results[1] != results[2] || !strings.Contains(results[0], "outcome 1") {
		t.Fatalf("replies %q", results)
	}
}

// Connections beyond maxBridgeConns wait unread in the backlog. A request
// written there is uncertain to its sender, and is served once a slot frees.
func TestBridgeConnectionsAreBounded(t *testing.T) {
	defer func(w time.Duration) { bridgeReadWait = w }(bridgeReadWait)
	bridgeReadWait = time.Minute // set before the actor is created: it takes the value then
	defer func(w time.Duration) { forwardReplyWait = w }(forwardReplyWait)
	forwardReplyWait = 500 * time.Millisecond
	h, sock := startHeld(t)
	defer func() { h.fakes[0].Interrupt(); h.waitOp("op-1", finished) }()
	var silent []net.Conn
	for i := 0; i < maxBridgeConns; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		silent = append(silent, c)
	}
	emitToolUse(t, h, "toolu_1", "Bash", `{"command":"ls"}`)
	_, err := Forward(sock, PermissionTool, json.RawMessage(`{"tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"toolu_1"}`))
	if !errors.Is(err, ErrDeliveryUncertain) {
		t.Fatalf("over the bound: %v", err)
	}
	if n := approvals(h); n != 0 {
		t.Fatalf("a request over the bound was served (%d raised)", n)
	}
	for _, c := range silent {
		c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for approvals(h) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if approvals(h) != 1 {
		t.Fatal("the waiting request was not served once slots freed")
	}
}
