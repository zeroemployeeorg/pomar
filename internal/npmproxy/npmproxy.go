// Package npmproxy is an npm registry for one attempt, on the pattern of the
// Python package index proxy: it serves the tarballs a package-lock.json names,
// by the paths it names them at, and nothing else. It is not a forward proxy:
// it never takes a host from a request, it refuses every method but GET and
// HEAD, and it fetches only from the npm registry.
//
// It is integrity-locked. An attempt carries its lock, read on the host from
// the repository at the attempt's exact commit, never from the guest; a
// tarball is served only if the lock names its path, and its bytes are
// checked against the lock's sha512 before they are cached or served.
// Tarballs are kept in a cache directory by that sha512. `npm ci` with a
// complete lock fetches each tarball by its resolved URL, which npm rewrites
// to the configured registry, so no package metadata (packument) is needed,
// and none is served (DESIGN-03 §2; the elders' ruling of 2026-09-27 §3.3).
package npmproxy

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream is the only registry.
const Upstream = "https://registry.npmjs.org"

// maxLock bounds a package-lock.json.
const maxLock = 64 << 20

// Lock maps each tarball path the lock resolves to (as the registry serves
// it, for example /left-pad/-/left-pad-1.3.0.tgz) to its sha512, hex.
type Lock map[string]string

// Paths returns the lock's tarball paths, sorted.
func (l Lock) Paths() []string {
	out := make([]string, 0, len(l))
	for p := range l {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// tarball is a registry tarball path: /name/-/file.tgz or /@scope/name/-/file.tgz
// (some old package names have capitals, such as JSONStream).
var tarball = regexp.MustCompile(`^/(?:@[A-Za-z0-9][A-Za-z0-9._~-]*/)?[A-Za-z0-9][A-Za-z0-9._~-]*/-/[A-Za-z0-9][A-Za-z0-9._~+-]*\.tgz$`)

// ParseLock reads a package-lock.json. It refuses the whole lock, naming the
// entry, unless it is lockfileVersion 2 or later and every package entry is a
// registry tarball on the npm registry, over https, with a sha512 integrity.
// A git, file or workspace (link) source, another host, and an sha1-only
// integrity are each refused. A package bundled inside another's tarball
// (inBundle) carries no source of its own and is skipped.
func ParseLock(r io.Reader, upstream string) (Lock, error) {
	if upstream == "" {
		upstream = Upstream
	}
	up, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	var doc struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Resolved  string `json:"resolved"`
			Integrity string `json:"integrity"`
			Link      bool   `json:"link"`
			InBundle  bool   `json:"inBundle"`
			Bundled   bool   `json:"bundled"`
		} `json:"packages"`
	}
	if err := json.NewDecoder(io.LimitReader(r, maxLock)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("npmproxy: lock: %w", err)
	}
	if doc.LockfileVersion < 2 {
		return nil, fmt.Errorf("npmproxy: lock: lockfileVersion %d; 2 or later is needed, for its packages section", doc.LockfileVersion)
	}
	lock := Lock{}
	keys := make([]string, 0, len(doc.Packages))
	for k := range doc.Packages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := doc.Packages[k]
		switch {
		case k == "":
			continue // the project itself
		case p.InBundle || p.Bundled:
			continue // inside another package's tarball
		case p.Link:
			return nil, fmt.Errorf("npmproxy: lock: %s is a link to a local folder, not a registry package", k)
		case p.Resolved == "":
			return nil, fmt.Errorf("npmproxy: lock: %s has no resolved URL", k)
		}
		u, err := url.Parse(p.Resolved)
		if err != nil || u.Scheme != up.Scheme || u.Host != up.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !tarball.MatchString(u.Path) {
			return nil, fmt.Errorf("npmproxy: lock: %s resolves to %q, not a tarball on %s", k, p.Resolved, upstream)
		}
		sum, err := sha512Of(p.Integrity)
		if err != nil {
			return nil, fmt.Errorf("npmproxy: lock: %s: %w", k, err)
		}
		if prev, ok := lock[u.Path]; ok && prev != sum {
			return nil, fmt.Errorf("npmproxy: lock: %s names %s with a second sha512", k, u.Path)
		}
		lock[u.Path] = sum
	}
	if len(lock) == 0 {
		return nil, errors.New("npmproxy: lock: no registry packages")
	}
	return lock, nil
}

// sha512Of returns the sha512 in an integrity string (Subresource Integrity:
// space-separated "algorithm-base64" entries), hex. One without sha512, or
// with two different sha512s, is refused.
func sha512Of(integrity string) (string, error) {
	var sum string
	for _, f := range strings.Fields(integrity) {
		alg, b64, ok := strings.Cut(f, "-")
		if !ok || alg != "sha512" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(b) != sha512.Size {
			return "", fmt.Errorf("integrity %q has a malformed sha512", integrity)
		}
		h := hex.EncodeToString(b)
		if sum != "" && sum != h {
			return "", fmt.Errorf("integrity %q has two sha512s", integrity)
		}
		sum = h
	}
	if sum == "" {
		return "", fmt.Errorf("integrity %q has no sha512 (an sha1 alone is not enough)", integrity)
	}
	return sum, nil
}

// Proxy serves one lock from its cache or the registry.
type Proxy struct {
	// Lock is the attempt's lock; nothing outside it is served.
	Lock Lock
	// Cache is the directory for tarballs, by sha512 (hex); it must exist.
	Cache string
	// Client fetches from the upstream; nil means a client with a timeout.
	// Redirects are never followed.
	Client *http.Client
	// Upstream can be replaced in tests; empty means the npm registry.
	Upstream string

	once   sync.Once
	client *http.Client
}

func (p *Proxy) init() {
	p.once.Do(func() {
		c := &http.Client{Timeout: 5 * time.Minute}
		if p.Client != nil {
			*c = *p.Client
		}
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		p.client = c
		if p.Upstream == "" {
			p.Upstream = Upstream
		}
	})
}

var errRoute = errors.New("npmproxy: not a locked tarball")

// Resolve maps a request path to the sha512 its lock names, or refuses it:
// only a path the lock names exactly, with no dot segment, is served.
func (p *Proxy) Resolve(urlPath string) (string, error) {
	p.init()
	if strings.Contains(urlPath, "//") || path.Clean(urlPath) != urlPath {
		return "", errRoute
	}
	for _, seg := range strings.Split(urlPath, "/") {
		if seg == "." || seg == ".." {
			return "", errRoute
		}
	}
	sum, ok := p.Lock[urlPath]
	if !ok || !tarball.MatchString(urlPath) {
		return "", errRoute
	}
	return sum, nil
}

// Handler retains the completion line and adds correlated lifecycle events.
// Unknown paths, queries, absolute URLs and headers are never logged.
func (p *Proxy) Handler(log io.Writer) http.Handler {
	p.init()
	var sequence atomic.Uint64
	var logMu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, started := sequence.Add(1), time.Now()
		method := r.Method
		if method != http.MethodGet && method != http.MethodHead {
			method = "OTHER"
		}
		route := "[redacted]"
		if strings.HasPrefix(r.RequestURI, "/") && r.URL.Host == "" && r.URL.RawQuery == "" {
			if _, err := p.Resolve(r.URL.Path); err == nil || r.URL.Path == "/npm" || r.URL.Path == "/-/ping" {
				route = r.URL.Path
			}
		}
		writeEvent := func(event, cache string, status int, bytes int64) {
			if log == nil {
				return
			}
			entry := struct {
				Schema    string `json:"schema"`
				Time      string `json:"time"`
				Request   uint64 `json:"request"`
				Event     string `json:"event"`
				Method    string `json:"method"`
				Path      string `json:"path"`
				Cache     string `json:"cache,omitempty"`
				ElapsedMS int64  `json:"elapsed_ms"`
				Status    int    `json:"status,omitempty"`
				Bytes     int64  `json:"bytes,omitempty"`
			}{"pomar.npm-request/v1", time.Now().UTC().Format(time.RFC3339Nano), id, event, method, route, cache, time.Since(started).Milliseconds(), status, bytes}
			logMu.Lock()
			defer logMu.Unlock()
			_ = json.NewEncoder(log).Encode(entry)
		}
		trace := func(event, cache string) { writeEvent(event, cache, 0, 0) }
		trace("received", "unknown")
		status, n, hit := p.serve(w, r, trace)
		writeEvent("completed", "", status, n)
		if log != nil {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(log, "%s %s %q %d %d cache=%v\n", time.Now().UTC().Format(time.RFC3339), method, route, status, n, hit)
		}
	})
}

func fail(w http.ResponseWriter, code int, msg string) (int, int64, bool) {
	http.Error(w, msg, code)
	return code, 0, false
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, trace func(string, string)) (int, int64, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
	// Absolute-form requests are how a forward proxy is asked for a host.
	if !strings.HasPrefix(r.RequestURI, "/") || r.URL.Host != "" || r.URL.RawQuery != "" {
		return fail(w, http.StatusBadRequest, "not a registry request")
	}
	sum, err := p.Resolve(r.URL.Path)
	if err != nil {
		return fail(w, http.StatusNotFound, "not found")
	}
	dst := filepath.Join(p.Cache, sum)
	if f, err := os.Open(dst); err == nil {
		trace("cache", "hit")
		defer f.Close()
		return sendFile(w, r, f, true)
	}
	trace("cache", "miss")
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.Upstream+r.URL.Path, nil)
	if err != nil {
		return fail(w, http.StatusBadGateway, "upstream unavailable")
	}
	upstream = upstream.WithContext(httptrace.WithClientTrace(upstream.Context(), &httptrace.ClientTrace{
		ConnectStart:         func(_, _ string) { trace("upstream_connect_started", "") },
		GotConn:              func(httptrace.GotConnInfo) { trace("upstream_connected", "") },
		GotFirstResponseByte: func() { trace("upstream_first_byte", "") },
	}))
	trace("upstream_started", "")
	resp, err := p.client.Do(upstream)
	if err != nil {
		return fail(w, http.StatusBadGateway, "upstream unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(w, http.StatusBadGateway, "upstream status") // a redirect is never followed
	}
	tmp, err := os.CreateTemp(p.Cache, ".part-*")
	if err != nil {
		return fail(w, http.StatusInternalServerError, "cache unavailable")
	}
	h := sha512.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return fail(w, http.StatusBadGateway, "upstream read failed")
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		os.Remove(tmp.Name())
		return fail(w, http.StatusBadGateway, "upstream tarball does not match the lock")
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return fail(w, http.StatusInternalServerError, "cache unavailable")
	}
	f, err := os.Open(dst)
	if err != nil {
		return fail(w, http.StatusInternalServerError, "cache unavailable")
	}
	defer f.Close()
	return sendFile(w, r, f, false)
}

func sendFile(w http.ResponseWriter, r *http.Request, f *os.File, hit bool) (int, int64, bool) {
	if fi, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return http.StatusOK, 0, hit
	}
	n, _ := io.Copy(w, f)
	return http.StatusOK, n, hit
}
