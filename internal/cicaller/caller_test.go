//go:build darwin

package cicaller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/manager"
)

func fixture(t *testing.T, h http.HandlerFunc) (Policy, Request) {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "ci-")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(socket, 0660)
	s := &http.Server{Handler: h}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	records := filepath.Join(root, "records")
	os.Mkdir(records, 0700)
	p := Policy{CallerUID: os.Geteuid(), ManagerUID: os.Geteuid(), Socket: socket, Records: records, Class: "fixed-ci", Mirror: "fixed-source", Ref: "refs/heads/main", Command: []string{"make", "verify"}, Inputs: []string{"one.json", "two.json"}, Outputs: []string{"release.json.gz"}}
	inputs := []manager.Input{}
	for _, name := range p.Inputs {
		b := []byte(`{"fixture":true}`)
		hash := sha256.Sum256(b)
		inputs = append(inputs, manager.Input{Name: name, Data: b, SHA256: hex.EncodeToString(hash[:])})
	}
	r := Request{Action: "start", Start: &manager.StartRequest{ID: "original-attempt", Class: p.Class, Command: p.Command, Outputs: p.Outputs, Source: &manager.Source{Mirror: p.Mirror, Ref: p.Ref, SHA: strings.Repeat("a", 40)}, Inputs: inputs}}
	return p, r
}
func runRequest(p Policy, r Request) (Reply, error) {
	b, _ := json.Marshal(r)
	return Run(p, bytes.NewReader(b))
}

func TestFixedPolicyRefusesPrivilegeExpansionBeforeAPI(t *testing.T) {
	for _, which := range []string{"class", "command", "mirror", "ref", "source", "source-git", "source-base", "source-readonly", "outputs", "input-name", "input-hash", "missing-input", "arbitrary-action"} {
		t.Run(which, func(t *testing.T) {
			var calls atomic.Int32
			var p Policy
			var r Request
			p, r = fixture(t, func(w http.ResponseWriter, req *http.Request) { calls.Add(1) })
			switch which {
			case "class":
				r.Start.Class = "another-class"
			case "command":
				r.Start.Command = []string{"sh", "-c", "true"}
			case "mirror":
				r.Start.Source.Mirror = "another-source"
			case "ref":
				r.Start.Source.Ref = "refs/heads/unreviewed"
			case "source":
				r.Start.Source.SHA = "main"
			case "source-git":
				r.Start.Source.Git = true
			case "source-base":
				r.Start.Source.Base = "another-branch"
			case "source-readonly":
				r.Start.Source.ReadOnly = true
			case "outputs":
				r.Start.Outputs = []string{"../../owner-key"}
			case "input-name":
				r.Start.Inputs[0].Name = "../../owner-key"
			case "input-hash":
				r.Start.Inputs[0].Data = []byte("changed")
			case "missing-input":
				r.Start.Inputs = r.Start.Inputs[:1]
			case "arbitrary-action":
				r.Action = "drain"
			}
			if _, err := runRequest(p, r); err == nil {
				t.Fatal("expanded caller capability accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("invalid policy reached API")
			}
		})
	}
}

func TestSealedAttemptIsRecoveredWithoutDuplicateAdmission(t *testing.T) {
	var posts, gets atomic.Int32
	var p Policy
	var r Request
	p, r = fixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			gets.Add(1)
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(attemptEntry(p, r))
	})
	a, err := runRequest(p, r)
	if err != nil || a.Status != 201 {
		t.Fatal(err)
	}
	seal, err := os.ReadFile(filepath.Join(p.Records, r.Start.ID, "intent.json"))
	if err != nil || !bytes.Contains(seal, []byte(a.RequestSHA256)) {
		t.Fatal("request identity not durably sealed")
	}
	b, err := runRequest(p, r)
	if err != nil || b.RequestSHA256 != a.RequestSHA256 || posts.Load() != 1 || gets.Load() != 1 {
		t.Fatal("completed admission repeated")
	}
	r.Start.Source.SHA = strings.Repeat("b", 40)
	if _, err = runRequest(p, r); err == nil || posts.Load() != 1 {
		t.Fatal("same ID admitted different source")
	}
}

func TestLostAdmissionResponseInspectsOriginalID(t *testing.T) {
	var posts atomic.Int32
	var p Policy
	var r Request
	p, r = fixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			posts.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		if posts.Load() == 0 {
			w.WriteHeader(404)
		} else {
			json.NewEncoder(w).Encode(attemptEntry(p, r))
		}
	})
	if _, err := runRequest(p, r); err == nil {
		t.Fatal("lost response relabelled success")
	}
	a, err := runRequest(p, r)
	if err != nil || !a.Observation || posts.Load() != 1 {
		t.Fatal("uncertain admission repeated instead of inspected")
	}
}

func TestUnacceptedRetryNeedsExplicitConsentAndSameDigest(t *testing.T) {
	var posts atomic.Int32
	var p Policy
	var r Request
	p, r = fixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	runRequest(p, r)
	if _, err := runRequest(p, r); err == nil || posts.Load() != 1 {
		t.Fatal("blind retry dispatched")
	}
	r.RetryKnownUnaccepted = true
	runRequest(p, r)
	if posts.Load() != 2 {
		t.Fatal("explicit original retry did not reconcile and dispatch")
	}
}

func TestReadRoutesAndActualPeerCustody(t *testing.T) {
	p, _ := fixture(t, func(w http.ResponseWriter, req *http.Request) { w.Write([]byte(`[]`)) })
	if r, err := runRequest(p, Request{Action: "list"}); err != nil || r.CallerUID != os.Geteuid() || r.Status != 200 {
		t.Fatal("native fixture caller route failed", err)
	}
	p.ManagerUID++
	if _, err := runRequest(p, Request{Action: "list"}); err == nil {
		t.Fatal("unexpected socket owner accepted")
	}
}

func TestServerFailureReconcilesAndRetainsObservation(t *testing.T) {
	var posts, gets atomic.Int32
	var p Policy
	var r Request
	p, r = fixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			posts.Add(1)
			w.WriteHeader(502)
			return
		}
		gets.Add(1)
		if posts.Load() == 0 {
			w.WriteHeader(404)
		} else {
			json.NewEncoder(w).Encode(attemptEntry(p, r))
		}
	})
	a, err := runRequest(p, r)
	if err == nil || a.Status != 502 {
		t.Fatal("server refusal not retained")
	}
	a, err = runRequest(p, r)
	if err != nil || !a.Observation || posts.Load() != 1 {
		t.Fatal("5xx caused blind retry")
	}
	count := gets.Load()
	a, err = runRequest(p, r)
	if err != nil || !a.Observation || gets.Load() != count+1 || posts.Load() != 1 {
		t.Fatal("observation was not recovered locally")
	}
}

func TestSelfBaseBindsTheIdenticalPinnedGitCommit(t *testing.T) {
	var p Policy
	var r Request
	p, r = fixture(t, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(404) })
	p.Ref = "refs/heads/main"
	p.SourceGit = true
	p.SourceSelfBase = true
	r.Start.Source.Ref = p.Ref
	r.Start.Source.Git = true
	if _, _, _, e := requestRoute(p, r); e == nil {
		t.Fatal("missing self base accepted")
	}
	r.Start.Source.BaseSHA = strings.Repeat("b", 40)
	if _, _, _, e := requestRoute(p, r); e == nil {
		t.Fatal("independently selected base accepted")
	}
	r.Start.Source.BaseSHA = r.Start.Source.SHA
	if _, _, _, e := requestRoute(p, r); e != nil {
		t.Fatal(e)
	}
	r.Start.Source.Git = false
	if _, _, _, e := requestRoute(p, r); e == nil {
		t.Fatal("self base without Git accepted")
	}
}

func attemptEntry(p Policy, r Request) manager.Entry {
	uid := uint32(p.CallerUID)
	s := r.Start.Source
	e := manager.Entry{Attempt: r.Start.ID, Command: r.Start.Command, Class: capacity.Class{Name: p.Class}, State: manager.StateRunning, StartedByUID: &uid, OutputNames: r.Start.Outputs, Source: &manager.PinnedSource{Mirror: s.Mirror, Ref: s.Ref, SHA: s.SHA, Git: s.Git, BaseSHA: s.BaseSHA, ReadOnly: s.ReadOnly}}
	if s.Git && s.BaseSHA == "" {
		e.Source.Base = s.Base
		if e.Source.Base == "" {
			e.Source.Base = "main"
		}
	}
	for _, in := range r.Start.Inputs {
		e.Inputs = append(e.Inputs, manager.InputRecord{Name: in.Name, Bytes: len(in.Data), SHA256: in.SHA256})
	}
	return e
}
