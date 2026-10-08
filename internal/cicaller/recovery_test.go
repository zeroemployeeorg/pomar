//go:build darwin

package cicaller

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRepeatedCapacityRefusalsRetainAllRepliesThenAdmission(t *testing.T) {
	var p Policy
	var r Request
	var posts, gets atomic.Int32
	p, r = fixture(t, func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "GET" {
			gets.Add(1)
			w.WriteHeader(404)
			return
		}
		if posts.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(attemptEntry(p, r))
	})
	for n := int32(1); n <= 3; n++ {
		r.RetryKnownUnaccepted = n > 1
		a, e := runRequest(p, r)
		if n < 3 && e == nil {
			t.Fatal("refusal succeeded")
		}
		if n == 3 && (e != nil || a.Outcome != "admitted") {
			t.Fatal(a, e)
		}
	}
	a, e := runRequest(p, r)
	if e != nil || !a.Cached || posts.Load() != 3 || gets.Load() != 3 {
		t.Fatal(a, e)
	}
	files, e := filepath.Glob(filepath.Join(p.Records, r.Start.ID, "event-*.json"))
	if e != nil || len(files) != 12 {
		t.Fatal("append-only history incomplete", len(files), e)
	}
}
func TestExistingAttemptMustMatchEverySealedBinding(t *testing.T) {
	for _, field := range []string{"source", "command", "input-digest", "input-size", "uid", "class", "output", "id"} {
		t.Run(field, func(t *testing.T) {
			var p Policy
			var r Request
			var posts atomic.Int32
			p, r = fixture(t, func(w http.ResponseWriter, q *http.Request) {
				if q.Method == "POST" {
					posts.Add(1)
					t.Error("collision dispatched")
					return
				}
				e := attemptEntry(p, r)
				switch field {
				case "source":
					e.Source.SHA = strings.Repeat("b", 40)
				case "command":
					e.Command = []string{"wrong"}
				case "input-digest":
					e.Inputs[0].SHA256 = strings.Repeat("c", 64)
				case "input-size":
					e.Inputs[0].Bytes++
				case "uid":
					u := uint32(999)
					e.StartedByUID = &u
				case "class":
					e.Class.Name = "other"
				case "output":
					e.OutputNames = []string{"other"}
				case "id":
					e.Attempt = "other"
				}
				json.NewEncoder(w).Encode(e)
			})
			a, e := runRequest(p, r)
			if e == nil || a.Outcome != "conflict" || posts.Load() != 0 {
				t.Fatal(a, e)
			}
		})
	}
}
func TestAdmissionPersistenceFailureReconcilesBoundOriginal(t *testing.T) {
	var p Policy
	var r Request
	var posts atomic.Int32
	p, r = fixture(t, func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "GET" && posts.Load() == 0 {
			w.WriteHeader(404)
			return
		}
		if q.Method == "POST" {
			posts.Add(1)
			os.Chmod(filepath.Join(p.Records, r.Start.ID), 0500)
			w.WriteHeader(201)
		}
		json.NewEncoder(w).Encode(attemptEntry(p, r))
	})
	a, e := runRequest(p, r)
	if e == nil || a.Transport != "response-not-durable" {
		t.Fatal(a, e)
	}
	os.Chmod(filepath.Join(p.Records, r.Start.ID), 0700)
	a, e = runRequest(p, r)
	if e != nil || !a.Observation || a.Cached || posts.Load() != 1 {
		t.Fatal(a, e)
	}
	a, e = runRequest(p, r)
	if e != nil || a.Cached || posts.Load() != 1 {
		t.Fatal("historical observation masqueraded as current", a, e)
	}
}
func TestConcurrentAttemptHasOneAdmission(t *testing.T) {
	var p Policy
	var r Request
	var posts atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	p, r = fixture(t, func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		close(entered)
		<-release
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(attemptEntry(p, r))
	})
	done := make(chan error, 1)
	go func() { _, e := runRequest(p, r); done <- e }()
	<-entered
	if _, e := runRequest(p, r); e == nil || posts.Load() != 1 {
		t.Error("concurrent admission", e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
