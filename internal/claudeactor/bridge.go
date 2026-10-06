package claudeactor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

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
	switch {
	case json.Unmarshal(line, &req) != nil:
		reply = bridgeReply{Text: denyText("unsupported bridge request")}
	case req.Tool == PermissionTool:
		reply = bridgeReply{Text: a.requestPermission(req.Arguments)}
	case req.Tool == ControllerTool:
		reply = a.requestController(req.Arguments)
	default:
		reply = bridgeReply{Text: denyText("unsupported bridge request")}
	}
	b, _ := json.Marshal(reply)
	c.Write(append(b, '\n'))
}

// A bridge request is honoured only when it names a tool call Claude Code
// itself emitted on stdout in the current turn, with the same tool and the
// same input, once per call (adviser note r41 §3.2). Qualified on 2.1.280:
// the permission call carries tool_use_id, and the assistant tool_use line
// arrives on stdout before it. Anything else, including a request the coding
// user writes to the socket directly, is refused unrecorded.

// toolUseWait bounds how long a bridge request waits for its tool_use line.
var toolUseWait = 5 * time.Second

// maxBridgeWaiting caps bridge requests waiting at once, so the queue cannot
// be flooded.
const maxBridgeWaiting = 16

type toolUse struct {
	name      string
	input     string // canonical JSON
	requested bool
}

// recordToolUses notes the tool_use blocks of an assistant message in the
// current turn and wakes requests waiting for them; a.mu must be held.
func (a *Actor) recordToolUses(message json.RawMessage) {
	var m struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(message, &m) != nil {
		return
	}
	for _, c := range m.Content {
		if c.Type != "tool_use" || c.ID == "" {
			continue
		}
		if a.toolUses == nil {
			a.toolUses = map[string]*toolUse{}
		}
		if _, seen := a.toolUses[c.ID]; !seen {
			a.toolUses[c.ID] = &toolUse{name: c.Name, input: canonical(c.Input)}
		}
	}
	if a.toolUseSeen != nil {
		close(a.toolUseSeen)
		a.toolUseSeen = nil
	}
}

func canonical(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// bindToolUse waits for the named tool call and claims it; a.mu must be held,
// and is held again on return.
func (a *Actor) bindToolUse(id, name string, input json.RawMessage) string {
	if id == "" {
		return "the request names no tool call"
	}
	deadline := time.Now().Add(toolUseWait)
	for {
		if u, ok := a.toolUses[id]; ok {
			switch {
			case u.requested:
				return "that tool call has already been asked about"
			case u.name != name || u.input != canonical(input):
				return "the request does not match the tool call"
			}
			u.requested = true
			return ""
		}
		left := time.Until(deadline)
		if left <= 0 {
			return "no such tool call in this turn"
		}
		if a.toolUseSeen == nil {
			a.toolUseSeen = make(chan struct{})
		}
		seen := a.toolUseSeen
		a.mu.Unlock()
		select {
		case <-seen:
		case <-time.After(left):
		case <-a.done:
			a.mu.Lock()
			return "the actor ended"
		}
		a.mu.Lock()
	}
}

func (a *Actor) requestPermission(args json.RawMessage) string {
	var p struct {
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	}
	if json.Unmarshal(args, &p) != nil || p.ToolName == "" {
		return denyText("malformed permission request")
	}
	a.mu.Lock()
	if a.turn == "" || a.thread == "" {
		a.mu.Unlock()
		return denyText("no active Pomar turn")
	}
	if a.bridgeWaiting >= maxBridgeWaiting {
		a.mu.Unlock()
		return denyText("too many waiting requests")
	}
	a.bridgeWaiting++
	turnAtStart := a.turn
	refusal := a.bindToolUse(p.ToolUseID, p.ToolName, p.Input)
	a.bridgeWaiting--
	if refusal == "" && a.turn != turnAtStart {
		refusal = "the turn ended"
	}
	if refusal != "" {
		a.mu.Unlock()
		return denyText(refusal)
	}
	a.nextPermission++
	// The actor's nonce keeps request IDs, and so the broker's permission
	// IDs, distinct across actors and incarnations (POMAR-CC SOW 06 finding 2).
	id, _ := json.Marshal(fmt.Sprintf("claude-permission-%s-%d", a.nonce, a.nextPermission))
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

// cancelPermissions denies every waiting call, and ends every waiting
// controller request unanswered; a.mu must be held.
func (a *Actor) cancelPermissions() {
	for id, w := range a.permissions {
		w.decision <- "cancel"
		delete(a.permissions, id)
	}
	for id, w := range a.controllers {
		w <- bridgeReply{Text: "the turn ended before the controller replied", IsError: true}
		delete(a.controllers, id)
	}
}

// The controller request tool (pomar.controller/v1). Claude Code calls
// mcp__pomar__pomar_controller_request; the MCP server forwards the call
// here. It is raised through the shared broker as the native request a Codex
// thread's dynamic tool makes (item/tool/call), journaled there, and answered
// by the controller through POST /v1/controller/requests/{id}/reply. The
// reply's text becomes the tool result. An MCP call carries no tool_use id,
// so a request is bound to a tool call Claude Code emitted in the current
// turn with the same name and input, each call once; its tool_use id is the
// request's call ID. Anything unbound is refused unrecorded.

// requestController raises one controller request and waits for its reply.
func (a *Actor) requestController(args json.RawMessage) bridgeReply {
	refuse := func(why string) bridgeReply { return bridgeReply{Text: why, IsError: true} }
	var p struct {
		Capability string `json:"capability"`
		Data       string `json:"data"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil || len(fields) != 2 || json.Unmarshal(args, &p) != nil || fields["capability"] == nil || fields["data"] == nil || len(p.Data) > controllerTextLimit {
		return refuse("malformed controller request")
	}
	if !slices.Contains(a.cfg.ControllerCapabilities, p.Capability) {
		return refuse("capability not configured for this environment")
	}
	a.mu.Lock()
	if a.turn == "" || a.thread == "" {
		a.mu.Unlock()
		return refuse("no active Pomar turn")
	}
	if a.bridgeWaiting >= maxBridgeWaiting {
		a.mu.Unlock()
		return refuse("too many waiting requests")
	}
	a.bridgeWaiting++
	turnAtStart := a.turn
	callID, refusal := a.bindToolUseByContent(ControllerToolName, args)
	a.bridgeWaiting--
	if refusal == "" && a.turn != turnAtStart {
		refusal = "the turn ended"
	}
	if refusal != "" {
		a.mu.Unlock()
		return refuse(refusal)
	}
	a.nextController++
	id, _ := json.Marshal(fmt.Sprintf("claude-controller-%s-%d", a.nonce, a.nextController))
	w := make(chan bridgeReply, 1)
	if a.controllers == nil {
		a.controllers = map[string]chan bridgeReply{}
	}
	a.controllers[string(id)] = w
	thread, turn := a.thread, a.turn
	a.mu.Unlock()

	arguments, _ := json.Marshal(map[string]string{"capability": p.Capability, "data": p.Data})
	params, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "callId": callID, "tool": ControllerTool, "arguments": json.RawMessage(arguments)})
	if err := a.observe(agentenv.Message{ID: id, Method: "item/tool/call", Params: params}); err != nil {
		a.mu.Lock()
		delete(a.controllers, string(id))
		a.mu.Unlock()
		return refuse("the request was not recorded")
	}
	select {
	case r := <-w:
		return r
	case <-a.done:
		return refuse("the actor ended before the controller replied")
	}
}

// answerController delivers the controller's reply, in the native shape the
// shared broker writes ({"success","contentItems":[{"type":"inputText",
// "text"}]}), to its waiting call. It reports false for an ID that is not a
// waiting controller request.
func (a *Actor) answerController(id json.RawMessage, result any) (bool, error) {
	a.mu.Lock()
	w, ok := a.controllers[string(id)]
	if ok {
		delete(a.controllers, string(id))
	}
	a.mu.Unlock()
	if !ok {
		return false, nil
	}
	var r struct {
		Success      *bool `json:"success"`
		ContentItems []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"contentItems"`
	}
	if remarshal(result, &r) != nil || r.Success == nil {
		w <- bridgeReply{Text: "the controller's reply was unreadable", IsError: true}
		return true, errors.New("claude actor: unreadable controller reply")
	}
	var text strings.Builder
	for _, c := range r.ContentItems {
		if c.Type == "inputText" {
			text.WriteString(c.Text)
		}
	}
	w <- bridgeReply{Text: text.String(), IsError: !*r.Success}
	return true, nil
}

// bindToolUseByContent waits for an unclaimed tool call in the current turn
// with this name and input, claims it and returns its id; a.mu must be held,
// and is held again on return.
func (a *Actor) bindToolUseByContent(name string, input json.RawMessage) (string, string) {
	want := canonical(input)
	if want == "" {
		return "", "malformed request"
	}
	deadline := time.Now().Add(toolUseWait)
	for {
		ids := make([]string, 0, len(a.toolUses))
		for id := range a.toolUses {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if u := a.toolUses[id]; !u.requested && u.name == name && u.input == want {
				u.requested = true
				return id, ""
			}
		}
		left := time.Until(deadline)
		if left <= 0 {
			return "", "no matching tool call in this turn"
		}
		if a.toolUseSeen == nil {
			a.toolUseSeen = make(chan struct{})
		}
		seen := a.toolUseSeen
		a.mu.Unlock()
		select {
		case <-seen:
		case <-time.After(left):
		case <-a.done:
			a.mu.Lock()
			return "", "the actor ended"
		}
		a.mu.Lock()
	}
}
