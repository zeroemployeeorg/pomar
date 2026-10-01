package manager

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zeroemployeeorg/pomar/internal/result"
)

// The attempt's output log (the elders' ruling r30 §4.2, and their note of
// 2026-09-27): the guest's stdout and stderr, which the helper writes to
// output.log in the record. When the attempt ends, the manager records the
// log's size and the sha256 of what it will serve: the whole log, or its
// first bytes up to its class's cap, marked truncated. The record goes in the entry and the
// signed result, and GET /v1/attempts/{id}/log serves exactly those bytes.

// DefaultLogCapBytes caps what is recorded and served of an attempt's output
// log when its class sets no cap of its own.
const DefaultLogCapBytes = 16 << 20

// maxLogRead bounds what a client reads of a served log, whatever it is told.
const maxLogRead = 1 << 30

const logName = "output.log"

// LogRecord is what the entry and the result keep of an attempt's log.
type LogRecord struct {
	Bytes       int64  `json:"bytes"`        // the log's whole size
	ServedBytes int64  `json:"served_bytes"` // what is served and hashed
	SHA256      string `json:"sha256"`       // of the served bytes
	Truncated   bool   `json:"truncated"`    // the log was longer than the cap
}

// recordLog hashes the served part of the record's log and makes the file
// read-only, so what was hashed is what is served. No log, no record.
func recordLog(rec string, capBytes int64) *LogRecord {
	if capBytes <= 0 {
		capBytes = DefaultLogCapBytes
	}
	p := filepath.Join(rec, logName)
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	served := min(fi.Size(), capBytes)
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, served))
	if err != nil || n != served {
		return nil
	}
	os.Chmod(p, 0o400)
	return &LogRecord{Bytes: fi.Size(), ServedBytes: served, SHA256: hex.EncodeToString(h.Sum(nil)), Truncated: fi.Size() > served}
}

// LogRecordOf returns an ended attempt's log record and the log's path.
func (m *Manager) LogRecordOf(id string) (LogRecord, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.t.entries[id]
	switch {
	case e == nil:
		return LogRecord{}, "", fmt.Errorf("manager: no attempt %s", id)
	case !e.Terminal():
		return LogRecord{}, "", fmt.Errorf("manager: attempt %s has not ended; its log is recorded when it does", id)
	case e.Log == nil:
		return LogRecord{}, "", fmt.Errorf("manager: attempt %s has no output log", id)
	}
	return *e.Log, filepath.Join(m.cfg.Venue.Root(), attemptsDir, id, logName), nil
}

// Headers of a served log.
const (
	LogSHA256Header    = "X-Pomar-Sha256"
	LogTruncatedHeader = "X-Pomar-Truncated"
)

// logRoutes serves an ended attempt's log, as recorded: a read, on both sockets.
func (m *Manager) logRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/attempts/{id}/log", m.attemptGuard(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID.MatchString(id) {
			reply(w, http.StatusBadRequest, map[string]string{"error": "invalid attempt id"})
			return
		}
		rec, p, err := m.LogRecordOf(id)
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
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", strconv.FormatInt(rec.ServedBytes, 10))
		w.Header().Set(LogSHA256Header, rec.SHA256)
		w.Header().Set(LogTruncatedHeader, strconv.FormatBool(rec.Truncated))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, io.LimitReader(f, rec.ServedBytes))
	}))
}

// Log fetches an ended attempt's log and checks it against the output_log of
// the attempt's result document, never against anything the log's own
// response says: a header travels with the bytes it describes, so it proves
// nothing about them (the elders' ruling of 2026-09-27 17:21Z §4). With pub,
// the result's signature is verified first, so the log is checked against
// what the manager signed; without it, against the document as served. It
// returns the bytes, their sha256, and whether the log was truncated.
func (c *Client) Log(id string, pub []byte) ([]byte, string, bool, error) {
	raw, err := c.Result(id)
	if err != nil {
		return nil, "", false, err
	}
	var r ResultReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, "", false, fmt.Errorf("manager: result of %s: %w", id, err)
	}
	if pub != nil {
		sig, err := base64.StdEncoding.DecodeString(r.Signature)
		if err != nil || len(sig) == 0 {
			return nil, "", false, fmt.Errorf("manager: the result of %s is not signed", id)
		}
		if !result.Verify(pub, r.Result, sig) {
			return nil, "", false, fmt.Errorf("manager: the result of %s does not verify against the pinned key", id)
		}
	}
	var doc struct {
		Attempt string     `json:"attempt"`
		Log     *LogRecord `json:"output_log"`
	}
	if err := json.Unmarshal(r.Result, &doc); err != nil {
		return nil, "", false, fmt.Errorf("manager: result of %s: %w", id, err)
	}
	if doc.Attempt != id {
		return nil, "", false, fmt.Errorf("manager: the result served for %s is attempt %q's", id, doc.Attempt)
	}
	if doc.Log == nil {
		return nil, "", false, fmt.Errorf("manager: the result of %s records no output log", id)
	}
	resp, err := c.http.Get("http://manager/v1/attempts/" + id + "/log")
	if err != nil {
		return nil, "", false, fmt.Errorf("manager not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		return nil, "", false, fmt.Errorf("manager: %s: %s", resp.Status, e["error"])
	}
	// One byte more than the record says is served, so a longer body is caught.
	b, err := io.ReadAll(io.LimitReader(resp.Body, min(doc.Log.ServedBytes, maxLogRead)+1))
	if err != nil {
		return nil, "", false, err
	}
	h := sha256.Sum256(b)
	if got := hex.EncodeToString(h[:]); int64(len(b)) != doc.Log.ServedBytes || got != doc.Log.SHA256 {
		return nil, "", false, fmt.Errorf("manager: log of %s: %d bytes with sha256 %s, not the %d bytes with sha256 %s its result records",
			id, len(b), got, doc.Log.ServedBytes, doc.Log.SHA256)
	}
	return b, doc.Log.SHA256, doc.Log.Truncated, nil
}
