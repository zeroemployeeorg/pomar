// Package goproxy is the host end of a guest's only egress: a Go module proxy
// that serves the GOPROXY protocol's read paths and the checksum database's
// paths, and nothing else. It is not a forward proxy: it never takes a host
// from a request, it refuses CONNECT and every method but GET and HEAD, and
// it fetches only from the Go module proxy and the checksum database.
//
// Immutable responses (a version's .info, .mod and .zip; checksum database
// lookups and tiles) are kept in a cache directory, which the manager
// ledgers as a cache object under a size budget.
package goproxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The only upstreams.
const (
	ModuleProxy = "https://proxy.golang.org"
	SumDB       = "https://sum.golang.org"
	SumDBName   = "sum.golang.org"
)

// Proxy serves the allowed paths from its cache or its upstreams.
type Proxy struct {
	// Cache is the directory for immutable responses; it must exist.
	Cache string
	// Client fetches from the upstreams; nil means a client with a timeout.
	Client *http.Client
	// Upstreams can be replaced in tests; empty means the real ones.
	ModuleUpstream, SumUpstream string

	once   sync.Once
	client *http.Client
}

func (p *Proxy) init() {
	p.once.Do(func() {
		p.client = p.Client
		if p.client == nil {
			p.client = &http.Client{Timeout: 5 * time.Minute}
		}
		if p.ModuleUpstream == "" {
			p.ModuleUpstream = ModuleProxy
		}
		if p.SumUpstream == "" {
			p.SumUpstream = SumDB
		}
	})
}

// Route is what an allowed request maps to.
type Route struct {
	Upstream string // full upstream URL
	Cache    string // cache path relative to the cache directory; "" = never cached
	Local    bool   // answered here (the sumdb "supported" probe)
}

// Module paths are escaped by the go command: lower case letters, digits
// and . - _ ~ / with upper case as !x. A version is escaped the same way.
var (
	modPath  = `[a-z0-9.\-_~!]+(?:/[a-z0-9.\-_~!]+)*`
	version  = `v[0-9A-Za-z.\-+!]+`
	vFile    = regexp.MustCompile(`^/(` + modPath + `)/@v/(` + version + `)\.(info|mod|zip)$`)
	vList    = regexp.MustCompile(`^/(` + modPath + `)/@v/list$`)
	latest   = regexp.MustCompile(`^/(` + modPath + `)/@latest$`)
	sumPath  = regexp.MustCompile(`^/sumdb/` + regexp.QuoteMeta(SumDBName) + `/(supported|latest|lookup/` + modPath + `@` + version + `|tile/[0-9]+/(?:data/)?[0-9x/]+(?:\.p/[0-9]+)?)$`)
	errRoute = errors.New("goproxy: not a module proxy path")
)

// Resolve maps a request path to its upstream, or refuses it. A path with a
// dot segment is refused before matching.
func (p *Proxy) Resolve(urlPath string) (Route, error) {
	p.init()
	if strings.Contains(urlPath, "//") || path.Clean(urlPath) != urlPath {
		return Route{}, errRoute
	}
	for _, seg := range strings.Split(urlPath, "/") {
		if seg == "." || seg == ".." {
			return Route{}, errRoute
		}
	}
	switch {
	case vFile.MatchString(urlPath):
		return Route{Upstream: p.ModuleUpstream + urlPath, Cache: "mod" + urlPath}, nil
	case vList.MatchString(urlPath), latest.MatchString(urlPath):
		return Route{Upstream: p.ModuleUpstream + urlPath}, nil
	case sumPath.MatchString(urlPath):
		rest := strings.TrimPrefix(urlPath, "/sumdb/"+SumDBName)
		switch {
		case rest == "/supported":
			return Route{Local: true}, nil
		case rest == "/latest":
			return Route{Upstream: p.SumUpstream + rest}, nil
		default: // lookups and tiles do not change once served
			return Route{Upstream: p.SumUpstream + rest, Cache: "sumdb" + rest}, nil
		}
	}
	return Route{}, errRoute
}

// Handler returns the HTTP handler. log receives one line per request.
func (p *Proxy) Handler(log io.Writer) http.Handler {
	p.init()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, n, hit := p.serve(w, r)
		if log != nil {
			fmt.Fprintf(log, "%s %s %q %d %d cache=%v\n", time.Now().UTC().Format(time.RFC3339), r.Method, r.RequestURI, status, n, hit)
		}
	})
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) (int, int64, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return http.StatusMethodNotAllowed, 0, false
	}
	// Absolute-form requests ("GET http://host/ HTTP/1.1") are how a
	// forward proxy is asked for a host. This is not one.
	if !strings.HasPrefix(r.RequestURI, "/") || r.URL.Host != "" || r.URL.RawQuery != "" {
		http.Error(w, "not a module proxy request", http.StatusBadRequest)
		return http.StatusBadRequest, 0, false
	}
	rt, err := p.Resolve(r.URL.Path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return http.StatusNotFound, 0, false
	}
	if rt.Local {
		w.WriteHeader(http.StatusOK)
		return http.StatusOK, 0, false
	}
	if rt.Cache != "" {
		if f, err := os.Open(filepath.Join(p.Cache, filepath.FromSlash(rt.Cache))); err == nil {
			defer f.Close()
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodHead {
				return http.StatusOK, 0, true
			}
			n, _ := io.Copy(w, f)
			return http.StatusOK, n, true
		}
	}
	resp, err := p.client.Get(rt.Upstream)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return http.StatusBadGateway, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || rt.Cache == "" {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodHead {
			return resp.StatusCode, 0, false
		}
		n, _ := io.Copy(w, resp.Body)
		return resp.StatusCode, n, false
	}
	// A cacheable 200: write it to the cache first, then serve the file, so
	// a response is cached whole or not at all.
	dst := filepath.Join(p.Cache, filepath.FromSlash(rt.Cache))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		http.Error(w, "cache unavailable", http.StatusInternalServerError)
		return http.StatusInternalServerError, 0, false
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".part-*")
	if err != nil {
		http.Error(w, "cache unavailable", http.StatusInternalServerError)
		return http.StatusInternalServerError, 0, false
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		http.Error(w, "upstream read failed", http.StatusBadGateway)
		return http.StatusBadGateway, 0, false
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		http.Error(w, "cache unavailable", http.StatusInternalServerError)
		return http.StatusInternalServerError, 0, false
	}
	f, err := os.Open(dst)
	if err != nil {
		http.Error(w, "cache unavailable", http.StatusInternalServerError)
		return http.StatusInternalServerError, 0, false
	}
	defer f.Close()
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return http.StatusOK, 0, false
	}
	n, _ := io.Copy(w, f)
	return http.StatusOK, n, false
}
