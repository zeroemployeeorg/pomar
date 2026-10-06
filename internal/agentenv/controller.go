package agentenv

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"unicode/utf8"
)

const controllerTool = "pomar_controller_request"
const controllerTextLimit = 32 << 10

var capabilityName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// These capabilities name owner-configured controller functions, never host
// commands or recipient addresses. The broker only transports bounded text.
func ValidateControllerCapabilities(names []string) error {
	if len(names) > 16 {
		return errors.New("too many controller capabilities")
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !capabilityName.MatchString(name) || seen[name] {
			return errors.New("invalid controller capability")
		}
		seen[name] = true
	}
	return nil
}

// ConfigureController is startup-only. A retained thread's tool configuration
// is immutable: adding tools requires a new explicitly owned environment.
func (b *Broker) ConfigureController(names []string) error {
	if err := ValidateControllerCapabilities(names); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Actor != nil {
		return errors.New("controller configuration requires an uninitialized broker")
	}
	m := b.Store
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(b.incarnation); err != nil {
		return err
	}
	if m.s.ThreadID != "" && !slices.Equal(names, m.s.ControllerCapabilities) {
		return ErrConflict
	}
	m.s.ControllerCapabilities = append([]string(nil), names...)
	return m.save()
}

func controllerTools(names []string) []map[string]any {
	return []map[string]any{{"type": "function", "name": controllerTool, "description": "Request a configured controller capability with bounded data. Delivery is not an acknowledgement. Never include credentials, host commands, socket paths or replacement recipient identities.", "inputSchema": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"capability", "data"}, "properties": map[string]any{
			"capability": map[string]any{"type": "string", "enum": names}, "data": map[string]any{"type": "string", "maxLength": controllerTextLimit},
		}}}}
}

type ControllerBinding struct {
	EnvironmentID   string          `json:"environment_id"`
	SessionID       string          `json:"session_id"`
	Incarnation     string          `json:"incarnation"`
	OperationID     string          `json:"operation_id"`
	InputSHA256     string          `json:"input_sha256"`
	ThreadID        string          `json:"thread_id"`
	TurnID          string          `json:"turn_id"`
	NativeRequestID json.RawMessage `json:"native_request_id"`
	CallID          string          `json:"call_id"`
}

type ControllerRequest struct {
	ID                 string            `json:"request_id"`
	Binding            ControllerBinding `json:"binding"`
	Capability         string            `json:"capability"`
	Data               string            `json:"data"`
	NativeParamsSHA256 string            `json:"native_params_sha256"`
	State              string            `json:"state"`
	Reply              *ControllerReply  `json:"reply,omitempty"`
	ReplySHA256        string            `json:"reply_sha256,omitempty"`
}

type ControllerReply struct {
	ReplyID string            `json:"reply_id"`
	Binding ControllerBinding `json:"binding"`
	Text    string            `json:"text"`
	Success bool              `json:"success"`
}

func nativeReply(reply ControllerReply) any {
	return map[string]any{"success": reply.Success, "contentItems": []map[string]string{{"type": "inputText", "text": reply.Text}}}
}

func strictJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validNativeID(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > 256 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s != "" && len(s) <= 128
	}
	var n int64
	return json.Unmarshal(raw, &n) == nil
}

// Caller holds Broker.mu. A request is accepted only for a known active turn;
// turn/started provides that binding even if turn/start's reply is still in flight.
func (b *Broker) observeControllerRequest(message Message) error {
	if !b.controllerReady || b.resumeFailure != nil || !validNativeID(message.ID) || len(message.Params) > 48<<10 {
		return errors.New("controller request unsupported or unbound")
	}
	// JSON-RPC IDs bind decoded values, not alternative string escape spellings.
	var decodedID any
	decoder := json.NewDecoder(bytes.NewReader(message.ID))
	decoder.UseNumber()
	if decoder.Decode(&decodedID) != nil {
		return errors.New("invalid native request id")
	}
	message.ID, _ = json.Marshal(decodedID)
	var p struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		CallID    string          `json:"callId"`
		Tool      string          `json:"tool"`
		Namespace *string         `json:"namespace"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if strictJSON(message.Params, &p) != nil || p.Tool != controllerTool || (p.Namespace != nil && *p.Namespace != "") || p.CallID == "" || len(p.CallID) > 128 {
		return errors.New("invalid native controller request")
	}
	var args struct {
		Capability string `json:"capability"`
		Data       string `json:"data"`
	}
	if strictJSON(p.Arguments, &args) != nil || len(args.Data) > controllerTextLimit || !utf8.ValidString(args.Data) {
		return errors.New("invalid controller arguments")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(p.Arguments, &fields) != nil || len(fields) != 2 || fields["data"] == nil {
		return errors.New("controller arguments require capability and data")
	}
	if bytes.Equal(fields["data"], []byte("null")) {
		return errors.New("controller data must be a string")
	}
	m := b.Store
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(b.incarnation); err != nil {
		return err
	}
	if !slices.Contains(m.s.ControllerCapabilities, args.Capability) || p.ThreadID == "" || p.ThreadID != m.s.ThreadID || p.TurnID == "" {
		return errors.New("controller capability or thread unavailable")
	}
	var operation Operation
	for _, op := range m.s.Operations {
		if op.Incarnation == b.incarnation && op.ThreadID == p.ThreadID && op.TurnID == p.TurnID && op.Completion == "" {
			operation = op
			break
		}
	}
	if operation.ID == "" {
		return ErrStale
	}
	binding := ControllerBinding{m.s.EnvironmentID, m.s.SessionID, b.incarnation, operation.ID, operation.InputHash, p.ThreadID, p.TurnID, append(json.RawMessage(nil), message.ID...), p.CallID}
	raw, _ := json.Marshal(binding)
	id := digest(raw)
	request := ControllerRequest{ID: id, Binding: binding, Capability: args.Capability, Data: args.Data, NativeParamsSHA256: digest(message.Params), State: "pending"}
	// Native request IDs and call IDs cannot be rebound to another operation or payload.
	for existingID, existing := range m.s.ControllerRequests {
		if existing.Binding.Incarnation == b.incarnation && (bytes.Equal(existing.Binding.NativeRequestID, message.ID) || existing.Binding.CallID == p.CallID) {
			if existingID != id || existing.NativeParamsSHA256 != request.NativeParamsSHA256 {
				return ErrConflict
			}
			return nil // Duplicate observation cannot reopen or redeliver a reply.
		}
	}
	if len(m.s.ControllerRequests) >= 1024 {
		return errors.New("controller request limit reached")
	}
	m.s.ControllerRequests[id] = request
	if err := m.save(); err != nil {
		return err
	}
	b.controllerPending[id] = append(json.RawMessage(nil), message.ID...)
	return nil
}

// ReplyController syncs identity, content and digest before any native write.
// Successful writes are transport evidence only, never recipient acknowledgements.
func (b *Broker) ReplyController(id string, reply ControllerReply) (ControllerRequest, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.Store
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(reply.Binding.Incarnation); err != nil {
		return ControllerRequest{}, err
	}
	request, ok := m.s.ControllerRequests[id]
	if !ok {
		return ControllerRequest{}, errors.New("unknown controller request")
	}
	expected, _ := json.Marshal(request.Binding)
	actual, _ := json.Marshal(reply.Binding)
	if !bytes.Equal(expected, actual) || b.incarnation != reply.Binding.Incarnation {
		return request, ErrStale
	}
	if !validID.MatchString(reply.ReplyID) || len(reply.Text) > controllerTextLimit || !utf8.ValidString(reply.Text) {
		return request, errors.New("invalid controller reply")
	}
	payload, _ := json.Marshal(nativeReply(reply))
	hash := digest(payload)
	if request.Reply != nil {
		if request.Reply.ReplyID != reply.ReplyID || request.ReplySHA256 != hash {
			return request, ErrConflict
		}
		return request, nil // Retained outcome, including unknown acceptance: no replay.
	}
	for _, existing := range m.s.ControllerRequests {
		if existing.Reply != nil && existing.Reply.ReplyID == reply.ReplyID {
			return request, ErrConflict
		}
	}
	op := m.s.Operations[request.Binding.OperationID]
	nativeID, live := b.controllerPending[id]
	if !b.controllerReady || b.resumeFailure != nil || !live || op.Completion != "" || op.Incarnation != b.incarnation || op.TurnID != request.Binding.TurnID {
		return request, ErrStale
	}
	request.Reply = &reply
	request.ReplySHA256 = hash
	request.State = "dispatching"
	m.s.ControllerRequests[id] = request
	if err := m.save(); err != nil {
		return request, err
	}
	delete(b.controllerPending, id)
	err := b.Actor.Answer(nativeID, nativeReply(reply))
	request.State = "written"
	if err != nil {
		request.State = "acceptance_unknown"
	}
	m.s.ControllerRequests[id] = request
	if persistErr := m.save(); persistErr != nil {
		return request, persistErr
	}
	return request, nil // An uncertain native write is inspectable, not retried.
}

func (b *Broker) controllerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/controller/requests", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		ready := b.controllerReady
		b.mu.Unlock()
		state := b.Store.Snapshot()
		respond(w, 200, map[string]any{"contract": "pomar.controller/v1", "native_method": "item/tool/call", "adapter_version": "0.160.0", "supported": ready, "configured_capabilities": state.ControllerCapabilities, "requests": state.ControllerRequests})
	})
	mux.HandleFunc("POST /v1/controller/requests/{id}/reply", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		var reply ControllerReply
		if err != nil || strictJSON(raw, &reply) != nil {
			respond(w, 400, map[string]string{"error": "invalid controller reply"})
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != 4 || fields["reply_id"] == nil || fields["binding"] == nil || fields["text"] == nil || fields["success"] == nil || bytes.Equal(fields["text"], []byte("null")) || bytes.Equal(fields["success"], []byte("null")) {
			respond(w, 400, map[string]string{"error": "all controller reply fields are required"})
			return
		}
		request, err := b.ReplyController(r.PathValue("id"), reply)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, request)
	})
}
