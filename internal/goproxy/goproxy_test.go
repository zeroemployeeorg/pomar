package goproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolve(t *testing.T) {
	p := &Proxy{Cache: t.TempDir()}
	for _, tc := range []struct {
		path, upstream, cache string
		local, refused        bool
	}{
		{path: "/golang.org/x/sys/@v/v0.47.0.zip", upstream: ModuleProxy + "/golang.org/x/sys/@v/v0.47.0.zip", cache: "mod/golang.org/x/sys/@v/v0.47.0.zip"},
		{path: "/github.com/!burnt!sushi/toml/@v/v1.3.2.mod", upstream: ModuleProxy + "/github.com/!burnt!sushi/toml/@v/v1.3.2.mod", cache: "mod/github.com/!burnt!sushi/toml/@v/v1.3.2.mod"},
		{path: "/modernc.org/sqlite/@v/list", upstream: ModuleProxy + "/modernc.org/sqlite/@v/list"},
		{path: "/modernc.org/sqlite/@latest", upstream: ModuleProxy + "/modernc.org/sqlite/@latest"},
		{path: "/sumdb/sum.golang.org/supported", local: true},
		{path: "/sumdb/sum.golang.org/latest", upstream: SumDB + "/latest"},
		{path: "/sumdb/sum.golang.org/lookup/golang.org/x/sys@v0.47.0", upstream: SumDB + "/lookup/golang.org/x/sys@v0.47.0", cache: "sumdb/lookup/golang.org/x/sys@v0.47.0"},
		{path: "/sumdb/sum.golang.org/tile/8/0/x123/456", upstream: SumDB + "/tile/8/0/x123/456", cache: "sumdb/tile/8/0/x123/456"},
		{path: "/sumdb/sum.golang.org/tile/8/data/x123/456.p/7", upstream: SumDB + "/tile/8/data/x123/456.p/7", cache: "sumdb/tile/8/data/x123/456.p/7"},
		{path: "/", refused: true},
		{path: "/etc/passwd", refused: true},
		{path: "/golang.org/x/sys/@v/../../../../etc/passwd", refused: true},
		{path: "/golang.org/x/sys/@v/v0.47.0.exe", refused: true},
		{path: "/Golang.org/x/sys/@v/list", refused: true}, // upper case is never sent unescaped
		{path: "/sumdb/other.example/latest", refused: true},
		{path: "/sumdb/sum.golang.org/../../x", refused: true},
		{path: "//evil.example/@v/list", refused: true},
	} {
		rt, err := p.Resolve(tc.path)
		if tc.refused {
			if err == nil {
				t.Errorf("%s accepted: %+v", tc.path, rt)
			}
			continue
		}
		if err != nil || rt.Upstream != tc.upstream || rt.Cache != tc.cache || rt.Local != tc.local {
			t.Errorf("%s = %+v, %v", tc.path, rt, err)
		}
	}
}

// fakeUpstream stands in for both upstreams and counts its requests.
func fakeUpstream(t *testing.T) (*httptest.Server, *int64) {
	var n int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		if strings.HasSuffix(r.URL.Path, "/missing/@v/list") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "body of %s", r.URL.Path)
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func TestServeCachesImmutableResponses(t *testing.T) {
	up, n := fakeUpstream(t)
	cache := t.TempDir()
	p := &Proxy{Cache: cache, ModuleUpstream: up.URL, SumUpstream: up.URL}
	srv := httptest.NewServer(p.Handler(io.Discard))
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for i := 0; i < 2; i++ {
		if code, body := get("/golang.org/x/sys/@v/v0.47.0.zip"); code != 200 || body != "body of /golang.org/x/sys/@v/v0.47.0.zip" {
			t.Fatalf("zip: %d %q", code, body)
		}
	}
	if atomic.LoadInt64(n) != 1 {
		t.Fatalf("upstream hit %d times for one immutable file, want 1", *n)
	}
	if _, err := os.Stat(filepath.Join(cache, "mod/golang.org/x/sys/@v/v0.47.0.zip")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		get("/golang.org/x/sys/@v/list")
	}
	if atomic.LoadInt64(n) != 3 {
		t.Fatalf("a list was cached: upstream hits %d, want 3", *n)
	}
	if code, _ := get("/example.com/missing/@v/list"); code != 404 {
		t.Fatalf("upstream 404 passed as %d", code)
	}
	if code, _ := get("/sumdb/sum.golang.org/supported"); code != 200 {
		t.Fatalf("supported = %d", code)
	}
	if code, body := get("/sumdb/sum.golang.org/lookup/golang.org/x/sys@v0.47.0"); code != 200 || body != "body of /lookup/golang.org/x/sys@v0.47.0" {
		t.Fatalf("lookup: %d %q (the sumdb prefix must be stripped upstream)", code, body)
	}
	matches, _ := filepath.Glob(filepath.Join(cache, "mod/golang.org/x/sys/@v/.part-*"))
	if len(matches) != 0 {
		t.Fatalf("partial files left: %v", matches)
	}
}

// The negative rows: this is not a forward proxy.
func TestRefusesAnythingButModuleReads(t *testing.T) {
	up, n := fakeUpstream(t)
	p := &Proxy{Cache: t.TempDir(), ModuleUpstream: up.URL, SumUpstream: up.URL}
	srv := httptest.NewServer(p.Handler(io.Discard))
	defer srv.Close()
	raw := func(req string) string {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprint(c, req)
		line, _ := bufio.NewReader(c).ReadString('\n')
		return strings.TrimSpace(line)
	}
	for _, tc := range []struct{ name, req, want string }{
		{"CONNECT", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n", "405"},
		{"absolute-form GET", "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", "400"},
		{"absolute-form module path", "GET http://example.com/golang.org/x/sys/@v/list HTTP/1.1\r\nHost: example.com\r\n\r\n", "400"},
		{"POST", "POST /golang.org/x/sys/@v/list HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", "405"},
		{"query", "GET /golang.org/x/sys/@v/list?x=1 HTTP/1.1\r\nHost: x\r\n\r\n", "400"},
		{"other path", "GET /robots.txt HTTP/1.1\r\nHost: x\r\n\r\n", "404"},
	} {
		if got := raw(tc.req); !strings.Contains(got, " "+tc.want+" ") {
			t.Errorf("%s: status line %q, want %s", tc.name, got, tc.want)
		}
	}
	if atomic.LoadInt64(n) != 0 {
		t.Fatalf("a refused request reached the upstream (%d)", *n)
	}
}
