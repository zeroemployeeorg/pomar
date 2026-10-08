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

	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/manager"
)

const maxWire = 48 << 20

var attemptID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
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
	CallerUID     int    `json:"caller_uid"`
	Status        int    `json:"http_status"`
	RequestSHA256 string `json:"request_sha256"`
	Observation   bool   `json:"observation"`
	Body          []byte `json:"body"`
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
		if s == nil || !attemptID.MatchString(s.ID) || s.Class != p.Class || !reflect.DeepEqual(s.Command, p.Command) || !reflect.DeepEqual(s.Outputs, p.Outputs) || s.Source == nil || s.Source.Mirror != p.Mirror || s.Source.Ref != p.Ref || !sha.MatchString(s.Source.SHA) || s.Source.BaseSHA != "" || s.Source.Git != p.SourceGit || s.Source.ReadOnly != p.SourceReadOnly || s.Source.Base != p.SourceBase {
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Run seals a start ID/digest before its one submission. Repeated same-byte
// calls recover the cached response or inspect that ID; they never mint an ID.
// A retry after a failed transport requires an explicit flag and fresh 404.
func Run(p Policy, reader io.Reader) (Reply, error) {
	reply := Reply{CallerUID: os.Geteuid()}
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
	var record string
	if method == "POST" {
		if err = localclient.PrivateDir(p.Records); err != nil {
			return reply, err
		}
		dir := filepath.Join(p.Records, r.Start.ID)
		existing := false
		if err = os.Mkdir(dir, 0700); os.IsExist(err) {
			existing = true
		} else if err != nil {
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
		if old, e := localclient.ReadPrivate(intent, 16<<10); e == nil {
			if !bytes.Equal(old, sealed) {
				return reply, errors.New("attempt ID already binds another request")
			}
		} else if existing {
			return reply, errors.New("attempt intent incomplete; preserve and inspect")
		} else if err = privateRecord(intent, sealed); err != nil {
			return reply, err
		}
		record = filepath.Join(dir, "response.json")
		for _, name := range []string{"reconciliation.json", "response.json"} {
			cachedPath := filepath.Join(dir, name)
			if saved, e := localclient.ReadPrivate(cachedPath, 12<<20); e == nil {
				var old Reply
				if json.Unmarshal(saved, &old) != nil || old.RequestSHA256 != reply.RequestSHA256 || old.CallerUID != p.CallerUID {
					return reply, errors.New("cached caller response identity invalid")
				}
				if old.Status >= 500 {
					record = filepath.Join(dir, "reconciliation.json")
					continue
				}
				return old, nil
			} else if _, e := os.Lstat(cachedPath); !os.IsNotExist(e) {
				return reply, errors.New("recorded start response unreadable; preserve and inspect")
			}
		}
		status, data, e := call("GET", "/v1/attempts/"+r.Start.ID, nil)
		if e != nil {
			return reply, e
		}
		if status == 200 {
			reply.Status = status
			reply.Body = data
			reply.Observation = true
			encoded, _ := json.Marshal(reply)
			if len(data) > 8<<20 || privateRecord(filepath.Join(dir, "reconciliation.json"), encoded) != nil {
				return reply, errors.New("original-attempt observation retention uncertain")
			}
			return reply, nil
		}
		if status != 404 {
			return reply, errors.New("original attempt inspection unresolved")
		}
		if existing && !r.RetryKnownUnaccepted {
			return reply, errors.New("original attempt currently absent; explicit same-ID retry required")
		}
	}
	reply.Status, reply.Body, err = call(method, path, body)
	if err != nil {
		return reply, err
	}
	if record != "" {
		if len(reply.Body) > 8<<20 {
			return reply, errors.New("start response exceeds retention bound; inspect original attempt")
		}
		data, _ := json.Marshal(reply)
		if err = privateRecord(record, data); err != nil {
			return reply, errors.New("start response retention uncertain; inspect original attempt")
		}
	}
	return reply, nil
}
