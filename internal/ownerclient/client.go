// Package ownerclient retains the identity and outcome of bounded owner calls.
// A lost transport is uncertainty, never permission to invent a replacement ID.
package ownerclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/zeroemployeeorg/pomar/internal/calljournal"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
)

const responseLimit = 8 << 20

var id = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var ErrUncertain = errors.New("acceptance unknown; reconcile this original operation before retrying")
var ErrConflict = errors.New("operation identity already binds different input or action")

type Config struct {
	Root, Records, Environment, Action, OperationID, Request, Session, Incarnation string
	Reconcile, RetryKnownUnaccepted, ConsumeRequest                                bool
}

type Intent struct {
	Action, Environment, OperationID, RequestSHA256, Socket, Session, Incarnation string
}

// Receipt is ordinary diagnostic metadata. Response/request/provider bytes are
// kept only in the separately protected files, never in this structure.
type Receipt struct {
	Action                    string `json:"action"`
	OperationID               string `json:"operation_id"`
	UID                       int    `json:"euid"`
	HTTPStatus                int    `json:"http_status"`
	Outcome                   string `json:"outcome"`
	Cached                    bool   `json:"cached"`
	Transport                 string `json:"transport_outcome"`
	OperationState            string `json:"operation_state"`
	SessionState              string `json:"session_state"`
	ResponseAvailable         bool   `json:"response_available"`
	RetainedResponseAvailable bool   `json:"retained_response_available"`
	RetainedResponseEventID   string `json:"retained_response_event_id,omitempty"`
	EventID                   string `json:"event_id,omitempty"`
	ObservedAt                string `json:"observed_at,omitempty"`
}

type response struct {
	Intent Intent
	Status int
	Kind   string
	Body   []byte
}

func route(c Config) (string, string, error) {
	if !id.MatchString(c.Environment) || !id.MatchString(c.OperationID) {
		return "", "", errors.New("bounded environment and operation identifiers required")
	}
	base := "/v1/environments/" + c.Environment
	switch c.Action {
	case "inspect":
		return "GET", base, nil
	case "session":
		return "GET", base + "/agent/session", nil
	case "operation", "agent-operation":
		if !id.MatchString(c.Session) || !id.MatchString(c.Incarnation) {
			return "", "", errors.New("original session and incarnation required")
		}
		if c.Action == "agent-operation" {
			base += "/agent"
		}
		q := url.Values{"session_id": {c.Session}, "expected_incarnation": {c.Incarnation}}
		return "GET", base + "/operations/" + c.OperationID + "?" + q.Encode(), nil
	case "start", "stop":
		return "POST", base + "/" + c.Action, nil
	case "login":
		return "POST", base + "/agent/login", nil
	case "login-complete":
		return "POST", base + "/agent/login/complete", nil
	default:
		return "", "", errors.New("unsupported owner action")
	}
}

func saveRecord(path string, value any) error { return calljournal.Save(path, value) }

func call(client *http.Client, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, "http://owner"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, ErrUncertain
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := client.Do(req)
	if err != nil {
		return 0, nil, ErrUncertain
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, responseLimit+1))
	if err != nil || len(b) > responseLimit {
		return 0, nil, ErrUncertain
	}
	return r.StatusCode, b, nil
}

// Run performs at most one mutation. A second invocation returns a recorded
// response locally, or requires explicit read-only reconciliation. Only a fresh
// durable_non_acceptance observation and explicit retry flag permit resubmission
// of the exact same original ID and bytes. No login input is journaled.
func Run(c Config) (Receipt, error) {
	meta := Receipt{Action: c.Action, OperationID: c.OperationID, UID: os.Geteuid(), Outcome: "local-refusal", Transport: "not-contacted", OperationState: "unknown", SessionState: "not-inspected"}
	method, path, err := route(c)
	if err != nil {
		return meta, err
	}
	if err = localclient.PrivateDir(c.Root); err != nil {
		return meta, err
	}
	if err = localclient.PrivateDir(c.Records); err != nil {
		return meta, err
	}
	var body []byte
	if method == "POST" && c.Request != "" {
		body, err = localclient.ReadPrivate(c.Request, 64<<10)
		if err != nil {
			return meta, err
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			return meta, errors.New("invalid private request")
		}
		allowed := map[string]bool{"operation_id": true, "expected_incarnation": true}
		if c.Action == "login-complete" {
			allowed["login_id"] = true
			allowed["code"] = true
		}
		for k := range fields {
			if !allowed[k] {
				return meta, errors.New("unsupported private request field")
			}
		}
		var op, inc string
		if json.Unmarshal(fields["operation_id"], &op) != nil || json.Unmarshal(fields["expected_incarnation"], &inc) != nil || op != c.OperationID || !id.MatchString(inc) || inc != c.Incarnation || !id.MatchString(c.Session) {
			return meta, errors.New("private request must bind the original operation, session and incarnation")
		}
	} else if method != "POST" && c.Request != "" {
		return meta, errors.New("read-only actions do not take a request body")
	}
	sum := sha256.Sum256(body)
	requestHash := hex.EncodeToString(sum[:])
	if method == "POST" && c.Request == "" {
		prior, e := localclient.ReadPrivate(filepath.Join(c.Records, c.OperationID, "intent.json"), 64<<10)
		var old Intent
		if e != nil || json.Unmarshal(prior, &old) != nil || len(old.RequestSHA256) != 64 {
			return meta, errors.New("original private request required for first dispatch")
		}
		requestHash = old.RequestSHA256
	}
	intent := Intent{Action: c.Action, Environment: c.Environment, OperationID: c.OperationID, RequestSHA256: requestHash, Socket: filepath.Join(c.Root, "host.sock"), Session: c.Session, Incarnation: c.Incarnation}
	dir := filepath.Join(c.Records, c.OperationID)
	if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return meta, err
	}
	if err = localclient.PrivateDir(dir); err != nil {
		return meta, err
	}
	fd, err := syscall.Open(filepath.Join(dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return meta, errors.New("operation lock unavailable")
	}
	lock := os.NewFile(uintptr(fd), "lock")
	defer lock.Close()
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return meta, errors.New("original operation already has an active client")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	expectedIntent, _ := json.Marshal(intent)
	if err = calljournal.Complete(filepath.Join(dir, "intent.json"), expectedIntent); err != nil {
		return meta, err
	}
	prior, err := localclient.ReadPrivate(filepath.Join(dir, "intent.json"), 64<<10)
	existing := err == nil
	if existing {
		var old Intent
		if json.Unmarshal(prior, &old) != nil || old != intent {
			return meta, ErrConflict
		}
	} else {
		if _, e := os.Lstat(filepath.Join(dir, "intent.json")); !os.IsNotExist(e) {
			return meta, errors.New("original intent unreadable; preserve and inspect")
		}
		if err = saveRecord(filepath.Join(dir, "intent.json"), intent); err != nil {
			return meta, err
		}
	}
	retainedResponse := func() error {
		ev, found, e := calljournal.LatestKind(dir, "response")
		if e != nil {
			return e
		}
		meta.RetainedResponseAvailable = found
		meta.RetainedResponseEventID = ev.ID
		if found {
			var saved response
			if json.Unmarshal(ev.Value, &saved) != nil || saved.Intent != intent {
				return ErrConflict
			}
		}
		return nil
	}
	finish := func(saved response, cached bool, ev calljournal.Event) (Receipt, error) {
		if e := retainedResponse(); e != nil {
			meta.Outcome = "uncertain"
			return meta, e
		}
		meta.HTTPStatus = saved.Status
		meta.Cached = cached
		meta.EventID = ev.ID
		meta.ObservedAt = ev.At
		meta.SessionState = "not-inspected"
		meta.Transport = "response"
		meta.ResponseAvailable = saved.Kind == "response"
		meta.OperationState = operationState(saved.Body, c.OperationID)
		if method == "GET" {
			if saved.Status >= 200 && saved.Status < 300 {
				meta.Outcome = "snapshot"
				return meta, nil
			}
			meta.Outcome = "refused"
			return meta, errors.New("owner HTTP refusal; response retained privately")
		}
		if saved.Status >= 500 || saved.Status == 409 || meta.OperationState == "dispatching" || meta.OperationState == "acceptance_unknown" {
			meta.Outcome = "uncertain"
			return meta, ErrUncertain
		}
		if saved.Status < 200 || saved.Status >= 300 {
			meta.Outcome = "refused"
			return meta, errors.New("owner HTTP refusal; response retained privately")
		}
		if c.Action == "login" || c.Action == "login-complete" {
			var out struct {
				Login json.RawMessage `json:"login"`
			}
			json.Unmarshal(saved.Body, &out)
			// A retained control only proves dispatch, not the missing one-time result.
			if saved.Kind == "response" && len(out.Login) > 0 && string(out.Login) != "null" {
				if c.Action == "login-complete" {
					var result struct {
						Success *bool `json:"success"`
					}
					if json.Unmarshal(out.Login, &result) != nil || result.Success == nil {
						meta.Outcome = "uncertain"
						return meta, ErrUncertain
					}
					if !*result.Success {
						meta.Outcome = "refused"
						return meta, errors.New("original login completion refused")
					}
				}
				meta.Outcome = "response-available"
				if c.ConsumeRequest && c.Action == "login-complete" && c.Request != "" {
					if err := os.Remove(c.Request); err != nil {
						return meta, errors.New("response retained; private request cleanup failed")
					}
				}
				return meta, nil
			}
			meta.Outcome = "original-operation-observed"
			return meta, ErrUncertain
		}
		if meta.OperationState == "completed" {
			meta.Outcome = "completed"
			return meta, nil
		}
		meta.Outcome = "uncertain"
		return meta, ErrUncertain
	}
	if e := calljournal.Recover(dir, func(ev calljournal.Event) bool {
		switch ev.Kind {
		case "response", "reconciliation", "dispatch", "transport-uncertain":
			var value struct{ Intent Intent }
			return json.Unmarshal(ev.Value, &value) == nil && value.Intent == intent
		}
		return false
	}); e != nil {
		meta.Outcome = "uncertain"
		return meta, e
	}
	if e := retainedResponse(); e != nil {
		meta.Outcome = "uncertain"
		return meta, e
	}
	latest, found, e := calljournal.Latest(dir)
	if e != nil {
		meta.Outcome = "uncertain"
		meta.OperationState = "acceptance_unknown"
		return meta, e
	}
	if found && !c.Reconcile {
		if latest.Kind == "response" || latest.Kind == "reconciliation" {
			var saved response
			if json.Unmarshal(latest.Value, &saved) != nil || saved.Intent != intent {
				return meta, ErrConflict
			}
			return finish(saved, true, latest)
		}
		meta.Transport = "lost-or-unrecorded"
		meta.OperationState = "acceptance_unknown"
		meta.Outcome = "uncertain"
		return meta, ErrUncertain
	}
	// Read legacy evidence without changing it. Explicit reconciliation always
	// bypasses historical observations and obtains a fresh server observation.
	if !found && !c.Reconcile {
		for _, name := range []string{"reconciliation.json", "response.json"} {
			data, e := localclient.ReadPrivate(filepath.Join(dir, name), calljournal.Limit)
			if e == nil {
				var saved response
				if json.Unmarshal(data, &saved) != nil || saved.Intent != intent {
					return meta, ErrConflict
				}
				saved.Kind = "response"
				if name == "reconciliation.json" {
					saved.Kind = "reconciliation"
				}
				return finish(saved, true, calljournal.Event{})
			}
			if _, e = os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(e) {
				return meta, errors.New("legacy response unreadable; preserve and inspect")
			}
		}
	}
	client := localclient.New(intent.Socket, os.Geteuid())
	defer client.CloseIdleConnections()
	exchange := func(kind, verb, target string, payload []byte) (response, calljournal.Event, error) {
		if _, err := calljournal.Append(dir, "dispatch", struct {
			Intent       Intent
			Method, Path string
		}{intent, verb, target}); err != nil {
			return response{}, calljournal.Event{}, err
		}
		status, b, e := call(client, verb, target, payload)
		saved := response{Intent: intent, Status: status, Body: b, Kind: kind}
		if e != nil {
			meta.Transport = "lost-or-unrecorded"
			meta.OperationState = "acceptance_unknown"
			meta.Outcome = "uncertain"
			calljournal.Append(dir, "transport-uncertain", struct{ Intent Intent }{intent})
			return saved, calljournal.Event{}, ErrUncertain
		}
		ev, e := calljournal.Append(dir, kind, saved)
		if e != nil {
			meta.Transport = "response-not-durable"
			meta.HTTPStatus = status
			meta.Outcome = "uncertain"
			meta.OperationState = "acceptance_unknown"
			return saved, ev, ErrUncertain
		}
		return saved, ev, nil
	}
	if existing && method == "POST" {
		if !c.Reconcile {
			meta.Outcome = "uncertain"
			return meta, ErrUncertain
		}
		inspect := c
		inspect.Action = "operation"
		if c.Action == "login" || c.Action == "login-complete" {
			inspect.Action = "agent-operation"
		}
		_, p, _ := route(inspect)
		saved, ev, e := exchange("reconciliation", "GET", p, nil)
		if e != nil {
			return meta, e
		}
		var evidence struct {
			Evidence string `json:"evidence"`
		}
		json.Unmarshal(saved.Body, &evidence)
		if saved.Status == 404 && evidence.Evidence == "durable_non_acceptance" {
			meta.HTTPStatus = 404
			meta.OperationState = "not-accepted"
			meta.Outcome = "durable-non-acceptance"
			meta.EventID = ev.ID
			meta.ObservedAt = ev.At
			meta.Transport = "response"
			meta.SessionState = "not-inspected"
			if !c.RetryKnownUnaccepted {
				return meta, errors.New("fresh durable non-acceptance; explicit same-operation retry required")
			}
		} else {
			return finish(saved, false, ev)
		}
	}
	if method == "POST" && len(body) == 0 {
		meta.Outcome = "uncertain"
		return meta, errors.New("same-byte original request required for retry; no replacement minted")
	}
	saved, ev, e := exchange("response", method, path, body)
	if e != nil {
		return meta, e
	}
	return finish(saved, false, ev)
}

// Extract only the original action/control state, never the environment phase
// or current authentication. Unknown shapes remain unresolved.
func operationState(body []byte, op string) string {
	var v struct {
		State  string `json:"state"`
		Action struct {
			State string `json:"state"`
		} `json:"action"`
		Operation struct {
			State string `json:"state"`
		} `json:"operation"`
		Actions map[string]struct {
			State string `json:"state"`
		} `json:"actions"`
	}
	if json.Unmarshal(body, &v) != nil {
		return "unknown"
	}
	if v.Action.State != "" {
		return v.Action.State
	}
	if v.Operation.State != "" {
		return v.Operation.State
	}
	if a, ok := v.Actions[op]; ok {
		return a.State
	}
	if v.State != "" {
		return v.State
	}
	return "unknown"
}

// RetainedResponse returns the HTTP status and body of an operation's latest
// durably retained response, after Run has reported one available. It reads
// only: a reply that was never retained can't be recovered, and none is ever
// recreated.
func RetainedResponse(records, operationID string) (int, []byte, error) {
	if !id.MatchString(operationID) {
		return 0, nil, errors.New("bounded operation identifier required")
	}
	if err := localclient.PrivateDir(records); err != nil {
		return 0, nil, err
	}
	ev, found, err := calljournal.LatestKind(filepath.Join(records, operationID), "response")
	if err != nil {
		return 0, nil, err
	}
	if !found {
		return 0, nil, errors.New("no retained response")
	}
	var saved response
	if json.Unmarshal(ev.Value, &saved) != nil || saved.Intent.OperationID != operationID {
		return 0, nil, ErrConflict
	}
	return saved.Status, saved.Body, nil
}
