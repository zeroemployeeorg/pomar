package seatapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/seatdecl"
)

const pin = "c3e64a5000000000000000000000000000000001"

func declaration(class string) []byte {
	d := seatdecl.Declaration{
		Schema: seatdecl.Schema, Seat: "zeocreator", Project: "zeocreator",
		Sources: []seatdecl.Source{
			{Role: "work", Repository: "zeroemployeeorg/zeocreator", Fork: "architect/zeocreator", Branch: "main", Commit: pin, Tree: pin, Destination: "work", Writable: true},
			{Role: "records", Repository: "owner/records", Fork: "architect/records", Branch: "records/zeocreator", Commit: pin, Tree: pin, Destination: "records", Writable: true},
		},
		Class: class, Resources: seatdecl.Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
		Provider: seatdecl.Provider{Agent: "claude", Version: "2.1.280", Account: seatdecl.NotEstablished},
		Resume:   seatdecl.Resume{Policy: "fresh-from-sow", HandoverSOW: "records/zeocreator@x:SOW-04"},
		Identity: seatdecl.Identity{Role: seatdecl.NotEstablished, Forks: []string{"architect/zeocreator", "architect/records"},
			Push: []string{"refs/heads/zeocreator/*"}, Permissions: map[string]string{"contents": "write", "metadata": "read"}},
	}
	b, _ := json.Marshal(d)
	return b
}

func store(t *testing.T) Store {
	t.Helper()
	dir, _ := os.MkdirTemp("", "seats")
	dir, _ = filepath.EvalSymlinks(dir)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return Store{Root: dir}
}

func call(t *testing.T, s Store, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, r))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func declare(t *testing.T, s Store, expected string, raw []byte) (int, map[string]any) {
	return call(t, s, "PUT", "/v1/seats/zeocreator/declaration", map[string]string{"expected_sha256": expected, "declaration_base64": base64.StdEncoding.EncodeToString(raw)})
}

func TestADeclarationIsRecordedByCompareAndSetOnItsHash(t *testing.T) {
	s := store(t)
	if code, out := call(t, s, "GET", "/v1/seats", nil); code != 200 || len(out["seats"].([]any)) != 0 {
		t.Fatal(code, out)
	}
	code, first := declare(t, s, "", declaration("seat-linux-arm64-v1"))
	if code != 200 || first["declaration_revision"].(float64) != 1 {
		t.Fatal(code, first)
	}
	sum := first["declaration_sha256"].(string)
	if code, _ := declare(t, s, "", declaration("seat-linux-arm64-v2")); code != 409 {
		t.Fatalf("a stale expected hash: %d", code)
	}
	if code, again := declare(t, s, sum, declaration("seat-linux-arm64-v1")); code != 200 || again["declaration_revision"].(float64) != 1 {
		t.Fatalf("the same bytes again made a revision: %d %v", code, again)
	}
	code, second := declare(t, s, sum, declaration("seat-linux-arm64-v2"))
	if code != 200 || second["declaration_revision"].(float64) != 2 {
		t.Fatal(code, second)
	}
	// The first revision is kept, byte for byte.
	b, _ := os.ReadFile(filepath.Join(s.Root, "declarations", "zeocreator", "declaration-000000000001.json"))
	if !bytes.Equal(b, declaration("seat-linux-arm64-v1")) {
		t.Fatal("an earlier declaration was rewritten")
	}
	code, list := call(t, s, "GET", "/v1/seats", nil)
	seats := list["seats"].([]any)
	if code != 200 || len(seats) != 1 || seats[0].(map[string]any)["role"] != "NOT_ESTABLISHED" {
		t.Fatal(code, list)
	}
}

func TestDeclarationsAreRefusedWhenInvalidOrForAnotherSeat(t *testing.T) {
	s := store(t)
	other := bytes.Replace(declaration("seat-linux-arm64-v1"), []byte(`"seat":"zeocreator"`), []byte(`"seat":"zeonewsroom"`), 1)
	for name, raw := range map[string][]byte{
		"another seat's declaration": other,
		"an invalid declaration":     []byte(`{"schema":"x"}`),
		"a credential-shaped field":  bytes.Replace(declaration("seat-linux-arm64-v1"), []byte(`"seat":`), []byte(`"token":"x","seat":`), 1),
	} {
		if code, _ := declare(t, s, "", raw); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := call(t, s, "PUT", "/v1/seats/zeocreator/declaration", map[string]string{"declaration_base64": "!!", "expected_sha256": ""}); code != 400 {
		t.Error("non-base64 accepted")
	}
	if code, _ := call(t, s, "GET", "/v1/seats/zeocreator", nil); code != 404 {
		t.Error("an undeclared seat was reported")
	}
}

func TestALocationNamesTheCurrentDeclarationAndMovesByRevision(t *testing.T) {
	s := store(t)
	if code, _ := call(t, s, "POST", "/v1/seats/zeocreator/location", map[string]any{"expected_revision": 0, "location": map[string]string{"where": "mac-mini-naked", "declaration_sha256": strings.Repeat("d", 64)}}); code != 400 {
		t.Fatalf("a location for an undeclared seat: %d", code)
	}
	_, d := declare(t, s, "", declaration("seat-linux-arm64-v1"))
	sum := d["declaration_sha256"].(string)
	if code, _ := call(t, s, "POST", "/v1/seats/zeocreator/location", map[string]any{"expected_revision": 0, "location": map[string]string{"where": "mac-mini-naked", "declaration_sha256": strings.Repeat("d", 64)}}); code != 409 {
		t.Fatalf("a location naming another declaration: %d", code)
	}
	if code, out := call(t, s, "POST", "/v1/seats/zeocreator/location", map[string]any{"expected_revision": 0, "location": map[string]string{"where": "mac-mini-naked", "declaration_sha256": sum}}); code != 200 {
		t.Fatal(code, out)
	}
	if code, _ := call(t, s, "POST", "/v1/seats/zeocreator/location", map[string]any{"expected_revision": 1, "location": map[string]string{"where": "pomar:macbook", "declaration_sha256": sum}}); code != 400 {
		t.Fatalf("a move without evidence: %d", code)
	}
	if code, _ := call(t, s, "POST", "/v1/seats/zeocreator/location", map[string]any{"expected_revision": 0, "location": map[string]string{"where": "pomar:macbook", "declaration_sha256": sum, "handover": "h", "mechanism_binding": "m"}}); code != 409 {
		t.Fatalf("a stale revision: %d", code)
	}
	code, out := call(t, s, "GET", "/v1/seats/zeocreator", nil)
	seat := out["seat"].(map[string]any)
	if code != 200 || seat["location"].(map[string]any)["where"] != "mac-mini-naked" || seat["location_matches_declaration"] != true {
		t.Fatal(code, out)
	}
}

func TestConcurrentDeclarersOneWins(t *testing.T) {
	s := store(t)
	_, d := declare(t, s, "", declaration("seat-linux-arm64-v1"))
	sum := d["declaration_sha256"].(string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Declare("zeocreator", sum, declaration("seat-linux-arm64-v"+string(rune('a'+i)))); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d declarers won", wins)
	}
}

func TestRequestsAreStrict(t *testing.T) {
	s := store(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("PUT", "/v1/seats/zeocreator/declaration", strings.NewReader(`{"expected_sha256":"","declaration_base64":"","extra":1}`)))
	if w.Code != 400 {
		t.Fatalf("an unknown request field: %d", w.Code)
	}
	if code, _ := call(t, s, "GET", "/v1/seats/..", nil); code == 200 {
		t.Fatal("an invalid seat name was served")
	}
	os.Chmod(s.Root, 0o755)
	if code, _ := call(t, s, "GET", "/v1/seats", nil); code != 500 {
		t.Fatal("a non-private root was served")
	}
}

// The POMAR Codex's counterexamples (PR93 review at 92e7f77), as given.
func TestOwnerReadRoutesDoNotInitializeStore(t *testing.T) {
	s := store(t)
	before, err := os.ReadDir(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatal("fixture not empty")
	}
	if code, _ := call(t, s, "GET", "/v1/seats", nil); code != 200 {
		t.Fatalf("GET %d", code)
	}
	after, err := os.ReadDir(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("read-only GET created %d store entries", len(after))
	}
}

func TestOwnerDeclareRefusesSymlinkedEmptySeatDirectory(t *testing.T) {
	s := store(t)
	for _, n := range []string{"declarations", "locations"} {
		if err := os.Mkdir(filepath.Join(s.Root, n), 0700); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(s.Root, "owned-outside-fixture")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Root, "declarations", "zeocreator")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Declare("zeocreator", "", declaration("seat-linux-arm64-v1")); err == nil {
		t.Fatal("symlinked empty seat dir accepted; declaration was written outside declarations")
	}
	files, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatal("refusal still wrote outside declaration directory")
	}
}

// The same boundary for the other reads and the location record.
func TestReadsCreateNothingAndLinkedSeatDirectoriesAreRefused(t *testing.T) {
	s := store(t)
	for _, path := range []string{"/v1/seats/zeocreator"} {
		call(t, s, "GET", path, nil)
	}
	if entries, _ := os.ReadDir(s.Root); len(entries) != 0 {
		t.Fatalf("a read of one seat created %d entries", len(entries))
	}
	_, d := declare(t, s, "", declaration("seat-linux-arm64-v1"))
	outside := filepath.Join(s.Root, "outside-locations")
	os.Mkdir(outside, 0o700)
	os.Symlink(outside, filepath.Join(s.Root, "locations", "zeocreator"))
	if code, _ := call(t, s, "GET", "/v1/seats/zeocreator", nil); code == 200 {
		t.Fatal("a linked location directory was read")
	}
	code, _ := call(t, s, "POST", "/v1/seats/zeocreator/location", map[string]any{"expected_revision": 0, "location": map[string]string{"where": "mac-mini-naked", "declaration_sha256": d["declaration_sha256"].(string)}})
	if code == 200 {
		t.Fatal("a location was written through a linked directory")
	}
	if files, _ := os.ReadDir(outside); len(files) != 0 {
		t.Fatal("a location was written outside the store")
	}
}
