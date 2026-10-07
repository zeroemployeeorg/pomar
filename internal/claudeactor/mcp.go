package claudeactor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The MCP side of the bridge: a minimal MCP server on stdio that Claude Code
// starts (from --mcp-config) as the coding user. Each tool call is forwarded,
// one request per connection, to the actor's bridge socket in the root
// broker, which raises it through the shared broker and waits for the
// controller's answer. The server itself decides nothing.

// MCPServerName is the server name in --mcp-config; Claude Code exposes its
// tools as mcp__pomar__<tool>.
const MCPServerName = "pomar"

// PermissionTool is the --permission-prompt-tool Claude Code calls before an
// action that needs approval.
const PermissionTool = "permission"

// mcpProtocol is the MCP revision this server speaks; a client's own
// requested revision is echoed when it asks for one.
const mcpProtocol = "2025-06-18"

type bridgeRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type bridgeReply struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error"`
}

// ErrDeliveryUncertain is Forward's error when its request was written in full
// but no reply came back: the bridge may have raised it, and the controller
// may have answered it. It must not be reported as "not delivered", which
// invites a resend that would duplicate the effect (adviser note r43 §5.1).
// Any other Forward error means the request never reached the bridge: the
// bridge acts only on a complete line.
var ErrDeliveryUncertain = errors.New("the request reached the Pomar bridge but its reply was lost; delivery is uncertain")

// forwardReplyWait bounds how long Forward waits for its reply once the
// request is written. The bridge holds a request until the controller answers
// (RT's answer deadline is 30 minutes), so this is well above that.
var forwardReplyWait = time.Hour

// Forward sends one tool call to the bridge socket and waits for its reply.
func Forward(socket string, tool string, args json.RawMessage) (bridgeReply, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return bridgeReply{}, fmt.Errorf("not delivered: %w", err)
	}
	defer c.Close()
	b, err := json.Marshal(bridgeRequest{Tool: tool, Arguments: args})
	if err != nil {
		return bridgeReply{}, fmt.Errorf("not delivered: %w", err)
	}
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err = c.Write(append(b, '\n')); err != nil {
		return bridgeReply{}, fmt.Errorf("not delivered: %w", err)
	}
	c.SetReadDeadline(time.Now().Add(forwardReplyWait))
	var r bridgeReply
	line, err := readLine(c)
	if err != nil || json.Unmarshal(line, &r) != nil {
		return bridgeReply{}, ErrDeliveryUncertain
	}
	return r, nil
}

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ControllerTool is the controller request tool (pomar.controller/v1), the
// same name and schema the shared broker gives a Codex thread as a dynamic
// tool. It is listed only when the environment configures capabilities.
const ControllerTool = "pomar_controller_request"

// controllerTextLimit is the shared broker's bound on request and reply text.
const controllerTextLimit = 32 << 10

// maxMCPInFlight bounds tool calls the MCP server serves at once; the bridge
// itself refuses beyond maxBridgeWaiting outstanding requests.
const maxMCPInFlight = 2 * maxBridgeWaiting

// maxRecordedCalls bounds the completed tool calls whose outcome a reused
// request ID is answered with.
const maxRecordedCalls = 256

// recordedCall is one tool call's input and, once done is closed, its reply.
type recordedCall struct {
	input  string
	done   chan struct{}
	result map[string]any
}

// ServeMCP runs the MCP server on in/out until in ends. call forwards a tool
// call; ServeMCP lists exactly the permission tool.
func ServeMCP(in io.Reader, out io.Writer, call func(tool string, args json.RawMessage) (bridgeReply, error)) error {
	return ServeMCPController(in, out, nil, call)
}

// ServeMCPController is ServeMCP that also lists the controller request tool
// for the environment's configured capabilities, when there are any.
func ServeMCPController(in io.Reader, out io.Writer, capabilities []string, call func(tool string, args json.RawMessage) (bridgeReply, error)) error {
	var mu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		out.Write(append(b, '\n'))
		mu.Unlock()
	}
	reply := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(id json.RawMessage, code int, msg string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 4096), 4<<20)
	var wg sync.WaitGroup
	var inFlight atomic.Int32
	defer wg.Wait()
	// Tool calls by JSON-RPC request ID, for this server's one client
	// connection: the Claude Code process that started it. A reconnection is
	// a new process and a new server, so IDs never span connections. A
	// reused ID with the same tool and input is answered with the one
	// recorded outcome, waiting for it if still in flight, and is never
	// dispatched twice. A reused ID with other input is refused. Completed
	// calls are kept for the last maxRecordedCalls IDs.
	var callsMu sync.Mutex
	calls := map[string]*recordedCall{}
	var completed []string
	for s.Scan() {
		var m mcpMessage
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			return fmt.Errorf("mcp: unreadable message: %w", err)
		}
		if len(m.ID) == 0 {
			continue // notifications (initialized, cancelled) need no reply
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(m.Params, &p)
			version := mcpProtocol
			if p.ProtocolVersion != "" {
				version = p.ProtocolVersion
			}
			reply(m.ID, map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": MCPServerName, "version": "0.1.0"},
			})
		case "ping":
			reply(m.ID, map[string]any{})
		case "tools/list":
			tools := []any{map[string]any{
				"name":        PermissionTool,
				"description": "Pomar's approval for an action Claude Code would take. The environment's controller decides.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"tool_name": map[string]string{"type": "string"},
					"input":     map[string]string{"type": "object"},
				}, "required": []string{"tool_name", "input"}},
			}}
			if len(capabilities) > 0 {
				tools = append(tools, map[string]any{
					"name":        ControllerTool,
					"description": "Request a configured controller capability with bounded data. Delivery is not an acknowledgement. Never include credentials, host commands, socket paths or replacement recipient identities.",
					"inputSchema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"capability", "data"}, "properties": map[string]any{
						"capability": map[string]any{"type": "string", "enum": capabilities},
						"data":       map[string]any{"type": "string", "maxLength": controllerTextLimit},
					}},
				})
			}
			reply(m.ID, map[string]any{"tools": tools})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil || !(p.Name == PermissionTool || (p.Name == ControllerTool && len(capabilities) > 0)) {
				fail(m.ID, -32602, "unknown tool")
				continue
			}
			// A call waits for the controller; others are served meanwhile,
			// up to a bound on calls in flight.
			if inFlight.Add(1) > maxMCPInFlight {
				inFlight.Add(-1)
				fail(m.ID, -32000, "too many tool calls in flight")
				continue
			}
			idKey, input := canonical(m.ID), p.Name+"\x00"+canonical(p.Arguments)
			if idKey == "" {
				idKey = string(m.ID)
			}
			callsMu.Lock()
			rec, reused := calls[idKey]
			if reused && rec.input != input {
				callsMu.Unlock()
				inFlight.Add(-1)
				fail(m.ID, -32600, "request id reused with different input")
				continue
			}
			if !reused {
				rec = &recordedCall{input: input, done: make(chan struct{})}
				calls[idKey] = rec
			}
			callsMu.Unlock()
			wg.Add(1)
			if reused {
				// The same call again: its one outcome, never a second dispatch.
				go func(id json.RawMessage) {
					defer wg.Done()
					defer inFlight.Add(-1)
					<-rec.done
					reply(id, rec.result)
				}(m.ID)
				continue
			}
			go func(id json.RawMessage, name string, args json.RawMessage) {
				defer wg.Done()
				defer inFlight.Add(-1)
				r, err := call(name, args)
				switch {
				case errors.Is(err, ErrDeliveryUncertain):
					// The bridge may have raised it and the controller may
					// have answered: never a resend, and never an approval.
					r = bridgeReply{Text: denyText(ErrDeliveryUncertain.Error() + "; do not repeat it")}
					if name == ControllerTool {
						r = bridgeReply{Text: ErrDeliveryUncertain.Error() + ". Do not repeat this request: the controller can inspect the original.", IsError: true}
					}
				case err != nil:
					// The request never reached the bridge: never an implicit
					// approval, and never a controller reply.
					r = bridgeReply{Text: denyText("the Pomar bridge is unavailable"), IsError: false}
					if name == ControllerTool {
						r = bridgeReply{Text: "the Pomar bridge is unavailable; the request was not delivered", IsError: true}
					}
				}
				rec.result = map[string]any{"content": []any{map[string]string{"type": "text", "text": r.Text}}, "isError": r.IsError}
				close(rec.done)
				callsMu.Lock()
				completed = append(completed, idKey)
				if len(completed) > maxRecordedCalls {
					delete(calls, completed[0])
					completed = completed[1:]
				}
				callsMu.Unlock()
				reply(id, rec.result)
			}(m.ID, p.Name, p.Arguments)
		default:
			fail(m.ID, -32601, "method not found")
		}
	}
	return s.Err()
}

// The permission tool's reply is a JSON object in the result text:
// {"behavior":"allow","updatedInput":{...}} or {"behavior":"deny","message":...}.
func allowText(input json.RawMessage) string {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(map[string]any{"behavior": "allow", "updatedInput": input})
	return string(b)
}

func denyText(message string) string {
	b, _ := json.Marshal(map[string]string{"behavior": "deny", "message": message})
	return string(b)
}

// BridgeReply is a bridge call's reply: the tool result text.
type BridgeReply = bridgeReply

// PermissionToolName is the --permission-prompt-tool value.
const PermissionToolName = "mcp__" + MCPServerName + "__" + PermissionTool

// ControllerToolName is the controller tool as Claude Code names it, for
// --allowedTools: the call itself is the controlled exchange, so it is not
// also put to the permission tool.
const ControllerToolName = "mcp__" + MCPServerName + "__" + ControllerTool

// MCPConfig is the --mcp-config JSON naming only Pomar's bridge: the guest
// binary's claude-mcp subcommand, pointed at the bridge socket, listing the
// controller tool for the given capabilities, if any.
func MCPConfig(guestBinary, socket string, capabilities ...string) (string, error) {
	args := []string{"claude-mcp", "-socket", socket}
	if len(capabilities) > 0 {
		args = append(args, "-controller-capabilities", strings.Join(capabilities, ","))
	}
	b, err := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{
		"type": "stdio", "command": guestBinary, "args": args,
	}}})
	return string(b), err
}
