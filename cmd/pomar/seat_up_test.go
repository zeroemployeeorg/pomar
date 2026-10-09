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
	mu       sync.Mutex
	envs     map[string]map[string]any
	profiles map[string]map[string]any // the host configuration's profiles
	creates  int
	starts   int
	stops    int
}

func lifecycleHost(t *testing.T) (string, seatapi.Store, *fakeHost) {
	t.Helper()
	return lifecycleHostAt(t, "pomar:macbook")
}

// lifecycleHostAt serves the seat routes as a host whose configuration
// names here as its location ("" names none).
func lifecycleHostAt(t *testing.T, here string) (string, seatapi.Store, *fakeHost) {
	t.Helper()
	root, _ := os.MkdirTemp("/tmp", "seatup")
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { os.RemoveAll(root) })
	store := seatapi.Store{Root: filepath.Join(root, "seats"), Here: here}
	os.Mkdir(store.Root, 0o700)
	f := &fakeHost{envs: map[string]map[string]any{}, profiles: map[string]map[string]any{}}
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
		spec := map[string]any{"profile": req.Profile, "session": "session-1", "incarnation": "created-1"}
		for k, v := range f.profiles[req.Profile] { // the host resolves the profile into the spec
			if k != "sourceBundle" && k != "agentArchive" && k != "agentArchiveSHA256" {
				spec[k] = v
			}
		}
		spec["codexArchiveSHA256"] = f.profiles[req.Profile]["agentArchiveSHA256"]
		f.envs[req.ID] = map[string]any{"phase": "created", "spec": spec}
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
		spec := e["spec"].(map[string]any)
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
	return declareSeatAt(t, s, version, strings.Repeat("c", 40))
}

// declareSeatAt declares zeocreator with its work and records sources at pin.
func declareSeatAt(t *testing.T, s seatapi.Store, version, pin string) string {
	t.Helper()
	sources := []seatdecl.Source{{Role: "work", Repository: "zeroemployeeorg/zeocreator", Fork: "architect/zeocreator", Branch: "main", Commit: pin, Tree: pin, Destination: "work", Writable: true}}
	sources = append(sources, seatdecl.Source{Role: "records", Repository: "owner/records", Fork: "architect/records", Branch: "records/zeocreator", Commit: pin, Tree: pin, Destination: "records", Writable: true})
	d := seatdecl.Declaration{Schema: seatdecl.Schema, Seat: "zeocreator", Project: "zeocreator",
		Sources: sources,
		Class:   "seat-linux-arm64-v1", Resources: seatdecl.Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
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

// writeClasses writes the owner's catalogue, its image at digest, with
// Claude Code 2.1.280 and 2.1.281 qualified interactively.
func writeClasses(t *testing.T, path, digest string) {
	t.Helper()
	agent := seatdecl.Agent{Archive: "/claude.tar", ArchiveSHA256: strings.Repeat("f", 64), InteractiveQualified: "SOW NN", RequiredHosts: []string{"api.anthropic.com"}}
	cat := seatdecl.Classes{Schema: seatdecl.ClassesSchema, Classes: map[string]seatdecl.Class{"seat-linux-arm64-v1": {
		Base: "/base.ext4", ImageRef: "localhost/pomar/runner-tools@sha256:" + digest, ImageDigest: "sha256:" + digest,
		Max:    seatdecl.Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
		Agents: map[string]seatdecl.Agent{"claude@2.1.280": agent, "claude@2.1.281": agent}}}}
	b, _ := json.Marshal(cat)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// installProfile compiles the seat's profile and adds it to the fake host's
// configuration, as the owner does.
func installProfile(t *testing.T, root, classes string, f *fakeHost) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := seatProfileCmd([]string{"-root", root, "-classes", classes, "-source-bundle", "/owner/zeocreator.tar.gz", "zeocreator"}, &out, &errb); code != 0 {
		t.Fatalf("profile: %d %s", code, errb.String())
	}
	var p map[string]map[string]any
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range p {
		f.profiles[k] = v
	}
}

func catalogue(t *testing.T) string {
	t.Helper()
	classes := filepath.Join(t.TempDir(), "classes.json")
	writeClasses(t, classes, strings.Repeat("e", 64))
	return classes
}

func records(t *testing.T) string {
	t.Helper()
	d, _ := os.MkdirTemp("/tmp", "seatrec")
	d, _ = filepath.EvalSymlinks(d)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestSeatUpRefusesUnlessTheLocationNamesThisHost(t *testing.T) {
	carryRecords(t)
	root, store, f := lifecycleHost(t)
	sum := declareSeat(t, store, "2.1.280")
	rec, classes := records(t), catalogue(t)
	var out, errb bytes.Buffer
	if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "-classes", classes, "zeocreator"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no recorded location") {
		t.Fatalf("no location: %d %s", code, errb.String())
	}
	locations := seatdecl.Locations{Dir: filepath.Join(store.Root, "locations")}
	os.Mkdir(locations.Dir, 0o700)
	locations.Move("zeocreator", 0, seatdecl.Location{Where: "mac-mini-naked", DeclarationSHA256: sum})
	errb.Reset()
	if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "-classes", classes, "zeocreator"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "never runs in two places") {
		t.Fatalf("elsewhere: %d %s", code, errb.String())
	}
	locations.Move("zeocreator", 1, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: sum, Handover: "h", MechanismBinding: "m"})
	declareSeat(t, store, "2.1.281") // a newer declaration the location doesn't name
	errb.Reset()
	if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "-classes", classes, "zeocreator"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "older declaration") {
		t.Fatalf("older declaration: %d %s", code, errb.String())
	}
	if f.creates != 0 || f.starts != 0 {
		t.Fatalf("%d creates and %d starts on a refusal", f.creates, f.starts)
	}
}

func TestSeatUpCreatesAndStartsOnceAndStopStops(t *testing.T) {
	carryRecords(t)
	root, store, f := lifecycleHost(t)
	sum := declareSeat(t, store, "2.1.280")
	locations := seatdecl.Locations{Dir: filepath.Join(store.Root, "locations")}
	os.Mkdir(locations.Dir, 0o700)
	locations.Move("zeocreator", 0, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: sum})
	rec, classes := records(t), catalogue(t)
	installProfile(t, root, classes, f)
	for i := 0; i < 2; i++ {
		var out, errb bytes.Buffer
		if code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "-classes", classes, "zeocreator"}, &out, &errb); code != 0 {
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
	carryRecords(t)
	root, store, _ := lifecycleHost(t)
	sum := declareSeat(t, store, "2.1.280")
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
	if json.Unmarshal(out.Bytes(), &got) != nil || got["seat-zeocreator"]["seat"] != "zeocreator" || got["seat-zeocreator"]["sourceBundle"] != "/owner/zeocreator.tar.gz" || got["seat-zeocreator"]["seatDeclarationSHA256"] != sum {
		t.Fatalf("profile %s", out.String())
	}
	cat.Classes["seat-linux-arm64-v1"].Agents["claude@2.1.280"] = seatdecl.Agent{Archive: "/claude.tar", ArchiveSHA256: strings.Repeat("f", 64)}
	b, _ = json.Marshal(cat)
	os.WriteFile(classes, b, 0o600)
	if code := seatProfileCmd([]string{"-root", root, "-classes", classes, "-source-bundle", "/owner/zeocreator.tar.gz", "zeocreator"}, &out, &errb); code != 1 {
		t.Fatal("an agent not qualified interactively was compiled")
	}
}

// A declaration whose sources the environment can't all carry is refused,
// never compiled without the ones it drops.
func TestSeatProfileRefusesASourceTheEnvironmentDoesntCarry(t *testing.T) {
	root, store, _ := lifecycleHost(t)
	declareSeat(t, store, "2.1.280")
	var out, errb bytes.Buffer
	if code := seatProfileCmd([]string{"-root", root, "-classes", catalogue(t), "-source-bundle", "/owner/zeocreator.tar.gz", "zeocreator"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "a records source") {
		t.Fatalf("a records source was dropped: %d %s %s", code, out.String(), errb.String())
	}
}

// up runs the seat up command and returns its exit and stderr.
func up(root, rec, classes string) (int, string) {
	var out, errb bytes.Buffer
	code := seatUpCmd([]string{"-root", root, "-records", rec, "-here", "pomar:macbook", "-classes", classes, "zeocreator"}, &out, &errb)
	return code, errb.String()
}

// A running environment made from an older declaration is never reported
// as the seat, though the seat's location is current: up refuses, and
// leaves the environment and its incarnation as they are (the POMAR
// Codex's review of #103).
func TestSeatUpRefusesARunningEnvironmentFromAStaleDeclaration(t *testing.T) {
	for name, change := range map[string]func(store seatapi.Store) string{
		"source":   func(s seatapi.Store) string { return declareSeatAt(t, s, "2.1.280", strings.Repeat("d", 40)) },
		"provider": func(s seatapi.Store) string { return declareSeat(t, s, "2.1.281") },
	} {
		t.Run(name, func(t *testing.T) {
			carryRecords(t)
			root, store, f := lifecycleHost(t)
			sum := declareSeat(t, store, "2.1.280")
			locations := seatdecl.Locations{Dir: filepath.Join(store.Root, "locations")}
			os.Mkdir(locations.Dir, 0o700)
			locations.Move("zeocreator", 0, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: sum})
			rec, classes := records(t), catalogue(t)
			installProfile(t, root, classes, f)
			if code, e := up(root, rec, classes); code != 0 {
				t.Fatalf("first up: %d %s", code, e)
			}
			newer := change(store)
			if _, err := locations.Move("zeocreator", 1, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: newer, Handover: "h", MechanismBinding: "m"}); err != nil {
				t.Fatal(err)
			}
			code, e := up(root, rec, classes)
			if code != 1 || !strings.Contains(e, "isn't the seat's current declaration") || !strings.Contains(e, "seatDeclarationSHA256") {
				t.Fatalf("a stale %s was reported as the seat: %d %s", name, code, e)
			}
			spec := f.envs["seat-zeocreator"]["spec"].(map[string]any)
			if f.starts != 1 || f.stops != 0 || f.envs["seat-zeocreator"]["phase"] != "running" || spec["incarnation"] != "running-1" {
				t.Fatalf("the stale environment was acted on: %d starts, %d stops, %v", f.starts, f.stops, f.envs["seat-zeocreator"])
			}
		})
	}
}

// An environment made from a profile the host's configuration still holds
// for an older catalogue is created but never started.
func TestSeatUpRefusesAnEnvironmentFromAStaleProfile(t *testing.T) {
	carryRecords(t)
	root, store, f := lifecycleHost(t)
	sum := declareSeat(t, store, "2.1.280")
	locations := seatdecl.Locations{Dir: filepath.Join(store.Root, "locations")}
	os.Mkdir(locations.Dir, 0o700)
	locations.Move("zeocreator", 0, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: sum})
	rec, classes := records(t), catalogue(t)
	installProfile(t, root, classes, f)
	writeClasses(t, classes, strings.Repeat("9", 64)) // the owner's catalogue moved on; the host's profile didn't
	code, e := up(root, rec, classes)
	if code != 1 || !strings.Contains(e, "imageDigest") {
		t.Fatalf("an environment from a stale profile: %d %s", code, e)
	}
	if f.creates != 1 || f.starts != 0 {
		t.Fatalf("%d creates and %d starts", f.creates, f.starts)
	}
}

// carryRecords runs a test as an environment that carries the records
// source too will (multi-source), so the rest of the lifecycle is exercised
// on a valid declaration.
func carryRecords(t *testing.T) {
	prev := carriedSources
	carriedSources = []string{"work", "records"}
	t.Cleanup(func() { carriedSources = prev })
}

// This host's location is its configuration's statement, never the
// caller's flag (ZEO-RT's review, SOW 84): a host naming no location starts
// no seat, a -here that disagrees with the host is refused, and -here may
// be left out.
func TestSeatUpTakesThisHostsLocationFromItsConfiguration(t *testing.T) {
	carryRecords(t)
	for _, c := range []struct {
		name, host, flag, want string
		code                   int
	}{
		{"a host naming no location", "", "pomar:macbook", "names no seat location", 1},
		{"a flag naming the seat's location on another host", "pomar:other", "pomar:macbook", "this host is pomar:other", 1},
		{"no flag, at the seat's location", "pomar:macbook", "", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, store, f := lifecycleHostAt(t, c.host)
			sum := declareSeat(t, store, "2.1.280")
			locations := seatdecl.Locations{Dir: filepath.Join(store.Root, "locations")}
			os.Mkdir(locations.Dir, 0o700)
			locations.Move("zeocreator", 0, seatdecl.Location{Where: "pomar:macbook", DeclarationSHA256: sum})
			rec, classes := records(t), catalogue(t)
			installProfile(t, root, classes, f)
			args := []string{"-root", root, "-records", rec, "-classes", classes}
			if c.flag != "" {
				args = append(args, "-here", c.flag)
			}
			var out, errb bytes.Buffer
			code := seatUpCmd(append(args, "zeocreator"), &out, &errb)
			if code != c.code || !strings.Contains(errb.String(), c.want) {
				t.Fatalf("%d %s", code, errb.String())
			}
			if c.code != 0 && (f.creates != 0 || f.starts != 0) {
				t.Fatalf("%d creates and %d starts on a refusal", f.creates, f.starts)
			}
		})
	}
}
