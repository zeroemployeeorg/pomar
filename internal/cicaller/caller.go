// Package cicaller implements a fixed class-scoped caller on the existing
// protected control socket. It never opens the manager's private owner socket.
package cicaller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/calljournal"
	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/manager"
)

const maxWire = 48 << 20

var attemptID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var sha = regexp.MustCompile(`^[a-f0-9]{40}$`)

type Policy struct {
	CallerUID      int      `json:"caller_uid"`
	ManagerUID     int      `json:"manager_uid"`
	Socket         string   `json:"socket"`
	Records        string   `json:"records"`
	Class          string   `json:"class"`
	Mirror         string   `json:"mirror"`
	Ref            string   `json:"ref"`
	SourceGit      bool     `json:"source_git"`
	SourceReadOnly bool     `json:"source_readonly"`
	SourceBase     string   `json:"source_base"`
	SourceSelfBase bool     `json:"source_self_base"`
	Command        []string `json:"command"`
	Inputs         []string `json:"inputs"`
	Outputs        []string `json:"outputs"`
}
type Request struct {
	Action               string                `json:"action"`
	Attempt              string                `json:"attempt_id,omitempty"`
	Output               string                `json:"output,omitempty"`
	Start                *manager.StartRequest `json:"start,omitempty"`
	RetryKnownUnaccepted bool                  `json:"retry_known_unaccepted,omitempty"`
}
type Reply struct {
	CallerUID                 int    `json:"caller_uid"`
	Status                    int    `json:"http_status"`
	RequestSHA256             string `json:"request_sha256"`
	Observation               bool   `json:"observation"`
	Body                      []byte `json:"body"`
	Outcome                   string `json:"outcome"`
	OperationState            string `json:"operation_state"`
	Transport                 string `json:"transport_outcome"`
	SessionState              string `json:"session_state"`
	ResponseAvailable         bool   `json:"response_available"`
	RetainedResponseAvailable bool   `json:"retained_response_available"`
	RetainedResponseEventID   string `json:"retained_response_event_id,omitempty"`
	Cached                    bool   `json:"cached"`
	EventID                   string `json:"event_id,omitempty"`
	ObservedAt                string `json:"observed_at,omitempty"`
}

// ReadPolicy reads only root-owned, non-writable, non-symlink public
// configuration. The effective UID chooses the filename; no argv or environment
// variable can select another policy, socket, command or identity.
func ReadPolicy(path string) (Policy, error) {
	var p Policy
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil {
			return p, errors.New("caller policy directory unavailable")
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || !fi.IsDir() || st.Uid != 0 || fi.Mode().Perm()&0022 != 0 {
			return p, errors.New("caller policy directory custody refused")
		}
		if dir == "/" {
			break
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return p, errors.New("caller policy unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return p, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || st.Uid != 0 || st.Nlink != 1 || fi.Mode().Perm()&0022 != 0 || fi.Size() > 16<<10 {
		return p, errors.New("caller policy custody refused")
	}
	d := json.NewDecoder(io.LimitReader(f, (16<<10)+1))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return p, errors.New("invalid caller policy")
	}
	return p, nil
}

func requestRoute(p Policy, r Request) (string, string, []byte, error) {
	if r.Action != "start" && r.Start != nil {
		return "", "", nil, errors.New("read-only action contains a start request")
	}
	base := "/v1/attempts/" + r.Attempt
	switch r.Action {
	case "list":
		return "GET", "/v1/attempts", nil, nil
	case "capacity":
		return "GET", "/v1/capacity", nil, nil
	case "signing-key":
		return "GET", "/v1/signing-key", nil, nil
	case "start":
		s := r.Start
		if s == nil || !attemptID.MatchString(s.ID) || s.Class != p.Class || !reflect.DeepEqual(s.Command, p.Command) || !reflect.DeepEqual(s.Outputs, p.Outputs) || s.Source == nil || s.Source.Mirror != p.Mirror || s.Source.Ref != p.Ref || !sha.MatchString(s.Source.SHA) || (p.SourceSelfBase && (!p.SourceGit || s.Source.BaseSHA != s.Source.SHA)) || (!p.SourceSelfBase && s.Source.BaseSHA != "") || s.Source.Git != p.SourceGit || s.Source.ReadOnly != p.SourceReadOnly || s.Source.Base != p.SourceBase {
			return "", "", nil, errors.New("start outside fixed class/source/command/output policy")
		}
		if len(s.Inputs) != len(p.Inputs) {
			return "", "", nil, errors.New("exact named-input set required")
		}
		seen := map[string]bool{}
		total := 0
		for _, in := range s.Inputs {
			allowed := false
			for _, name := range p.Inputs {
				if name == in.Name {
					allowed = true
				}
			}
			h := sha256.Sum256(in.Data)
			total += len(in.Data)
			if !allowed || seen[in.Name] || len(in.Data) == 0 || total > 32<<20 || hex.EncodeToString(h[:]) != in.SHA256 {
				return "", "", nil, errors.New("input name/digest/size refused")
			}
			seen[in.Name] = true
		}
		b, err := json.Marshal(s)
		return "POST", "/v1/attempts", b, err
	case "get", "result", "pins", "log", "output":
		if !attemptID.MatchString(r.Attempt) {
			return "", "", nil, errors.New("invalid attempt identifier")
		}
		if r.Action == "get" {
			return "GET", base, nil, nil
		}
		if r.Action == "output" {
			allowed := false
			for _, name := range p.Outputs {
				if name == r.Output {
					allowed = true
				}
			}
			if !allowed {
				return "", "", nil, errors.New("output outside fixed policy")
			}
			return "GET", base + "/outputs/" + r.Output, nil, nil
		}
		return "GET", base + "/" + r.Action, nil, nil
	default:
		return "", "", nil, errors.New("unsupported caller action")
	}
}

func privateRecord(path string, data []byte) error {
	return calljournal.Save(path, json.RawMessage(data))
}

// Run seals a start ID/digest before its one submission. Repeated same-byte
// calls recover the cached response or inspect that ID; they never mint an ID.
// A retry after a failed transport requires an explicit flag and fresh 404.
func Run(p Policy, reader io.Reader) (Reply, error) {
	reply := Reply{CallerUID: os.Geteuid(), Outcome: "local-refusal", Transport: "not-contacted", OperationState: "unknown", SessionState: "not-applicable"}
	if p.CallerUID != os.Geteuid() || p.CallerUID == 0 || p.Class == "" || p.Socket == "" {
		return reply, errors.New("effective caller identity/policy refused")
	}
	b, err := io.ReadAll(io.LimitReader(reader, maxWire+1))
	if err != nil || len(b) > maxWire {
		return reply, errors.New("caller request exceeds bound")
	}
	var r Request
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return reply, errors.New("invalid caller request")
	}
	method, path, body, err := requestRoute(p, r)
	if err != nil {
		return reply, err
	}
	h := sha256.Sum256(body)
	reply.RequestSHA256 = hex.EncodeToString(h[:])
	client := localclient.New(p.Socket, p.ManagerUID)
	defer client.CloseIdleConnections()
	call := func(method, path string, body []byte) (int, []byte, error) {
		req, e := http.NewRequest(method, "http://control"+path, bytes.NewReader(body))
		if e != nil {
			return 0, nil, e
		}
		req.Header.Set("Content-Type", "application/json")
		resp, e := client.Do(req)
		if e != nil {
			return 0, nil, errors.New("caller transport uncertain; inspect original attempt")
		}
		defer resp.Body.Close()
		data, e := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
		if e != nil || len(data) > 64<<20 {
			return 0, nil, errors.New("caller response uncertain or oversized; inspect original attempt")
		}
		return resp.StatusCode, data, nil
	}
	reply.SessionState = "not-applicable"
	if method != "POST" {
		reply.Status, reply.Body, err = call(method, path, body)
		reply.Transport = "response"
		reply.ResponseAvailable = err == nil
		if err != nil {
			reply.Transport = "lost-or-unrecorded"
			reply.Outcome = "uncertain"
			return reply, err
		}
		if reply.Status < 200 || reply.Status >= 300 {
			reply.Outcome = "refused"
			return reply, errors.New("caller HTTP refusal")
		}
		reply.Outcome = "snapshot"
		return reply, nil
	}
	if err = localclient.PrivateDir(p.Records); err != nil {
		return reply, err
	}
	dir := filepath.Join(p.Records, r.Start.ID)
	if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return reply, err
	}
	if err = localclient.PrivateDir(dir); err != nil {
		return reply, err
	}
	fd, e := syscall.Open(filepath.Join(dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return reply, errors.New("attempt lock unavailable")
	}
	f := os.NewFile(uintptr(fd), "lock")
	defer f.Close()
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return reply, errors.New("original attempt has an active caller")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	intent := filepath.Join(dir, "intent.json")
	sealed, _ := json.Marshal(struct {
		ID     string `json:"attempt_id"`
		Digest string `json:"request_sha256"`
	}{r.Start.ID, reply.RequestSHA256})
	if e := calljournal.Complete(intent, sealed); e != nil {
		return reply, e
	}
	existing := false
	if old, e := localclient.ReadPrivate(intent, 16<<10); e == nil {
		existing = true
		if !bytes.Equal(old, sealed) {
			reply.Outcome = "conflict"
			return reply, errors.New("attempt ID already binds another request")
		}
	} else {
		if _, e := os.Lstat(intent); !os.IsNotExist(e) {
			return reply, errors.New("attempt intent unreadable; preserve and inspect")
		}
		if err = privateRecord(intent, sealed); err != nil {
			return reply, err
		}
	}
	original := filepath.Join(dir, "original-request.json")
	if !existing {
		if e := calljournal.SaveBytes(original, body, maxWire); e != nil {
			return reply, e
		}
	} else {
		if e := calljournal.Complete(original, body); e != nil {
			return reply, e
		}
		originalBytes, e := localclient.ReadPrivate(original, maxWire)
		if e != nil || !bytes.Equal(originalBytes, body) {
			reply.Outcome = "uncertain"
			return reply, errors.New("sealed original request unavailable or changed; preserve and inspect")
		}
	}
	retainedResponse := func(out *Reply) error {
		ev, found, e := calljournal.LatestKind(dir, "response")
		if e != nil {
			return e
		}
		out.RetainedResponseAvailable = found
		out.RetainedResponseEventID = ev.ID
		if found {
			var saved Reply
			if json.Unmarshal(ev.Value, &saved) != nil || saved.RequestSHA256 != reply.RequestSHA256 || saved.CallerUID != p.CallerUID {
				return errors.New("retained response binding invalid")
			}
		}
		return nil
	}
	finish := func(saved Reply, cached bool) (Reply, error) {
		if e := retainedResponse(&saved); e != nil {
			saved.Outcome = "uncertain"
			return saved, e
		}
		saved.Cached = cached
		if saved.Status >= 500 || saved.Status == 409 {
			saved.Outcome = "uncertain"
			return saved, errors.New("original attempt unresolved; fresh inspection required")
		}
		if saved.Status < 200 || saved.Status >= 300 {
			saved.Outcome = "refused"
			return saved, errors.New("caller HTTP refusal; retained privately")
		}
		if !matchesAttempt(saved.Body, r.Start, p.CallerUID) {
			saved.Outcome = "conflict"
			return saved, errors.New("existing attempt does not match sealed source/command/inputs/caller")
		}
		var entry manager.Entry
		json.Unmarshal(saved.Body, &entry)
		saved.OperationState = string(entry.State)
		switch entry.State {
		case manager.StateStarting, manager.StateRunning, manager.StateStopping, manager.StateExited, manager.StateStopped, manager.StateFailed, manager.StateTimedOut:
			saved.Outcome = "admitted"
			if saved.Observation {
				saved.Outcome = "original-attempt-observed"
			}
			return saved, nil
		default:
			saved.Outcome = "uncertain"
			return saved, errors.New("original attempt execution unresolved")
		}
	}
	if e := calljournal.Recover(dir, func(ev calljournal.Event) bool {
		switch ev.Kind {
		case "response", "reconciliation":
			var old Reply
			return json.Unmarshal(ev.Value, &old) == nil && old.RequestSHA256 == reply.RequestSHA256 && old.CallerUID == p.CallerUID
		case "dispatch", "transport-uncertain":
			var value struct{ Digest string }
			return json.Unmarshal(ev.Value, &value) == nil && value.Digest == reply.RequestSHA256
		}
		return false
	}); e != nil {
		reply.Outcome = "uncertain"
		return reply, e
	}
	if e := retainedResponse(&reply); e != nil {
		reply.Outcome = "uncertain"
		return reply, e
	}
	latest, found, e := calljournal.Latest(dir)
	if e != nil {
		return reply, e
	}
	if found && latest.Kind == "response" {
		var old Reply
		if json.Unmarshal(latest.Value, &old) != nil || old.RequestSHA256 != reply.RequestSHA256 || old.CallerUID != p.CallerUID {
			return reply, errors.New("cached response binding invalid")
		}
		if old.Status == 201 {
			old.EventID = latest.ID
			old.ObservedAt = latest.At
			return finish(old, true)
		}
	}
	// Any nonterminal result or historical reconciliation gets a fresh GET.
	exchange := func(kind, verb, target string, payload []byte) (Reply, error) {
		out := reply
		out.Observation = kind == "reconciliation"
		if _, e := calljournal.Append(dir, "dispatch", struct{ Digest, Method, Path string }{reply.RequestSHA256, verb, target}); e != nil {
			return out, e
		}
		status, data, e := call(verb, target, payload)
		out.Status = status
		out.Body = data
		out.Transport = "response"
		out.ResponseAvailable = kind == "response" && e == nil
		if e != nil {
			out.Transport = "lost-or-unrecorded"
			out.Outcome = "uncertain"
			calljournal.Append(dir, "transport-uncertain", struct{ Digest string }{reply.RequestSHA256})
			return out, e
		}
		if len(data) > 8<<20 {
			out.Body = nil
			out.Outcome = "uncertain"
			return out, errors.New("response exceeds retention bound; inspect original attempt")
		}
		ev, e := calljournal.Append(dir, kind, out)
		out.EventID = ev.ID
		out.ObservedAt = ev.At
		if e != nil {
			out.Transport = "response-not-durable"
			out.Outcome = "uncertain"
			out.ResponseAvailable = false
			return out, errors.New("response retention uncertain; inspect original attempt")
		}
		if e := retainedResponse(&out); e != nil {
			out.Outcome = "uncertain"
			return out, e
		}
		return out, nil
	}
	observation, e := exchange("reconciliation", "GET", "/v1/attempts/"+r.Start.ID, nil)
	if e != nil {
		return observation, e
	}
	if observation.Status == 200 {
		return finish(observation, false)
	}
	if observation.Status != 404 {
		observation.Outcome = "uncertain"
		return observation, errors.New("original attempt inspection unresolved")
	}
	// Manager GET's documented 404 is authoritative absence in its retained
	// attempt table. No 409/503, local missing file or cached GET permits retry.
	if existing && !r.RetryKnownUnaccepted {
		observation.Outcome = "not-accepted"
		observation.OperationState = "not-accepted"
		return observation, errors.New("fresh original-attempt absence; explicit same-ID retry required")
	}
	answer, e := exchange("response", method, path, body)
	if e != nil {
		return answer, e
	}
	return finish(answer, false)
}

func matchesAttempt(data []byte, s *manager.StartRequest, uid int) bool {
	var e manager.Entry
	if json.Unmarshal(data, &e) != nil || e.Attempt != s.ID || e.Class.Name != s.Class || !reflect.DeepEqual(e.Command, s.Command) || !reflect.DeepEqual(e.OutputNames, s.Outputs) || e.Source == nil || e.StartedByUID == nil || int(*e.StartedByUID) != uid {
		return false
	}
	a, b := e.Source, s.Source
	base := ""
	if b.Git && b.BaseSHA == "" {
		base = b.Base
		if base == "" {
			base = "main"
		}
	}
	if a.Mirror != b.Mirror || a.Ref != b.Ref || a.SHA != b.SHA || a.Git != b.Git || a.BaseSHA != b.BaseSHA || a.Base != base || a.ReadOnly != b.ReadOnly || len(e.Inputs) != len(s.Inputs) {
		return false
	}
	inputs := map[string]manager.InputRecord{}
	for _, in := range e.Inputs {
		if _, ok := inputs[in.Name]; ok {
			return false
		}
		inputs[in.Name] = in
	}
	for _, in := range s.Inputs {
		old, ok := inputs[in.Name]
		if !ok || old.SHA256 != in.SHA256 || old.Bytes != len(in.Data) {
			return false
		}
	}
	return true
}
