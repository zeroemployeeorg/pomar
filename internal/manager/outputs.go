package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Copy-out (POMAR-SOW-05 §7, PR 4): a start names the files it will leave in
// its guest at /pomar/outputs/NAME. When the command exits, the guest stages
// each named file that is a regular file within the cap, and the helper copies
// the staged files into the attempt's record. The manager then checks each
// one again on the host and records its name, size and sha256 in the entry
// and the result document. Nothing but the named files comes out.

// OutputRecord is what the entry and the result keep of a named output.
type OutputRecord struct {
	Name   string `json:"name"`
	Status string `json:"status"` // OutputOK, or why there is no file
	Bytes  int64  `json:"bytes,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// An output's status.
const (
	OutputOK         = "ok"
	OutputMissing    = "missing"     // not copied out: absent, not a regular file or over the cap in the guest, or the attempt did not exit
	OutputNotRegular = "not-regular" // what came out was not a regular file; removed
	OutputOverCap    = "over-cap"    // what came out was over the cap; removed
)

// Limits on outputs: results and reports, not artefacts to ship.
const (
	maxOutputs      = 16
	MaxOutputsBytes = 64 << 20 // across all of an attempt's outputs
	outputsDir      = "outputs"
)

// checkOutputs refuses bad names, duplicates and too many outputs. The names
// follow the inputs' rule, so none can be a path or begin with a dot.
func checkOutputs(names []string) error {
	if len(names) > maxOutputs {
		return fmt.Errorf("manager: %d outputs, at most %d", len(names), maxOutputs)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !inputName.MatchString(n) || seen[n] {
			return fmt.Errorf("manager: invalid or repeated output name %q", n)
		}
		seen[n] = true
	}
	return nil
}

// outputArgs are the helper's flags for an attempt's outputs.
func outputArgs(rec string, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	return []string{"--outputs", strings.Join(names, ","), "--outputs-dir", filepath.Join(rec, outputsDir),
		"--outputs-max", strconv.Itoa(MaxOutputsBytes)}
}

// collectOutputs checks what the helper copied into the record against the
// declared names and the cap, whatever the guest did: anything that is not a
// regular file, or that would take the total over the cap, is removed. What
// is kept is made read-only.
func collectOutputs(rec string, names []string) []OutputRecord {
	dir := filepath.Join(rec, outputsDir)
	var out []OutputRecord
	var total int64
	for _, n := range names {
		p := filepath.Join(dir, n)
		fi, err := os.Lstat(p)
		switch {
		case err != nil:
			out = append(out, OutputRecord{Name: n, Status: OutputMissing})
			continue
		case !fi.Mode().IsRegular():
			os.RemoveAll(p)
			out = append(out, OutputRecord{Name: n, Status: OutputNotRegular})
			continue
		case total+fi.Size() > MaxOutputsBytes:
			os.Remove(p)
			out = append(out, OutputRecord{Name: n, Status: OutputOverCap})
			continue
		}
		sum, size, err := hashFile(p)
		if err != nil || size != fi.Size() {
			os.Remove(p)
			out = append(out, OutputRecord{Name: n, Status: OutputMissing})
			continue
		}
		total += size
		os.Chmod(p, 0o400)
		out = append(out, OutputRecord{Name: n, Status: OutputOK, Bytes: size, SHA256: sum})
	}
	return out
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxOutputsBytes+1))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// OutputPath is where a recorded output of attempt id is, if the entry
// records it as copied out.
func (m *Manager) OutputPath(id, name string) (OutputRecord, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.t.entries[id]
	if e == nil {
		return OutputRecord{}, "", fmt.Errorf("manager: no attempt %s", id)
	}
	for _, o := range e.Outputs {
		if o.Name == name && o.Status == OutputOK {
			return o, filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, outputsDir, name), nil
		}
	}
	return OutputRecord{}, "", fmt.Errorf("manager: attempt %s has no output %q", id, name)
}

// outputRoutes serves a recorded output's bytes, as copied out and checked,
// with its sha256 in a header for the client to check.
func (m *Manager) outputRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/attempts/{id}/outputs/{name}", func(w http.ResponseWriter, r *http.Request) {
		id, name := r.PathValue("id"), r.PathValue("name")
		if !validID.MatchString(id) || !inputName.MatchString(name) {
			reply(w, http.StatusBadRequest, map[string]string{"error": "invalid attempt id or output name"})
			return
		}
		o, p, err := m.OutputPath(id, name)
		if err != nil {
			reply(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		f, err := os.Open(p)
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(o.Bytes, 10))
		w.Header().Set(OutputSHA256Header, o.SHA256)
		w.WriteHeader(http.StatusOK)
		io.Copy(w, io.LimitReader(f, o.Bytes))
	})
}

// OutputSHA256Header carries a served output's recorded sha256.
const OutputSHA256Header = "X-Pomar-Sha256"

// Output fetches a recorded output into w and checks it against the sha256
// the manager recorded for it, which it returns.
func (c *Client) Output(id, name string, w io.Writer) (string, error) {
	resp, err := c.http.Get("http://manager/v1/attempts/" + id + "/outputs/" + name)
	if err != nil {
		return "", fmt.Errorf("manager not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return "", fmt.Errorf("manager: %s: %s", resp.Status, e["error"])
	}
	want := resp.Header.Get(OutputSHA256Header)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("manager: output %s: sha256 %s, not the recorded %s", name, got, want)
	}
	return want, nil
}
