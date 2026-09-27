package manager

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordLog(t *testing.T) {
	rec := t.TempDir()
	if recordLog(rec, 0) != nil {
		t.Fatal("a record with no log has a log record")
	}
	p := filepath.Join(rec, logName)
	os.WriteFile(p, []byte("hello from the guest\n"), 0o600)
	r := recordLog(rec, 0)
	if r == nil || r.Bytes != 21 || r.ServedBytes != 21 || r.Truncated || r.SHA256 != sha("hello from the guest\n") {
		t.Fatalf("record = %+v", r)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o400 {
		t.Fatalf("mode %v, want read-only", fi.Mode().Perm())
	}
}

// A log over its class's cap is recorded as truncated, and its sha256 covers
// exactly the first cap bytes, which is what is served; no cap means the
// default.
func TestRecordLogOverTheCapIsTruncated(t *testing.T) {
	rec := t.TempDir()
	os.WriteFile(filepath.Join(rec, logName), []byte(strings.Repeat("x", 5000)), 0o600)
	r := recordLog(rec, 1000)
	if r == nil || r.Bytes != 5000 || r.ServedBytes != 1000 || !r.Truncated || r.SHA256 != sha(strings.Repeat("x", 1000)) {
		t.Fatalf("class cap 1000: %+v", r)
	}
	rec = t.TempDir()
	f, _ := os.Create(filepath.Join(rec, logName))
	f.Truncate(DefaultLogCapBytes + 1000) // sparse: zeros
	f.Close()
	r = recordLog(rec, 0)
	if r == nil || r.Bytes != DefaultLogCapBytes+1000 || r.ServedBytes != DefaultLogCapBytes || !r.Truncated {
		t.Fatalf("default cap: %+v", r)
	}
	if r.SHA256 != sha(string(make([]byte, DefaultLogCapBytes))) {
		t.Fatal("the sha256 does not cover exactly the served bytes")
	}
}

// An ended attempt records its log in the entry and the result, and the
// control socket serves exactly the recorded bytes, checked by the client.
func TestFinishRecordsAndServesTheLog(t *testing.T) {
	m, ctl, v := openSigning(t, false)
	id := "log-1"
	rec := filepath.Join(v.Root(), attemptsDir, id)
	os.MkdirAll(rec, 0o700)
	os.WriteFile(filepath.Join(rec, logName), []byte("line one\nline two\n"), 0o600)
	e := terminalEntry(id)
	e.State, e.ExitCode = StateRunning, nil
	m.mu.Lock()
	m.t.entries[id] = &e
	m.mu.Unlock()

	// Live: not served yet.
	if _, _, _, err := ctl.Log(id); err == nil || !strings.Contains(err.Error(), "has not ended") {
		t.Fatalf("a live attempt's log: %v", err)
	}
	code := 0
	m.finish(id, StateExited, "", &code)

	want := LogRecord{Bytes: 18, ServedBytes: 18, SHA256: sha("line one\nline two\n")}
	if got := entry(m, id).Log; got == nil || *got != want {
		t.Fatalf("entry log = %+v", got)
	}
	var r ResultReply
	if err := ctl.Do("GET", "/v1/attempts/"+id+"/result", nil, &r); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Log *LogRecord `json:"output_log"`
	}
	if err := json.Unmarshal(r.Result, &doc); err != nil || doc.Log == nil || *doc.Log != want {
		t.Fatalf("result output_log = %+v, %v", doc.Log, err)
	}
	b, sum, truncated, err := ctl.Log(id)
	if err != nil || !bytes.Equal(b, []byte("line one\nline two\n")) || sum != want.SHA256 || truncated {
		t.Fatalf("Log = %q %s %v %v", b, sum, truncated, err)
	}
	if _, _, _, err := ctl.Log("nope"); err == nil {
		t.Fatal("a missing attempt's log was served")
	}
}
