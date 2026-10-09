package manager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/result"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// openSigning opens a manager with a signing key (or none) and a control
// socket, and serves it.
func openSigning(t *testing.T, sign bool) (*Manager, *Client, *venue.Venue) {
	t.Helper()
	base := shortDir(t)
	root := filepath.Join(base, "root")
	os.Mkdir(root, 0o700)
	v, err := venue.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	var signer *result.Signer
	if sign {
		if err := v.EnsureCache(venue.KindVolume, "keys", "keys"); err != nil {
			t.Fatal(err)
		}
		if signer, err = result.LoadOrCreate(filepath.Join(root, "keys"), os.Getuid()); err != nil {
			t.Fatal(err)
		}
	}
	run := filepath.Join(base, "run")
	os.Mkdir(run, 0o750)
	os.Chmod(run, 0o750)
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, CtlSocket: filepath.Join(run, "ctl.sock"), Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { m.Serve(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped; m.Close() })
	waitTestSocket(t, filepath.Join(run, "ctl.sock"), 0o660)
	return m, NewSocketClient(filepath.Join(run, "ctl.sock")), v
}

func TestResultBindsAdmittedInputsAfterTableReload(t *testing.T) {
	m, ctl, v := openSigning(t, true)
	id := "res-inputs"
	rec := filepath.Join(v.Root(), attemptsDir, id)
	if err := os.MkdirAll(rec, 0700); err != nil {
		t.Fatal(err)
	}
	input := in("opportunity-sources.json", `{"qualification":"synthetic-only"}`)
	admitted, err := checkInputs([]Input{input})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeInputs(rec, []Input{input}); err != nil {
		t.Fatal(err)
	}
	e := terminalEntry(id)
	e.State, e.ExitCode, e.Inputs = StateRunning, nil, admitted
	m.mu.Lock()
	m.t.entries[id] = &e
	err = m.t.save()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTable(m.t.path)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.t = loaded
	m.mu.Unlock()
	// Signing uses the retained admission record, never rehashes a file
	// that could have changed after delivery to the guest.
	if err := os.WriteFile(filepath.Join(rec, inputsDir, input.Name), []byte("changed after admission"), 0600); err != nil {
		t.Fatal(err)
	}
	code := 0
	m.finish(id, StateExited, "", &code)
	b, err := ctl.Result(id)
	if err != nil {
		t.Fatal(err)
	}
	var reply ResultReply
	if err := json.Unmarshal(b, &reply); err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(reply.Signature)
	if err != nil || !result.Verify(m.cfg.Signer.Public(), reply.Result, sig) {
		t.Fatal("original signed input record does not verify")
	}
	var doc struct {
		Inputs []InputRecord `json:"inputs"`
	}
	if err := json.Unmarshal(reply.Result, &doc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.Inputs, admitted) || bytes.Contains(reply.Result, input.Data) {
		t.Fatalf("signed input evidence differs from admission: %+v", doc.Inputs)
	}
	for _, replacement := range []string{strings.Repeat("0", 64), strings.Repeat("f", 64)} {
		forged := bytes.Replace(reply.Result, []byte(input.SHA256), []byte(replacement), 1)
		if bytes.Equal(forged, reply.Result) || result.Verify(m.cfg.Signer.Public(), forged, sig) {
			t.Fatal("tampered input digest verifies")
		}
	}
	if _, exists := m.resultDoc(terminalEntry("no-inputs"))["inputs"]; exists {
		t.Fatal("legacy no-input result changed")
	}
}

func terminalEntry(id string) Entry {
	code := 0
	return Entry{
		Attempt: id, Command: []string{"make", "verify"}, State: StateExited, ExitCode: &code,
		Created: time.Date(2026, 9, 26, 5, 0, 0, 0, time.UTC), Ended: time.Date(2026, 9, 26, 5, 6, 0, 0, time.UTC),
		Class:   capacity.CI,
		Source:  &PinnedSource{Mirror: "example", Ref: "main", SHA: strings.Repeat("a", 40), Git: true, Base: "main"},
		GoProxy: true,
		Pins: &Pins{Kernel: "k", Init: "i", Image: "img", ImageArm64: "arm", PackageSet: "ps", Pomar: "v", HostBin: "hb",
			CommandHash: "ch"},
	}
}

// A terminal attempt's result is written once, signed, served on the control
// socket, verifiable against the published key, and never rewritten.
func TestResultIsWrittenOnceSignedAndVerifiable(t *testing.T) {
	m, ctl, v := openSigning(t, true)
	id := "res-1"
	if err := os.MkdirAll(filepath.Join(v.Root(), attemptsDir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	e := terminalEntry(id)
	m.writeResult(e)
	var r ResultReply
	if err := ctl.Do("GET", "/v1/attempts/"+id+"/result", nil, &r); err != nil {
		t.Fatal(err)
	}
	var k KeyReply
	if err := ctl.Do("GET", "/v1/signing-key", nil, &k); err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.StdEncoding.DecodeString(k.PublicKey)
	sig, _ := base64.StdEncoding.DecodeString(r.Signature)
	if !result.Verify(pub, r.Result, sig) || r.KeyID != k.KeyID {
		t.Fatalf("the served result does not verify against the served key (key %s, result key %s)", k.KeyID, r.KeyID)
	}
	var doc map[string]any
	if err := json.Unmarshal(r.Result, &doc); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"schema": result.Schema, "attempt": id, "state": "exited", "exit_code": 0.0, "sha": strings.Repeat("a", 40),
		"release_base": "main", "command_sha256": "ch", "image_digest": "img", "kernel_sha256": "k", "host": k.KeyID,
	} {
		if doc[field] != want {
			t.Fatalf("result %s = %v, want %v", field, doc[field], want)
		}
	}
	env, _ := doc["env"].(map[string]any)
	if env["HOME"] != jobHome || env["GOPROXY"] != jobGoProxy || env["uid"] != float64(jobUID) {
		t.Fatalf("result env = %v", env)
	}
	// Written once: a second finish of the same attempt changes nothing.
	before, _ := os.ReadFile(filepath.Join(v.Root(), attemptsDir, id, result.DocName))
	e.State = StateFailed
	m.writeResult(e)
	after, _ := os.ReadFile(filepath.Join(v.Root(), attemptsDir, id, result.DocName))
	if string(before) != string(after) {
		t.Fatal("the result was rewritten")
	}
	// A result edited by hand no longer verifies.
	forged := []byte(strings.Replace(string(r.Result), `"exit_code":0`, `"exit_code":1`, 1))
	if result.Verify(pub, forged, sig) {
		t.Fatal("an edited result verifies")
	}
}

// Without a key the result is still written, unsigned, and the manager
// publishes no key.
func TestUnsignedManagerWritesTheResultButServesNoKey(t *testing.T) {
	m, ctl, v := openSigning(t, false)
	id := "res-2"
	os.MkdirAll(filepath.Join(v.Root(), attemptsDir, id), 0o700)
	m.writeResult(terminalEntry(id))
	var r ResultReply
	if err := ctl.Do("GET", "/v1/attempts/"+id+"/result", nil, &r); err != nil {
		t.Fatal(err)
	}
	if r.Signature != "" || r.KeyID != "" {
		t.Fatalf("an unsigned manager returned a signature: %+v", r)
	}
	if err := ctl.Do("GET", "/v1/signing-key", nil, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("signing-key on an unsigned manager: %v, want 404", err)
	}
	if err := ctl.Do("GET", "/v1/attempts/no-such/result", nil, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a missing result: %v, want 404", err)
	}
}

// Through the control socket, the result document arrives byte for byte as
// signed, with <, >, & and non-ASCII in it, and verifies as received.
func TestResultIsServedExactlyAsSigned(t *testing.T) {
	m, ctl, v := openSigning(t, true)
	id := "res-exact"
	if err := os.MkdirAll(filepath.Join(v.Root(), attemptsDir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	e := terminalEntry(id)
	e.Command = []string{"/bin/sh", "-c", "make verify RELEASE_BASE=abc > out.log 2>&1 && echo 'déjà' < in"}
	m.writeResult(e)
	signed, sig, err := result.Read(filepath.Join(v.Root(), attemptsDir, id))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ctl.Result(id)
	if err != nil {
		t.Fatal(err)
	}
	var r ResultReply
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.Result, signed) {
		t.Fatalf("served document differs from the signed one:\n%s\n%s", r.Result, signed)
	}
	if !result.Verify(m.cfg.Signer.Public(), r.Result, sig) {
		t.Fatal("the received document does not verify")
	}
	if bytes.Contains(b, []byte("\\u003c")) || bytes.Contains(b, []byte("\\u0026")) || bytes.Contains(b, []byte("\\u003e")) {
		t.Fatalf("the reply escaped the document: %s", b)
	}
}
