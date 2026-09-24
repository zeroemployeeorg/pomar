package manager

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

// The module proxy is served to each attempt on its own Unix socket inside
// the attempt's record. The helper relays that socket, and only that one,
// into its guest; so a connection on it comes from the attempt the manager
// launched, and from nothing else.
const proxySocketName = "goproxy.sock"

// maxSocketPath is the longest path a Unix socket can bind on macOS
// (sockaddr_un.sun_path is 104 bytes, including the terminating NUL).
const maxSocketPath = 103

type proxyListener struct {
	ln  net.Listener
	srv *http.Server
	log *os.File
}

func (m *Manager) proxyEnabled() bool { return m.cfg.GoProxy != nil && m.cfg.ShimBin != "" }

func (m *Manager) proxySocket(id string) string {
	return filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, proxySocketName)
}

// listenProxy serves the module proxy on attempt id's socket, replacing a
// stale socket file left by a manager that has gone. m.mu must be held.
func (m *Manager) listenProxy(id string) error {
	if _, ok := m.proxies[id]; ok {
		return nil
	}
	sock := m.proxySocket(id)
	if len(sock) > maxSocketPath {
		return fmt.Errorf("manager: proxy socket path is %d bytes, over the %d a Unix socket allows: %s", len(sock), maxSocketPath, sock)
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
		return fmt.Errorf("manager: proxy socket: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	log, err := os.OpenFile(filepath.Join(filepath.Dir(sock), "goproxy.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: m.cfg.GoProxy.Handler(log)}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.event("goproxy-error", id, 0, err.Error())
		}
	}()
	m.proxies[id] = &proxyListener{ln: ln, srv: srv, log: log}
	return nil
}

// closeProxy stops attempt id's proxy and removes its socket. m.mu must be
// held.
func (m *Manager) closeProxy(id string) {
	p, ok := m.proxies[id]
	if !ok {
		return
	}
	p.srv.Close()
	p.log.Close()
	os.Remove(m.proxySocket(id))
	delete(m.proxies, id)
}

// reopenProxies serves the proxy again for live attempts adopted after a
// restart: their helpers relay to the same socket path.
func (m *Manager) reopenProxies() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.t.entries {
		if e.GoProxy && !e.Terminal() {
			if err := m.listenProxy(id); err != nil {
				m.event("goproxy-error", id, e.PID, err.Error())
			} else {
				m.event("goproxy-reopened", id, e.PID, "")
			}
		}
	}
}
