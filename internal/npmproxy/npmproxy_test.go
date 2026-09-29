package npmproxy

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func integrity(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

func sha1Integrity(b []byte) string {
	s := sha1.Sum(b)
	return "sha1-" + base64.StdEncoding.EncodeToString(s[:])
}

// lockJSON builds a package-lock.json: entries are key -> {resolved, integrity, flags}.
func lockJSON(t *testing.T, version int, entries map[string]map[string]any) string {
	t.Helper()
	pkgs := map[string]any{"": map[string]any{"name": "demo", "version": "1.0.0"}}
	for k, v := range entries {
		pkgs[k] = v
	}
	b, err := json.Marshal(map[string]any{"name": "demo", "lockfileVersion": version, "requires": true, "packages": pkgs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	leftPad = []byte("left-pad tarball bytes")
	scoped  = []byte("scoped tarball bytes")
)

func goodEntries(up string) map[string]map[string]any {
	return map[string]map[string]any{
		"node_modules/left-pad":                        {"version": "1.3.0", "resolved": up + "/left-pad/-/left-pad-1.3.0.tgz", "integrity": sha1Integrity(leftPad) + " " + integrity(leftPad)},
		"node_modules/@scope/thing":                    {"version": "2.0.0", "resolved": up + "/@scope/thing/-/thing-2.0.0.tgz", "integrity": integrity(scoped)},
		"node_modules/@scope/thing/node_modules/inner": {"inBundle": true, "version": "0.1.0"},
	}
}

func TestParseLockAcceptsARegistryLock(t *testing.T) {
	for _, v := range []int{2, 3} {
		l, err := ParseLock(strings.NewReader(lockJSON(t, v, goodEntries(Upstream))), "")
		if err != nil {
			t.Fatalf("lockfileVersion %d: %v", v, err)
		}
		want := "/@scope/thing/-/thing-2.0.0.tgz /left-pad/-/left-pad-1.3.0.tgz"
		if got := strings.Join(l.Paths(), " "); got != want {
			t.Fatalf("paths %s, want %s", got, want)
		}
		if s := sha512.Sum512(leftPad); l["/left-pad/-/left-pad-1.3.0.tgz"] != fmt.Sprintf("%x", s) {
			t.Fatal("the sha512 is not the integrity's")
		}
	}
}

// Each kind of source the proxy cannot serve refuses the whole lock, naming
// the entry.
func TestParseLockRefuses(t *testing.T) {
	good := goodEntries(Upstream)
	for name, tc := range map[string]struct {
		version int
		entry   map[string]any
		want    string
	}{
		"lockfileVersion 1":    {1, nil, "lockfileVersion 1"},
		"an sha1 alone":        {2, map[string]any{"resolved": Upstream + "/x/-/x-1.0.0.tgz", "integrity": sha1Integrity(leftPad)}, "no sha512"},
		"no integrity":         {2, map[string]any{"resolved": Upstream + "/x/-/x-1.0.0.tgz"}, "no sha512"},
		"a malformed sha512":   {2, map[string]any{"resolved": Upstream + "/x/-/x-1.0.0.tgz", "integrity": "sha512-AAAA"}, "malformed"},
		"a git dependency":     {2, map[string]any{"resolved": "git+ssh://git@github.com/x/y.git#abc", "integrity": integrity(leftPad)}, "not a tarball"},
		"a file dependency":    {2, map[string]any{"resolved": "file:../x", "integrity": integrity(leftPad)}, "not a tarball"},
		"another host":         {2, map[string]any{"resolved": "https://registry.example.org/x/-/x-1.0.0.tgz", "integrity": integrity(leftPad)}, "not a tarball"},
		"plain http":           {2, map[string]any{"resolved": "http://registry.npmjs.org/x/-/x-1.0.0.tgz", "integrity": integrity(leftPad)}, "not a tarball"},
		"a query":              {2, map[string]any{"resolved": Upstream + "/x/-/x-1.0.0.tgz?a=b", "integrity": integrity(leftPad)}, "not a tarball"},
		"not a tarball path":   {2, map[string]any{"resolved": Upstream + "/x", "integrity": integrity(leftPad)}, "not a tarball"},
		"a workspace link":     {2, map[string]any{"resolved": "packages/x", "link": true}, "link to a local folder"},
		"no resolved URL":      {2, map[string]any{"version": "1.0.0", "integrity": integrity(leftPad)}, "no resolved URL"},
		"two different sha512": {2, map[string]any{"resolved": Upstream + "/x/-/x-1.0.0.tgz", "integrity": integrity(leftPad) + " " + integrity(scoped)}, "two sha512s"},
	} {
		entries := map[string]map[string]any{}
		for k, v := range good {
			entries[k] = v
		}
		if tc.entry != nil {
			entries["node_modules/bad"] = tc.entry
		}
		_, err := ParseLock(strings.NewReader(lockJSON(t, tc.version, entries)), "")
		if err == nil || !strings.Contains(err.Error(), tc.want) || (tc.entry != nil && !strings.Contains(err.Error(), "node_modules/bad")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseLock(strings.NewReader(lockJSON(t, 3, nil)), ""); err == nil || !strings.Contains(err.Error(), "no registry packages") {
		t.Errorf("an empty lock: %v", err)
	}
}

// fixture is a fake registry that counts its hits, and a proxy over it.
func fixture(t *testing.T, serve map[string][]byte) (*Proxy, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/moved/-/moved-1.0.0.tgz" {
			http.Redirect(w, r, "/left-pad/-/left-pad-1.3.0.tgz", http.StatusFound)
			return
		}
		b, ok := serve[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(up.Close)
	entries := goodEntries(up.URL)
	entries["node_modules/moved"] = map[string]any{"resolved": up.URL + "/moved/-/moved-1.0.0.tgz", "integrity": integrity(leftPad)}
	lock, err := ParseLock(strings.NewReader(lockJSON(t, 3, entries)), up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Lock: lock, Cache: t.TempDir(), Upstream: up.URL}
	srv := httptest.NewServer(p.Handler(io.Discard))
	t.Cleanup(srv.Close)
	return p, srv, &hits
}

func get(t *testing.T, method, url string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestALockedTarballIsCheckedCachedAndServed(t *testing.T) {
	_, srv, hits := fixture(t, map[string][]byte{"/left-pad/-/left-pad-1.3.0.tgz": leftPad, "/@scope/thing/-/thing-2.0.0.tgz": scoped})
	for i := 0; i < 2; i++ {
		if code, b := get(t, "GET", srv.URL+"/left-pad/-/left-pad-1.3.0.tgz"); code != 200 || string(b) != string(leftPad) {
			t.Fatalf("round %d: %d %q", i, code, b)
		}
	}
	if code, b := get(t, "GET", srv.URL+"/@scope/thing/-/thing-2.0.0.tgz"); code != 200 || string(b) != string(scoped) {
		t.Fatalf("scoped: %d %q", code, b)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("%d upstream fetches, want 2: the second request must come from the cache", n)
	}
	if code, b := get(t, "HEAD", srv.URL+"/left-pad/-/left-pad-1.3.0.tgz"); code != 200 || len(b) != 0 {
		t.Fatalf("HEAD: %d %q", code, b)
	}
}

// A tarball whose bytes are not the lock's is not served, and not cached.
func TestAMismatchedTarballIsRefused(t *testing.T) {
	p, srv, hits := fixture(t, map[string][]byte{"/left-pad/-/left-pad-1.3.0.tgz": []byte("something else")})
	for i := 0; i < 2; i++ {
		if code, _ := get(t, "GET", srv.URL+"/left-pad/-/left-pad-1.3.0.tgz"); code != http.StatusBadGateway {
			t.Fatalf("round %d: %d, want 502", i, code)
		}
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("%d upstream fetches: a mismatch must not be cached", n)
	}
	if ents, _ := os.ReadDir(p.Cache); len(ents) != 0 {
		t.Fatalf("the cache holds %d files", len(ents))
	}
}

// Nothing outside the lock is fetched: not an unlocked tarball, not a
// packument, not a path with dot segments; and a redirect is never followed.
func TestOnlyLockedPathsAreServed(t *testing.T) {
	_, srv, hits := fixture(t, map[string][]byte{"/left-pad/-/left-pad-1.3.0.tgz": leftPad, "/other/-/other-1.0.0.tgz": leftPad, "/left-pad": []byte("{}")})
	for _, p := range []string{
		"/other/-/other-1.0.0.tgz",            // not in the lock
		"/left-pad",                           // a packument
		"/left-pad/-/left-pad-1.3.1.tgz",      // another version
		"/x/../left-pad/-/left-pad-1.3.0.tgz", // a dot segment
		"//left-pad/-/left-pad-1.3.0.tgz",
		"/",
	} {
		if code, _ := get(t, "GET", srv.URL+p); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, code)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d upstream fetches for refused paths", n)
	}
	if code, _ := get(t, "GET", srv.URL+"/moved/-/moved-1.0.0.tgz"); code != http.StatusBadGateway {
		t.Errorf("a redirect upstream: %d, want 502", code)
	}
	if code, _ := get(t, "POST", srv.URL+"/left-pad/-/left-pad-1.3.0.tgz"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
	if code, _ := get(t, "GET", srv.URL+"/left-pad/-/left-pad-1.3.0.tgz?x=1"); code != http.StatusBadRequest {
		t.Errorf("a query: %d", code)
	}
}

// With NPMPROXY_REAL_LOCK naming a package-lock.json, the proxy fetches each
// of its tarballs from the real registry and checks it against the lock. It
// is skipped otherwise: it needs the network.
func TestRealRegistry(t *testing.T) {
	lf := os.Getenv("NPMPROXY_REAL_LOCK")
	if lf == "" {
		t.Skip("set NPMPROXY_REAL_LOCK to a package-lock.json to fetch from the real registry")
	}
	f, err := os.Open(lf)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lock, err := ParseLock(f, "")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Lock: lock, Cache: t.TempDir()}
	srv := httptest.NewServer(p.Handler(os.Stderr))
	defer srv.Close()
	for _, path := range lock.Paths() {
		code, b := get(t, "GET", srv.URL+path)
		s := sha512.Sum512(b)
		if code != 200 || fmt.Sprintf("%x", s) != lock[path] {
			t.Fatalf("%s: %d, %d bytes", path, code, len(b))
		}
		t.Logf("%s: %d bytes, sha512 matches the lock", path, len(b))
	}
	// A path the lock does not name is refused without a fetch.
	if code, _ := get(t, "GET", srv.URL+"/left-pad/-/left-pad-1.2.0.tgz"); code != http.StatusNotFound {
		t.Fatalf("an unlocked version: %d", code)
	}
}
