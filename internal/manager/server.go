package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
)

// SocketPath is where the manager listens, inside its ledgered directory.
func SocketPath(root string) string { return filepath.Join(root, managerDir, "manager.sock") }

// StartRequest is the body of POST /v1/attempts.
type StartRequest struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
	Source  *Source  `json:"source,omitempty"`
	Inputs  []Input  `json:"inputs,omitempty"`
}

// Serve listens on the Unix socket until ctx ends: the owner's socket with every
// route, and, when configured, the stream's control socket with the stream's
// routes only. A stale socket left by a dead manager is replaced; the lock taken
// in Open guarantees it is stale.
func (m *Manager) Serve(ctx context.Context) error {
	ln, err := listen(SocketPath(m.cfg.Venue.Root()), 0o600)
	if err != nil {
		return err
	}
	servers := []*http.Server{{Handler: m.mux(true)}}
	listeners := []net.Listener{ln}
	if m.cfg.CtlSocket != "" {
		if err := checkCtlDir(filepath.Dir(m.cfg.CtlSocket)); err != nil {
			ln.Close()
			return err
		}
		cl, err := listen(m.cfg.CtlSocket, 0o660)
		if err != nil {
			ln.Close()
			return err
		}
		servers = append(servers, &http.Server{Handler: m.mux(false)})
		listeners = append(listeners, cl)
	}
	go func() {
		<-ctx.Done()
		for _, s := range servers {
			s.Close()
		}
	}()
	errs := make(chan error, len(servers))
	for i := range servers {
		go func(s *http.Server, l net.Listener) { errs <- s.Serve(l) }(servers[i], listeners[i])
	}
	// The first server to stop stops the others; ctx ending stops them all.
	err = <-errs
	for _, s := range servers {
		s.Close()
	}
	for range servers[1:] {
		<-errs
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// listen replaces a stale socket at path, listens, and sets its mode.
func listen(path string, mode fs.FileMode) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("manager: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// checkCtlDir refuses a control-socket directory that others can enter: the
// socket's reach is its directory's owner and group, and nobody else.
func checkCtlDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("manager: control socket directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("manager: control socket directory %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("manager: control socket directory %s is open to others (mode %o); want 0750", dir, fi.Mode().Perm())
	}
	return nil
}

// mux serves the API. The owner's socket gets every route (full); the stream's
// control socket gets start, stop and reads, and never a route that removes a
// record, evicts a cache or changes configuration (DESIGN-02 approach A).
func (m *Manager) mux(full bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/attempts", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, m.List())
	})
	mux.HandleFunc("POST /v1/attempts", func(w http.ResponseWriter, r *http.Request) {
		var req StartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		e, err := m.Start(req.ID, req.Command, req.Source, req.Inputs...)
		if errors.Is(err, ErrExists) {
			reply(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		var ref *capacity.Refusal
		if errors.As(err, &ref) {
			reply(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error(), "reason": ref.Reason})
			return
		}
		if err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusCreated, e)
	})
	mux.HandleFunc("POST /v1/attempts/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		e, err := m.Stop(r.PathValue("id"))
		if err != nil {
			reply(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusOK, e)
	})
	if full {
		mux.HandleFunc("DELETE /v1/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
			if err := m.Remove(r.PathValue("id")); err != nil {
				reply(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("GET /v1/vm-orphans", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, m.VMOrphans())
	})
	mux.HandleFunc("GET /v1/reconcile", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, m.Report())
	})
	caches := func(apply bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rep, err := m.Caches(apply)
			if err != nil {
				reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			reply(w, http.StatusOK, rep)
		}
	}
	mux.HandleFunc("GET /v1/caches", caches(false))
	if full {
		mux.HandleFunc("POST /v1/caches/evict", caches(true))
	}
	mux.HandleFunc("GET /v1/capacity", func(w http.ResponseWriter, r *http.Request) {
		c, err := m.Capacity()
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusOK, c)
	})
	return mux
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// Client talks to a manager over its socket.
type Client struct{ http *http.Client }

// NewClient returns a client for the manager of the data root.
func NewClient(root string) *Client { return NewSocketClient(SocketPath(root)) }

// NewSocketClient talks to a manager over the socket at sock: the stream uses
// it for the permanent manager's control socket.
func NewSocketClient(sock string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

// Do sends a request and decodes a JSON reply into out (if non-nil).
func (c *Client) Do(method, path string, body, out any) error {
	var req *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		req, err = http.NewRequest(method, "http://manager"+path, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequest(method, "http://manager"+path, nil)
	}
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("manager not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("manager: %s: %s", resp.Status, e["error"])
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
