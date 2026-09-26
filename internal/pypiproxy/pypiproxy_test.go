package pypiproxy

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func sum(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

func TestParseLock(t *testing.T) {
	a, b, c := sum("a"), sum("b"), sum("c")
	lock, err := ParseLock(strings.NewReader(`# written by uv export
--index-url https://pypi.org/simple
requests==2.32.3 \
    --hash=sha256:` + a + ` \
    --hash=sha256:` + b + `
    # via some-tool
idna==3.10 --hash=sha256:` + c + ` # inline comment
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(lock) != 3 || !lock[a] || !lock[b] || !lock[c] {
		t.Fatalf("lock = %v", lock)
	}
	for name, text := range map[string]string{
		"no hash":        "requests==2.32.3\n",
		"md5":            "requests==2.32.3 --hash=md5:0123456789abcdef0123456789abcdef\n",
		"short sha256":   "requests==2.32.3 --hash=sha256:abc\n",
		"upper case":     "requests==2.32.3 --hash=sha256:" + strings.ToUpper(a) + "\n",
		"empty":          "# nothing\n",
		"second no hash": "idna==3.10 --hash=sha256:" + c + "\nrequests==2.32.3\n",
	} {
		if l, err := ParseLock(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted: %v", name, l)
		}
	}
}

func TestResolve(t *testing.T) {
	in := sum("locked")
	p := &Proxy{Lock: Lock{in: true}, Cache: t.TempDir()}
	for _, tc := range []struct {
		path    string
		want    Route
		refused bool
	}{
		{path: "/simple/requests/", want: Route{Project: "requests"}},
		{path: "/simple/zope-interface/", want: Route{Project: "zope-interface"}},
		{path: "/files/" + in + "/requests-2.32.3-py3-none-any.whl", want: Route{SHA256: in, File: "requests-2.32.3-py3-none-any.whl"}},
		{path: "/files/" + in + "/requests-2.32.3.tar.gz", want: Route{SHA256: in, File: "requests-2.32.3.tar.gz"}},
		{path: "/files/" + sum("not locked") + "/requests-2.32.3.tar.gz", refused: true},
		{path: "/simple/", refused: true},
		{path: "/simple/requests", refused: true},
		{path: "/simple/Requests/", refused: true}, // only normalized names
		{path: "/simple/zope_interface/", refused: true},
		{path: "/simple/../etc/passwd/", refused: true},
		{path: "/simple/requests/../idna/", refused: true},
		{path: "//evil.example/simple/requests/", refused: true},
		{path: "/files/" + in + "/requests.exe", refused: true},
		{path: "/files/" + in + "/../" + in + "/x.whl", refused: true},
		{path: "/files/" + in + "/", refused: true},
		{path: "/packages/ab/cd/requests-2.32.3.tar.gz", refused: true},
		{path: "/", refused: true},
	} {
		rt, err := p.Resolve(tc.path)
		if tc.refused {
			if err == nil {
				t.Errorf("%s accepted: %+v", tc.path, rt)
			}
			continue
		}
		if err != nil || rt != tc.want {
			t.Errorf("%s = %+v, %v", tc.path, rt, err)
		}
	}
}

// fakeIndex stands in for both upstreams. Its project page for "demo" lists
// a locked wheel, an unlocked sdist, a locked file on a foreign host and a
// locked file whose bytes are not what the lock pins. It records every path
// it is asked for.
type fakeIndex struct {
	srv   *httptest.Server
	mu    sync.Mutex
	asked []string
}

const (
	wheel   = "wheel bytes"
	sdist   = "sdist bytes"
	foreign = "foreign bytes"
	liar    = "what the lock pins"
)

func newFakeIndex(t *testing.T) *fakeIndex {
	f := &fakeIndex{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.asked = append(f.asked, r.URL.Path)
		f.mu.Unlock()
		switch r.URL.Path {
		case "/simple/demo/":
			if r.Header.Get("Accept") != SimpleJSON {
				http.Error(w, "want json", http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Content-Type", SimpleJSON)
			base := f.srv.URL + "/packages/aa/bb/"
			fmt.Fprintf(w, `{"meta":{"api-version":"1.1"},"name":"demo","files":[
				{"filename":"demo-1.0-py3-none-any.whl","url":%q,"hashes":{"sha256":%q},"requires-python":">=3.9","core-metadata":{"sha256":"x"}},
				{"filename":"demo-1.0.tar.gz","url":%q,"hashes":{"sha256":%q}},
				{"filename":"demo-0.9-py3-none-any.whl","url":"https://elsewhere.example/demo-0.9-py3-none-any.whl","hashes":{"sha256":%q}},
				{"filename":"demo-0.8-py3-none-any.whl","url":%q,"hashes":{"sha256":%q}}
			]}`, base+"demo-1.0-py3-none-any.whl", sum(wheel), base+"demo-1.0.tar.gz", sum(sdist), sum(foreign),
				base+"demo-0.8-py3-none-any.whl", sum(liar))
		case "/simple/html-only/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html></html>")
		case "/simple/moved/":
			http.Redirect(w, r, "https://elsewhere.example/simple/moved/", http.StatusMovedPermanently)
		case "/packages/aa/bb/demo-1.0-py3-none-any.whl":
			fmt.Fprint(w, wheel)
		case "/packages/aa/bb/demo-1.0.tar.gz":
			fmt.Fprint(w, sdist)
		case "/packages/aa/bb/demo-0.8-py3-none-any.whl":
			fmt.Fprint(w, "not "+liar)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIndex) count(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.asked {
		if a == p {
			n++
		}
	}
	return n
}

func setup(t *testing.T) (*fakeIndex, *httptest.Server, string) {
	up := newFakeIndex(t)
	cache := t.TempDir()
	p := &Proxy{
		Lock:  Lock{sum(wheel): true, sum(foreign): true, sum(liar): true},
		Cache: cache, IndexUpstream: up.srv.URL + "/simple", FilesUpstream: up.srv.URL,
	}
	srv := httptest.NewServer(p.Handler(io.Discard))
	t.Cleanup(srv.Close)
	return up, srv, cache
}

func get(t *testing.T, srv *httptest.Server, p string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + p)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestPageListsOnlyLockedFilesOnTheFilesHost(t *testing.T) {
	_, srv, _ := setup(t)
	code, ct, body := get(t, srv, "/simple/demo/")
	if code != http.StatusOK || ct != SimpleJSON {
		t.Fatalf("page = %d %q %s", code, ct, body)
	}
	var pg page
	if err := json.Unmarshal([]byte(body), &pg); err != nil {
		t.Fatal(err)
	}
	// The unlocked sdist and the file on a foreign host are not listed; the
	// liar is listed (its page hash is locked) but is refused when fetched.
	if len(pg.Files) != 2 {
		t.Fatalf("files = %+v", pg.Files)
	}
	f := pg.Files[0]
	if f.Filename != "demo-1.0-py3-none-any.whl" || f.URL != "/files/"+sum(wheel)+"/demo-1.0-py3-none-any.whl" ||
		len(f.Hashes) != 1 || f.Hashes["sha256"] != sum(wheel) || f.RequiresPython != ">=3.9" {
		t.Fatalf("file = %+v", f)
	}
	if strings.Contains(body, "core-metadata") || strings.Contains(body, "elsewhere") || strings.Contains(body, "demo-1.0.tar.gz") {
		t.Fatalf("page leaks: %s", body)
	}
}

func TestLockedFileIsCheckedCachedAndServed(t *testing.T) {
	up, srv, cache := setup(t)
	p := "/files/" + sum(wheel) + "/demo-1.0-py3-none-any.whl"
	if code, _, _ := get(t, srv, p); code != http.StatusNotFound {
		t.Fatalf("a file before its project page = %d, want 404", code)
	}
	get(t, srv, "/simple/demo/")
	for i := 0; i < 2; i++ {
		if code, _, body := get(t, srv, p); code != http.StatusOK || body != wheel {
			t.Fatalf("get %d = %d %q", i, code, body)
		}
	}
	if n := up.count("/packages/aa/bb/demo-1.0-py3-none-any.whl"); n != 1 {
		t.Fatalf("upstream fetched %d times, want 1", n)
	}
	if b, err := os.ReadFile(filepath.Join(cache, sum(wheel))); err != nil || string(b) != wheel {
		t.Fatalf("cache = %q, %v", b, err)
	}
}

// The ruled test (POMAR-SOW-05 §7, PR 3): a file whose hash is not in the
// lock is refused, and the upstream is never asked for it.
func TestFileNotInLockIsRefused(t *testing.T) {
	up, srv, cache := setup(t)
	get(t, srv, "/simple/demo/")
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		req, _ := http.NewRequest(m, srv.URL+"/files/"+sum(sdist)+"/demo-1.0.tar.gz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s unlocked file = %d, want 404", m, resp.StatusCode)
		}
	}
	if n := up.count("/packages/aa/bb/demo-1.0.tar.gz"); n != 0 {
		t.Fatalf("upstream asked for the unlocked file %d times", n)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("cache holds %v", ents)
	}
}

func TestFileWhoseBytesDoNotMatchIsRefused(t *testing.T) {
	_, srv, cache := setup(t)
	get(t, srv, "/simple/demo/")
	if code, _, body := get(t, srv, "/files/"+sum(liar)+"/demo-0.8-py3-none-any.whl"); code != http.StatusBadGateway || strings.Contains(body, liar) {
		t.Fatalf("mismatched file = %d %q, want 502", code, body)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("cache holds %v", ents)
	}
}

func TestLockedFileOnAForeignHostIsNotFetched(t *testing.T) {
	up, srv, _ := setup(t)
	get(t, srv, "/simple/demo/")
	if code, _, _ := get(t, srv, "/files/"+sum(foreign)+"/demo-0.9-py3-none-any.whl"); code != http.StatusNotFound {
		t.Fatalf("foreign file = %d, want 404", code)
	}
	for _, a := range up.asked {
		if strings.Contains(a, "0.9") {
			t.Fatalf("upstream asked for %s", a)
		}
	}
}

func TestPageRefusals(t *testing.T) {
	_, srv, _ := setup(t)
	for p, want := range map[string]int{
		"/simple/html-only/": http.StatusBadGateway, // only the JSON form is read
		"/simple/moved/":     http.StatusBadGateway, // a redirect is never followed
		"/simple/nothing/":   http.StatusNotFound,
	} {
		if code, _, _ := get(t, srv, p); code != want {
			t.Errorf("%s = %d, want %d", p, code, want)
		}
	}
}

// raw sends one request line as written, as a guest could.
func raw(t *testing.T, srv *httptest.Server, line string) string {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%s\r\nHost: x\r\nConnection: close\r\n\r\n", line)
	status, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(status)
}

func TestNotAForwardProxy(t *testing.T) {
	_, srv, _ := setup(t)
	for line, want := range map[string]string{
		"CONNECT pypi.org:443 HTTP/1.1":             "405",
		"POST /simple/demo/ HTTP/1.1":               "405",
		"GET http://pypi.org/simple/demo/ HTTP/1.1": "400",
		"GET /simple/demo/?x=1 HTTP/1.1":            "400",
		"GET /simple/../../etc/passwd HTTP/1.1":     "404",
	} {
		if got := raw(t, srv, line); !strings.Contains(got, " "+want+" ") {
			t.Errorf("%s -> %s, want %s", line, got, want)
		}
	}
}
