package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/npmproxy"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// An npm class's attempts get an npm registry of their own, on the pattern of
// the module proxy: its own Unix socket in the attempt's record, relayed into
// its guest only, serving exactly the tarballs the repository's
// package-lock.json locks at the attempt's commit (DESIGN-03 §2).
const (
	npmSocketName = "npmproxy.sock"
	npmLockName   = "npm-lock.json" // the parsed lock, kept so a restarted manager can serve it again
	npmLockFile   = "package-lock.json"
	npmCacheID    = "npm-cache"
	npmCacheRel   = "downloads/npm"
)

// NPMLock is what the entry and the result keep of an attempt's npm lock.
type NPMLock struct {
	Path     string `json:"path"`     // the lock's path in the repository, the class's
	SHA256   string `json:"sha256"`   // of that file at the commit, as read from the mirror
	Packages int    `json:"packages"` // registry tarballs it locks
}

// npmLockPath is the class's lock path in the repository.
func (jc JobClass) npmLockPath() string {
	if jc.NPMLock == "" {
		return npmLockFile
	}
	return jc.NPMLock
}

// checkNPMLockPath refuses, when the manager opens, a lock path on a class
// that does not serve npm, and one that is not a clean relative path in the
// repository naming a package-lock.json.
func checkNPMLockPath(jc JobClass) error {
	p := jc.NPMLock
	if p == "" {
		return nil
	}
	if !jc.NPM {
		return fmt.Errorf("manager: class %q names an npm lock and does not serve npm", jc.Class.Name)
	}
	if path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") ||
		path.Base(p) != npmLockFile || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("manager: class %q: the npm lock %q must be a clean relative path in the repository ending in %s", jc.Class.Name, p, npmLockFile)
	}
	return nil
}

func (m *Manager) npmSocket(id string) string {
	return filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, npmSocketName)
}

// readNPMLock reads the class's lock (file, a path in the repository) from
// the mirror at the attempt's commit, on the host, and parses it. The job
// cannot widen a lock it never writes. A class that needs npm and a commit
// with no lock, or one the proxy refuses, is refused before capacity is
// claimed.
func (m *Manager) readNPMLock(src *Source, sha, file string) (npmproxy.Lock, *NPMLock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := m.cfg.Mirrors.Show(ctx, src.Mirror, sha, file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("manager: the class serves npm, and %s has no %s at %s", src.Mirror, file, sha)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("manager: reading %s: %w", file, err)
	}
	lock, err := npmproxy.ParseLock(bytes.NewReader(b), m.cfg.NPMUpstream)
	if err != nil {
		return nil, nil, fmt.Errorf("manager: %w", err)
	}
	h := sha256.Sum256(b)
	return lock, &NPMLock{Path: file, SHA256: hex.EncodeToString(h[:]), Packages: len(lock)}, nil
}

// listenNPM serves lock on attempt id's npm socket and keeps the lock in its
// record. m.mu must be held.
func (m *Manager) listenNPM(id string, lock npmproxy.Lock) error {
	if _, ok := m.npmProxies[id]; ok {
		return nil
	}
	v := m.cfg.Venue
	if err := v.EnsureCache(venue.KindDownload, npmCacheID, npmCacheRel); err != nil {
		return err
	}
	sock := m.npmSocket(id)
	if len(sock) > maxSocketPath {
		return fmt.Errorf("manager: npm socket path is %d bytes, over the %d a Unix socket allows: %s", len(sock), maxSocketPath, sock)
	}
	// The parsed lock, written once at the start; a reopen reads it back.
	if kept := filepath.Join(filepath.Dir(sock), npmLockName); !exists(kept) {
		b, err := json.Marshal(lock)
		if err != nil {
			return err
		}
		if err := os.WriteFile(kept, b, 0o400); err != nil {
			return err
		}
	}
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("manager: %s exists and is not a socket", sock)
		}
		if err := os.Remove(sock); err != nil {
			return err
		}
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("manager: npm socket: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	log, err := os.OpenFile(filepath.Join(filepath.Dir(sock), "npmproxy.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		ln.Close()
		return err
	}
	p := &npmproxy.Proxy{Lock: lock, Cache: filepath.Join(v.Root(), npmCacheRel), Upstream: m.cfg.NPMUpstream}
	srv := &http.Server{Handler: p.Handler(log)}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.event("npmproxy-error", id, 0, err.Error())
		}
	}()
	m.npmProxies[id] = &proxyListener{ln: ln, srv: srv, log: log}
	return nil
}

// closeNPM stops attempt id's npm registry and removes its socket. m.mu must
// be held.
func (m *Manager) closeNPM(id string) {
	p, ok := m.npmProxies[id]
	if !ok {
		return
	}
	p.srv.Close()
	p.log.Close()
	os.Remove(m.npmSocket(id))
	delete(m.npmProxies, id)
}

// reopenNPM serves the npm registry again for a live attempt adopted after a
// restart, from the lock kept in its record. m.mu must be held.
func (m *Manager) reopenNPM(id string) error {
	b, err := os.ReadFile(filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, npmLockName))
	if err != nil {
		return err
	}
	var lock npmproxy.Lock
	if err := json.Unmarshal(b, &lock); err != nil {
		return err
	}
	return m.listenNPM(id, lock)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
