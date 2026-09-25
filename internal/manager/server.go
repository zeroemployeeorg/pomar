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
}

// Serve listens on the Unix socket until ctx ends. A stale socket left by a
// dead manager is replaced; the lock taken in Open guarantees it is stale.
func (m *Manager) Serve(ctx context.Context) error {
	sock := SocketPath(m.cfg.Venue.Root())
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return fmt.Errorf("manager: %s exists and is not a socket", sock)
		}
		if err := os.Remove(sock); err != nil {
			return err
		}
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: m.mux()}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (m *Manager) mux() *http.ServeMux {
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
		e, err := m.Start(req.ID, req.Command, req.Source)
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
	mux.HandleFunc("DELETE /v1/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Remove(r.PathValue("id")); err != nil {
			reply(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
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
	mux.HandleFunc("POST /v1/caches/evict", caches(true))
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
func NewClient(root string) *Client {
	sock := SocketPath(root)
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
