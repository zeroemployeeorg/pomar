package claudeactor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// The actor's side of the bridge. A permission call from Claude Code becomes
// the shared broker's per-request approval (the same item/*/requestApproval
// a Codex actor raises); the controller's accept, decline or cancel through
// POST /v1/permissions/{id} comes back as Answer, and only accept allows.
// Everything else fails closed: no active turn, a refused request, the turn
// ending, or the actor departing all deny.

// fileTools are Claude Code tools whose approval is a file change; any other
// tool's approval is a command execution.
var fileTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

type permissionWait struct {
	input    json.RawMessage
	decision chan string
}

// ServeBridge accepts tool calls from the MCP server on ln until ln closes.
// The listener's socket must be reachable by the coding user only.
func (a *Actor) ServeBridge(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go a.serveBridgeConn(c)
	}
}

func (a *Actor) serveBridgeConn(c net.Conn) {
	defer c.Close()
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req bridgeRequest
	var reply bridgeReply
	if json.Unmarshal(line, &req) != nil || req.Tool != PermissionTool {
		reply = bridgeReply{Text: denyText("unsupported bridge request")}
	} else {
		reply = bridgeReply{Text: a.requestPermission(req.Arguments)}
	}
	b, _ := json.Marshal(reply)
	c.Write(append(b, '\n'))
}

func (a *Actor) requestPermission(args json.RawMessage) string {
	var p struct {
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	}
	if json.Unmarshal(args, &p) != nil || p.ToolName == "" {
		return denyText("malformed permission request")
	}
	a.mu.Lock()
	if a.turn == "" || a.thread == "" {
		a.mu.Unlock()
		return denyText("no active Pomar turn")
	}
	a.nextPermission++
	id, _ := json.Marshal(fmt.Sprintf("claude-permission-%d", a.nextPermission))
	w := &permissionWait{input: p.Input, decision: make(chan string, 1)}
	if a.permissions == nil {
		a.permissions = map[string]*permissionWait{}
	}
	a.permissions[string(id)] = w
	thread, turn := a.thread, a.turn
	a.mu.Unlock()

	method := "item/commandExecution/requestApproval"
	if fileTools[p.ToolName] {
		method = "item/fileChange/requestApproval"
	}
	params, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "toolName": p.ToolName, "input": p.Input})
	if err := a.observe(agentenv.Message{ID: id, Method: method, Params: params}); err != nil {
		a.mu.Lock()
		delete(a.permissions, string(id))
		a.mu.Unlock()
		return denyText("the request was not recorded")
	}
	select {
	case d := <-w.decision:
		switch d {
		case "accept":
			return allowText(p.Input)
		case "decline":
			return denyText("declined by the environment's controller")
		}
		return denyText("cancelled")
	case <-a.done:
		return denyText("the actor ended")
	}
}

// answerPermission delivers the controller's decision to its waiting call.
func (a *Actor) answerPermission(id json.RawMessage, result any) error {
	var r struct {
		Decision string `json:"decision"`
	}
	if remarshal(result, &r) != nil || (r.Decision != "accept" && r.Decision != "decline" && r.Decision != "cancel") {
		return errors.New("claude actor: unsupported permission decision")
	}
	a.mu.Lock()
	w, ok := a.permissions[string(id)]
	delete(a.permissions, string(id))
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("claude actor: no waiting request %s", string(id))
	}
	w.decision <- r.Decision
	return nil
}

// cancelPermissions denies every waiting call; a.mu must be held.
func (a *Actor) cancelPermissions() {
	for id, w := range a.permissions {
		w.decision <- "cancel"
		delete(a.permissions, id)
	}
}
