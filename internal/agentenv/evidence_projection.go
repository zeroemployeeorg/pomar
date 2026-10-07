package agentenv

import "encoding/json"

// Lifecycle identity is evidence; arbitrary provider diagnostics are not a
// safe export surface. Retain their digest without storing message/data bytes.
// Item/tool content uses its existing contract and remains untrusted output.
func projectLifecycleEvidence(method string, raw json.RawMessage) (json.RawMessage, string) {
	if method != "error" && method != "turn/started" && method != "turn/completed" {
		return raw, ""
	}
	var fields struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     *struct {
			ID     string `json:"id"`
			Status string `json:"status,omitempty"`
		} `json:"turn,omitempty"`
	}
	// Store.Observe has already validated these identity fields and JSON.
	json.Unmarshal(raw, &fields)
	out := map[string]any{}
	if fields.ThreadID != "" {
		out["threadId"] = fields.ThreadID
	}
	if fields.TurnID != "" {
		out["turnId"] = fields.TurnID
	}
	if fields.Turn != nil {
		out["turn"] = fields.Turn
	}
	if method == "error" {
		var provider struct {
			Error struct {
				Code json.RawMessage `json:"code"`
			} `json:"error"`
		}
		json.Unmarshal(raw, &provider)
		diagnostic := map[string]any{"message": "provider error; private diagnostic details omitted"}
		var code *int64
		if len(provider.Error.Code) > 0 && json.Unmarshal(provider.Error.Code, &code) == nil && code != nil {
			diagnostic["code"] = *code
		}
		out["error"] = diagnostic
		var rawFields map[string]json.RawMessage
		json.Unmarshal(raw, &rawFields)
		var retry *bool
		if value, ok := rawFields["willRetry"]; ok && json.Unmarshal(value, &retry) == nil && retry != nil {
			out["willRetry"] = *retry
		}
	}
	projected, _ := json.Marshal(out)
	return projected, digest(raw)
}
