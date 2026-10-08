// Package ownerclient retains the identity and outcome of bounded owner calls.
// A lost transport is uncertainty, never permission to invent a replacement ID.
package ownerclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

var id = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
var ErrUncertain = errors.New("acceptance unknown; reconcile this original operation before retrying")
var ErrConflict = errors.New("operation identity already binds different input or action")

type Config struct {
	Root, Records, Environment, Action, OperationID, Request, Session, Incarnation string
	Reconcile, RetryKnownUnaccepted                                                bool
}

type Intent struct {
	Action, Environment, OperationID, RequestSHA256, Socket, Session, Incarnation string
}

// Receipt is ordinary diagnostic metadata. Response/request/provider bytes are
// kept only in the separately protected files, never in this structure.
type Receipt struct {
	Action      string `json:"action"`
	OperationID string `json:"operation_id"`
	UID         int    `json:"euid"`
	HTTPStatus  int    `json:"http_status"`
	Outcome     string `json:"outcome"`
	Cached      bool   `json:"cached"`
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

func saveRecord(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return errors.New("cannot encode private record")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(f.Name(), path); err != nil {
		return err
	}
	if err = os.Remove(f.Name()); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

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
	meta := Receipt{Action: c.Action, OperationID: c.OperationID, UID: os.Geteuid()}
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
	if method == "POST" {
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
	} else if c.Request != "" {
		return meta, errors.New("read-only actions do not take a request body")
	}
	sum := sha256.Sum256(body)
	intent := Intent{Action: c.Action, Environment: c.Environment, OperationID: c.OperationID, RequestSHA256: hex.EncodeToString(sum[:]), Socket: filepath.Join(c.Root, "host.sock"), Session: c.Session, Incarnation: c.Incarnation}
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
	finish := func(saved response, cached bool) (Receipt, error) {
		meta.HTTPStatus = saved.Status
		meta.Outcome = saved.Kind
		meta.Cached = cached
		if saved.Status < 200 || saved.Status >= 300 {
			return meta, errors.New("owner HTTP refusal; response retained privately")
		}
		return meta, nil
	}
	for _, name := range []string{"reconciliation.json", "response.json"} {
		if data, e := localclient.ReadPrivate(filepath.Join(dir, name), 12<<20); e == nil {
			var saved response
			if json.Unmarshal(data, &saved) != nil || saved.Intent != intent || len(saved.Body) > responseLimit {
				return meta, errors.New("recorded response identity invalid")
			}
			if c.Reconcile && method == "POST" && name == "response.json" && saved.Status >= 500 {
				continue
			}
			return finish(saved, true)
		} else if _, e := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(e) {
			return meta, errors.New("recorded response unreadable; preserve and inspect")
		}
	}
	client := localclient.New(intent.Socket, os.Geteuid())
	defer client.CloseIdleConnections()
	if existing && method == "POST" {
		if !c.Reconcile {
			return meta, ErrUncertain
		}
		inspect := c
		inspect.Action = "operation"
		if c.Action == "login" || c.Action == "login-complete" {
			inspect.Action = "agent-operation"
		}
		_, p, _ := route(inspect)
		status, b, e := call(client, "GET", p, nil)
		if e != nil {
			return meta, ErrUncertain
		}
		var evidence struct {
			Evidence string `json:"evidence"`
		}
		if status == 404 && json.Unmarshal(b, &evidence) == nil && evidence.Evidence == "durable_non_acceptance" {
			if !c.RetryKnownUnaccepted {
				return meta, errors.New("durable non-acceptance observed; explicit same-operation retry required")
			}
		} else {
			if status != 200 {
				return meta, ErrUncertain
			}
			saved := response{Intent: intent, Status: status, Body: b, Kind: "original-operation-observed"}
			if err = saveRecord(filepath.Join(dir, "reconciliation.json"), saved); err != nil {
				return meta, ErrUncertain
			}
			return finish(saved, false)
		}
	}
	status, b, err := call(client, method, path, body)
	if err != nil {
		return meta, ErrUncertain
	}
	saved := response{Intent: intent, Status: status, Body: b, Kind: "response-recorded"}
	if err = saveRecord(filepath.Join(dir, "response.json"), saved); err != nil {
		return meta, ErrUncertain
	}
	return finish(saved, false)
}
