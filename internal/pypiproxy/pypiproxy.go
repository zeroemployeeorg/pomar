// Package pypiproxy is a Python package index for one attempt, on the Go
// module proxy's pattern: it serves the simple repository API's project pages
// and the files they list, and nothing else. It is not a forward proxy: it
// never takes a host from a request, it refuses CONNECT and every method but
// GET and HEAD, and it fetches only from the Python Package Index.
//
// It is hash-locked. An attempt carries a lock (the sha256 of every file it
// may install), and a file is served only if its sha256 is in that lock: a
// project page lists only the locked files, a file is fetched only by a
// locked sha256, and its bytes are checked against that sha256 before they
// are cached or served. Files are kept in a cache directory by sha256.
package pypiproxy

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The only upstreams: the index's simple API and the host its files are on.
const (
	IndexUpstream = "https://pypi.org/simple"
	FilesUpstream = "https://files.pythonhosted.org"
)

// SimpleJSON is the simple API's JSON form (PEP 691), the only form served.
const SimpleJSON = "application/vnd.pypi.simple.v1+json"

// maxPage caps a project page read from the index.
const maxPage = 32 << 20

// Lock is the set of sha256 digests (lower-case hex) an attempt may install.
type Lock map[string]bool

var (
	hashOpt = regexp.MustCompile(`--hash[= ]([a-z0-9]+):([0-9A-Za-z]+)`)
	hexSHA  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseLock reads a hash-locked requirements file (as `pip install
// --require-hashes` takes, or `uv export` writes) and returns its sha256
// digests. It refuses a hash in any other algorithm and a requirement with
// no hash, so a lock cannot name a file it does not pin.
func ParseLock(r io.Reader) (Lock, error) {
	lock := Lock{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var req strings.Builder
	flush := func() error {
		line := strings.TrimSpace(req.String())
		req.Reset()
		if line == "" || strings.HasPrefix(line, "-") {
			return nil // an option line, such as --index-url, pins nothing
		}
		m := hashOpt.FindAllStringSubmatch(line, -1)
		if len(m) == 0 {
			return fmt.Errorf("pypiproxy: lock: requirement without a hash: %q", line)
		}
		for _, h := range m {
			if h[1] != "sha256" || !hexSHA.MatchString(h[2]) {
				return fmt.Errorf("pypiproxy: lock: not a sha256 digest: %s:%s", h[1], h[2])
			}
			lock[h[2]] = true
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			line = line[:i]
		}
		if cont := strings.HasSuffix(strings.TrimRight(line, " \t"), `\`); cont {
			req.WriteString(strings.TrimSuffix(strings.TrimRight(line, " \t"), `\`) + " ")
			continue
		}
		req.WriteString(line)
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(lock) == 0 {
		return nil, errors.New("pypiproxy: lock: no hashes")
	}
	return lock, nil
}

// Proxy serves one lock from its cache or the index.
type Proxy struct {
	// Lock is the attempt's lock; nothing outside it is listed or served.
	Lock Lock
	// Cache is the directory for files, by sha256; it must exist.
	Cache string
	// Client fetches from the upstreams; nil means a client with a timeout.
	// Redirects are never followed.
	Client *http.Client
	// Upstreams can be replaced in tests; empty means the real ones.
	IndexUpstream, FilesUpstream string

	once   sync.Once
	client *http.Client
	mu     sync.Mutex
	urls   map[string]string // locked sha256 -> file URL, from project pages served
}

func (p *Proxy) init() {
	p.once.Do(func() {
		c := &http.Client{Timeout: 5 * time.Minute}
		if p.Client != nil {
			*c = *p.Client
		}
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		p.client = c
		if p.IndexUpstream == "" {
			p.IndexUpstream = IndexUpstream
		}
		if p.FilesUpstream == "" {
			p.FilesUpstream = FilesUpstream
		}
		p.urls = map[string]string{}
	})
}

// Route is what an allowed request maps to.
type Route struct {
	Project string // a project page: the normalized name
	SHA256  string // a file: its locked digest
	File    string // a file: its name
}

var (
	// A normalized project name (PEP 503).
	project = regexp.MustCompile(`^/simple/([a-z0-9]+(?:-[a-z0-9]+)*)/$`)
	// A file, by the digest the lock pins and the name its page gave.
	file     = regexp.MustCompile(`^/files/([0-9a-f]{64})/([A-Za-z0-9][A-Za-z0-9._+-]*\.(?:whl|tar\.gz|zip))$`)
	errRoute = errors.New("pypiproxy: not an index path")
)

// Resolve maps a request path to a route, or refuses it. A path with a dot
// segment is refused before matching, and a file whose digest is not in the
// lock is refused here, before anything is fetched.
func (p *Proxy) Resolve(urlPath string) (Route, error) {
	p.init()
	clean := path.Clean(urlPath)
	if strings.HasSuffix(urlPath, "/") && clean != "/" {
		clean += "/" // a project page's path ends in a slash
	}
	if strings.Contains(urlPath, "//") || clean != urlPath {
		return Route{}, errRoute
	}
	for _, seg := range strings.Split(urlPath, "/") {
		if seg == "." || seg == ".." {
			return Route{}, errRoute
		}
	}
	if m := project.FindStringSubmatch(urlPath); m != nil {
		return Route{Project: m[1]}, nil
	}
	if m := file.FindStringSubmatch(urlPath); m != nil {
		if !p.Lock[m[1]] {
			return Route{}, fmt.Errorf("pypiproxy: %s is not in the lock", m[1])
		}
		return Route{SHA256: m[1], File: m[2]}, nil
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

func fail(w http.ResponseWriter, code int, msg string) (int, int64, bool) {
	http.Error(w, msg, code)
	return code, 0, false
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) (int, int64, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
	// Absolute-form requests ("GET http://host/ HTTP/1.1") are how a
	// forward proxy is asked for a host. This is not one.
	if !strings.HasPrefix(r.RequestURI, "/") || r.URL.Host != "" || r.URL.RawQuery != "" {
		return fail(w, http.StatusBadRequest, "not an index request")
	}
	rt, err := p.Resolve(r.URL.Path)
	if err != nil {
		return fail(w, http.StatusNotFound, "not found")
	}
	if rt.Project != "" {
		return p.servePage(w, r, rt.Project)
	}
	return p.serveFile(w, r, rt)
}

// page is the part of a PEP 691 project page this proxy reads and writes.
type page struct {
	Meta  map[string]any `json:"meta"`
	Name  string         `json:"name"`
	Files []pageFile     `json:"files"`
}

type pageFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python,omitempty"`
	Yanked         any               `json:"yanked,omitempty"`
}

// servePage fetches a project page and serves it with only the locked files,
// each pointing at this proxy. The page is not cached: an index page changes.
func (p *Proxy) servePage(w http.ResponseWriter, r *http.Request, name string) (int, int64, bool) {
	req, err := http.NewRequest(http.MethodGet, p.IndexUpstream+"/"+name+"/", nil)
	if err != nil {
		return fail(w, http.StatusInternalServerError, "bad upstream")
	}
	req.Header.Set("Accept", SimpleJSON)
	resp, err := p.client.Do(req)
	if err != nil {
		return fail(w, http.StatusBadGateway, "upstream unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return fail(w, http.StatusNotFound, "no such project")
		}
		return fail(w, http.StatusBadGateway, "upstream status") // a redirect is never followed
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, SimpleJSON) {
		return fail(w, http.StatusBadGateway, "upstream sent another form")
	}
	var in page
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPage)).Decode(&in); err != nil {
		return fail(w, http.StatusBadGateway, "upstream page unreadable")
	}
	out := page{Meta: map[string]any{"api-version": "1.0"}, Name: in.Name, Files: []pageFile{}}
	p.mu.Lock()
	for _, f := range in.Files {
		sum := strings.ToLower(f.Hashes["sha256"])
		if !p.Lock[sum] || !p.upstreamFile(f.URL, f.Filename) {
			continue
		}
		p.urls[sum] = f.URL
		out.Files = append(out.Files, pageFile{
			Filename: f.Filename, URL: "/files/" + sum + "/" + f.Filename,
			Hashes: map[string]string{"sha256": sum}, RequiresPython: f.RequiresPython, Yanked: f.Yanked,
		})
	}
	p.mu.Unlock()
	b, err := json.Marshal(out)
	if err != nil {
		return fail(w, http.StatusInternalServerError, "page unwritable")
	}
	w.Header().Set("Content-Type", SimpleJSON)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return http.StatusOK, 0, false
	}
	n, _ := w.Write(b)
	return http.StatusOK, int64(n), false
}

// upstreamFile reports whether u is a file on the files host, over the
// upstream's scheme, whose last segment is name and a name this proxy serves.
func (p *Proxy) upstreamFile(u, name string) bool {
	pu, err := url.Parse(u)
	if err != nil || pu.RawQuery != "" || pu.User != nil {
		return false
	}
	fu, err := url.Parse(p.FilesUpstream)
	if err != nil || pu.Scheme != fu.Scheme || pu.Host != fu.Host || path.Base(pu.Path) != name {
		return false
	}
	return file.MatchString("/files/" + strings.Repeat("0", 64) + "/" + name)
}

// serveFile serves a locked file from the cache, or fetches it from the URL
// its project page gave, checks its sha256, and caches it whole.
func (p *Proxy) serveFile(w http.ResponseWriter, r *http.Request, rt Route) (int, int64, bool) {
	dst := filepath.Join(p.Cache, rt.SHA256)
	if f, err := os.Open(dst); err == nil {
		defer f.Close()
		return sendFile(w, r, f, true)
	}
	p.mu.Lock()
	u := p.urls[rt.SHA256]
	p.mu.Unlock()
	if u == "" || path.Base(u) != rt.File {
		return fail(w, http.StatusNotFound, "not listed") // its project page was not served
	}
	resp, err := p.client.Get(u)
	if err != nil {
		return fail(w, http.StatusBadGateway, "upstream unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(w, http.StatusBadGateway, "upstream status")
	}
	tmp, err := os.CreateTemp(p.Cache, ".part-*")
	if err != nil {
		return fail(w, http.StatusInternalServerError, "cache unavailable")
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return fail(w, http.StatusBadGateway, "upstream read failed")
	}
	if hex.EncodeToString(h.Sum(nil)) != rt.SHA256 {
		os.Remove(tmp.Name())
		return fail(w, http.StatusBadGateway, "upstream file does not match the lock")
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
