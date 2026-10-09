package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/seatapi"
	"github.com/zeroemployeeorg/pomar/internal/seatdecl"
)

// fakeHost is the development host's API as `seat up` sees it: the real
// seat routes, and environments that count creates and starts.
type fakeHost struct {
	mu      sync.Mutex
	envs    map[string]map[string]any
	creates int
	starts  int
	stops   int
}

func lifecycleHost(t *testing.T) (string, seatapi.Store, *fakeHost) {
	t.Helper()
	root, _ := os.MkdirTemp("/tmp", "seatup")
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { os.RemoveAll(root) })
	store := seatapi.Store{Root: filepath.Join(root, "seats")}
	os.Mkdir(store.Root, 0o700)
	f := &fakeHost{envs: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.Handle("/v1/seats", store.Handler())
	mux.Handle("/v1/seats/", store.Handler())
	mux.HandleFunc("GET /v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		e := f.envs[r.PathValue("id")]
		if e == nil {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"environment": e})
	})
	mux.HandleFunc("POST /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID, OperationID, Profile string }
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.envs[req.ID] != nil {
			w.WriteHeader(200)
			return
		}
		f.creates++
		f.envs[req.ID] = map[string]any{"phase": "created", "spec": map[string]string{"profile": req.Profile, "session": "session-1", "incarnation": "created-1"}}
		w.WriteHeader(201)
	})
	mux.HandleFunc("POST /v1/environments/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			OperationID string `json:"operation_id"`
			Incarnation string `json:"expected_incarnation"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		e := f.envs[r.PathValue("id")]
		spec := e["spec"].(map[string]string)
		if req.Incarnation != spec["incarnation"] {
			w.WriteHeader(409)
			return
		}
		switch r.PathValue("action") {
		case "start":
			f.starts++
			e["phase"] = "running"
			spec["incarnation"] = "running-1"
		case "stop":
			f.stops++
			e["phase"] = "stopped"
		}
		json.NewEncoder(w).Encode(map[string]any{"action": map[string]string{"state": "completed"}})
	})
	mux.HandleFunc("GET /v1/environments/{id}/operations/{op}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"action": map[string]string{"state": "completed"}})
	})
	ln, err := net.Listen("unix", filepath.Join(root, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(root, "host.sock"), 0o600)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return root, store, f
}

func declareSeat(t *testing.T, s seatapi.Store, version string) string {
	t.Helper()
	pin := strings.Repeat("c", 40)
	d := seatdecl.Declaration{Schema: seatdecl.Schema, Seat: "zeocreator", Project: "zeocreator",
		Sources: []seatdecl.Source{
			{Role: "work", Repository: "zeroemployeeorg/zeocreator", Fork: "architect/zeocreator", Branch: "main", Commit: pin, Tree: pin, Destination: "work", Writable: true},
			{Role: "records", Repository: "owner/records", Fork: "architect/records", Branch: "records/zeocreator", Commit: pin, Tree: pin, Destination: "records", Writable: true}},
		Class: "seat-linux-arm64-v1", Resources: seatdecl.Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
		Provider: seatdecl.Provider{Agent: "claude", Version: version, Account: seatdecl.NotEstablished},
		Resume:   seatdecl.Resume{Policy: "fresh-from-sow", HandoverSOW: "x"},
		Identity: seatdecl.Identity{Role: seatdecl.NotEstablished, Forks: []string{"architect/zeocreator", "architect/records"},
			Push: []string{"refs/heads/zeocreator/*"}, Permissions: map[string]string{"contents": "write", "metadata": "read"}}}
	raw, _ := json.Marshal(d)
	cur, _, _ := s.Current("zeocreator")
	got, err := s.Declare("zeocreator", cur.SHA256, raw)
	if err != nil {
		t.Fatal(err)
	}
	return got.SHA256
}

func records(t *testing.T) string {
	t.Helper()
	d, _ := os.MkdirTemp("/tmp", "seatrec")
	d, _ = filepath.EvalSymlinks(d)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestSeatUpRefusesUnlessTheLocationNamesThisHost(t *testing.T) {
	root, store, f := lifecycleHost(t)
	sum := declareSeat(t, store, "2.1.280")
	rec := records(t)
	var out, errb bytes.Buffer
	if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "zeocreator"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no recorded location") {
		t.Fatalf("no location: %d %s", code, errb.String())
	}
	locations := seatdecl.Locations{Dir: filepath.Join(store.Root, "locations")}
	os.Mkdir(locations.Dir, 0o700)
	locations.Move("zeocreator", 0, seatdecl.Location{Where: "mac-mini-naked", DeclarationSHA256: sum})
	errb.Reset()
	if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "zeocreator"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "never runs in two places") {
		t.Fatalf("elsewhere: %d %s", code, errb.String())
	}
	locations.Move("zeocreator", 1, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: sum, Handover: "h", MechanismBinding: "m"})
	declareSeat(t, store, "2.1.281") // a newer declaration the location doesn't name
	errb.Reset()
	if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "zeocreator"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "older declaration") {
		t.Fatalf("older declaration: %d %s", code, errb.String())
	}
	if f.creates != 0 || f.starts != 0 {
		t.Fatalf("%d creates and %d starts on a refusal", f.creates, f.starts)
	}
}

func TestSeatUpCreatesAndStartsOnceAndStopStops(t *testing.T) {
	root, store, f := lifecycleHost(t)
	sum := declareSeat(t, store, "2.1.280")
	locations := seatdecl.Locations{Dir: filepath.Join(store.Root, "locations")}
	os.Mkdir(locations.Dir, 0o700)
	locations.Move("zeocreator", 0, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: sum})
	rec := records(t)
	for i := 0; i < 2; i++ {
		var out, errb bytes.Buffer
		if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "zeocreator"}, &out, &errb); code != 0 {
			t.Fatalf("up %d: %d %s", i, code, errb.String())
		}
		if i == 1 && !strings.Contains(out.String(), "is running") {
			t.Fatalf("a second up: %s", out.String())
		}
	}
	if f.creates != 1 || f.starts != 1 {
		t.Fatalf("%d creates and %d starts", f.creates, f.starts)
	}
	var out, errb bytes.Buffer
	if code := seatStopCmd([]string{"-root", root, "-records", rec, "zeocreator"}, &out, &errb); code != 0 || f.stops != 1 {
		t.Fatalf("stop: %d %s, %d stops", code, errb.String(), f.stops)
	}
}

func TestSeatProfileCompilesTheDeclaration(t *testing.T) {
	root, store, _ := lifecycleHost(t)
	declareSeat(t, store, "2.1.280")
	cat := seatdecl.Classes{Schema: seatdecl.ClassesSchema, Classes: map[string]seatdecl.Class{"seat-linux-arm64-v1": {
		Base: "/base.ext4", ImageRef: "localhost/pomar/runner-tools:x", ImageDigest: "sha256:" + strings.Repeat("e", 64),
		Max:    seatdecl.Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
		Agents: map[string]seatdecl.Agent{"claude@2.1.280": {Archive: "/claude.tar", ArchiveSHA256: strings.Repeat("f", 64), InteractiveQualified: "SOW NN", RequiredHosts: []string{"api.anthropic.com"}}}}}}
	b, _ := json.Marshal(cat)
	classes := filepath.Join(t.TempDir(), "classes.json")
	os.WriteFile(classes, b, 0o600)
	var out, errb bytes.Buffer
	if code := seatProfileCmd([]string{"-root", root, "-classes", classes, "-source-bundle", "/owner/zeocreator.tar.gz", "zeocreator"}, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	var got map[string]map[string]any
	if json.Unmarshal(out.Bytes(), &got) != nil || got["seat-zeocreator"]["seat"] != "zeocreator" || got["seat-zeocreator"]["sourceBundle"] != "/owner/zeocreator.tar.gz" {
		t.Fatalf("profile %s", out.String())
	}
	cat.Classes["seat-linux-arm64-v1"].Agents["claude@2.1.280"] = seatdecl.Agent{Archive: "/claude.tar", ArchiveSHA256: strings.Repeat("f", 64)}
	b, _ = json.Marshal(cat)
	os.WriteFile(classes, b, 0o600)
	if code := seatProfileCmd([]string{"-root", root, "-classes", classes, "-source-bundle", "/owner/zeocreator.tar.gz", "zeocreator"}, &out, &errb); code != 1 {
		t.Fatal("an agent not qualified interactively was compiled")
	}
}
