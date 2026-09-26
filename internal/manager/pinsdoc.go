package manager

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/zeroemployeeorg/pomar/internal/result"
)

// The pins document (POMAR-SOW-06 §4.1; elders' ruling r26 §4.1): what an
// attempt runs with, written once into its record at admission and copied
// into its guest at /pomar/pins.json (root-owned, mode 0444) before the
// command is released. `pomar attempt pins` prints the same bytes.
const (
	PinsSchema  = "pomar.pins/v1"
	pinsName    = "pins.json"
	PinsInGuest = "/pomar/pins.json"
)

// pinnedSource is what the entry and the pins document record of a source.
func pinnedSource(src *Source, sha string) *PinnedSource {
	p := &PinnedSource{Mirror: src.Mirror, Ref: src.Ref, SHA: sha, Git: src.Git, ReadOnly: src.ReadOnly}
	if src.Git {
		p.Base = src.base()
	}
	return p
}

// pinsDoc is the pins document's canonical JSON.
func pinsDoc(id string, class any, pins *Pins, src *PinnedSource) ([]byte, error) {
	return result.Canonical(map[string]any{
		"schema":  PinsSchema,
		"attempt": id,
		"class":   class,
		"pins":    pins,
		"source":  src,
	})
}

// writePins writes the pins document into the record, once, read-only.
func writePins(rec string, doc []byte) (string, error) {
	p := filepath.Join(rec, pinsName)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(doc); err != nil {
		f.Close()
		return "", err
	}
	return p, f.Close()
}

func (m *Manager) pinsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/attempts/{id}/pins", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID.MatchString(id) {
			reply(w, http.StatusBadRequest, map[string]string{"error": "invalid attempt id"})
			return
		}
		b, err := os.ReadFile(filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, pinsName))
		if errors.Is(err, fs.ErrNotExist) {
			reply(w, http.StatusNotFound, map[string]string{"error": "no pins document for " + id + " (an attempt with no source has none)"})
			return
		}
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(b)
	})
}

// Pins fetches an attempt's pins document, as the exact bytes written.
func (c *Client) Pins(id string) ([]byte, error) {
	return c.raw("/v1/attempts/" + id + "/pins")
}
