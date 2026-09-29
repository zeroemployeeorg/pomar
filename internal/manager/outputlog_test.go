package manager

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
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

	// Live: no result yet, so nothing to check a log against.
	if _, _, _, err := ctl.Log(id, nil); err == nil || !strings.Contains(err.Error(), "no result for "+id) {
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
	b, sum, truncated, err := ctl.Log(id, nil)
	if err != nil || !bytes.Equal(b, []byte("line one\nline two\n")) || sum != want.SHA256 || truncated {
		t.Fatalf("Log = %q %s %v %v", b, sum, truncated, err)
	}
	if _, _, _, err := ctl.Log("nope", nil); err == nil {
		t.Fatal("a missing attempt's log was served")
	}
}

// The log is checked against the result's output_log, never the response's
// own header: a log served with bytes and a header that agree with each other
// but not with the result is refused (the elders' ruling of 2026-09-27 17:21Z
// §4). With the pinned key, the result's signature is verified first.
func TestLogIsCheckedAgainstTheSignedResult(t *testing.T) {
	m, ctl, v := openSigning(t, true)
	id := "log-2"
	rec := filepath.Join(v.Root(), attemptsDir, id)
	os.MkdirAll(rec, 0o700)
	os.WriteFile(filepath.Join(rec, logName), []byte("green\n"), 0o600)
	e := terminalEntry(id)
	e.State, e.ExitCode = StateRunning, nil
	m.mu.Lock()
	m.t.entries[id] = &e
	m.mu.Unlock()
	code := 0
	m.finish(id, StateExited, "", &code)

	var k KeyReply
	if err := ctl.Do("GET", "/v1/signing-key", nil, &k); err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.StdEncoding.DecodeString(k.PublicKey)
	if b, _, _, err := ctl.Log(id, pub); err != nil || string(b) != "green\n" {
		t.Fatalf("Log with the pinned key: %q, %v", b, err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if _, _, _, err := ctl.Log(id, other); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("Log with another key: %v", err)
	}

	// The same number of bytes, changed, with the entry (and so the served
	// header) changed to match them: only the signed result still says green.
	p := filepath.Join(rec, logName)
	os.Chmod(p, 0o600)
	os.WriteFile(p, []byte("red!!\n"), 0o600)
	m.mu.Lock()
	m.t.entries[id].Log.SHA256 = sha("red!!\n")
	m.mu.Unlock()
	for _, key := range [][]byte{pub, nil} {
		if _, _, _, err := ctl.Log(id, key); err == nil || !strings.Contains(err.Error(), "its result records") {
			t.Fatalf("a log that matches its header but not its result (key %v): %v", key != nil, err)
		}
	}
}
