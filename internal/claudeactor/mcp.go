package claudeactor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
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

// Forward sends one tool call to the bridge socket and waits for its reply.
func Forward(socket string, tool string, args json.RawMessage) (bridgeReply, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return bridgeReply{}, err
	}
	defer c.Close()
	b, err := json.Marshal(bridgeRequest{Tool: tool, Arguments: args})
	if err != nil {
		return bridgeReply{}, err
	}
	if _, err = c.Write(append(b, '\n')); err != nil {
		return bridgeReply{}, err
	}
	var r bridgeReply
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return bridgeReply{}, err
	}
	return r, json.Unmarshal(line, &r)
}

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ServeMCP runs the MCP server on in/out until in ends. call forwards a tool
// call; ServeMCP lists exactly the permission tool.
func ServeMCP(in io.Reader, out io.Writer, call func(tool string, args json.RawMessage) (bridgeReply, error)) error {
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
	defer wg.Wait()
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
			reply(m.ID, map[string]any{"tools": []any{map[string]any{
				"name":        PermissionTool,
				"description": "Pomar's approval for an action Claude Code would take. The environment's controller decides.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"tool_name": map[string]string{"type": "string"},
					"input":     map[string]string{"type": "object"},
				}, "required": []string{"tool_name", "input"}},
			}}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil || p.Name != PermissionTool {
				fail(m.ID, -32602, "unknown tool")
				continue
			}
			// A call waits for the controller; others are served meanwhile.
			wg.Add(1)
			go func(id json.RawMessage, name string, args json.RawMessage) {
				defer wg.Done()
				r, err := call(name, args)
				if err != nil {
					// The bridge is unreachable: never an implicit approval.
					r = bridgeReply{Text: denyText("the Pomar bridge is unavailable"), IsError: false}
				}
				reply(id, map[string]any{"content": []any{map[string]string{"type": "text", "text": r.Text}}, "isError": r.IsError})
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

// MCPConfig is the --mcp-config JSON naming only Pomar's bridge: the guest
// binary's claude-mcp subcommand, pointed at the bridge socket.
func MCPConfig(guestBinary, socket string) (string, error) {
	b, err := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{
		"type": "stdio", "command": guestBinary, "args": []string{"claude-mcp", "-socket", socket},
	}}})
	return string(b), err
}
