package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
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
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(filepath.Join(run, "ctl.sock")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return m, NewSocketClient(filepath.Join(run, "ctl.sock")), v
}

func terminalEntry(id string) Entry {
	code := 0
	return Entry{
		Attempt: id, Command: []string{"make", "verify"}, State: StateExited, ExitCode: &code,
		Created: time.Date(2026, 9, 26, 5, 0, 0, 0, time.UTC), Ended: time.Date(2026, 9, 26, 5, 6, 0, 0, time.UTC),
		Class:   capacity.CI,
		Source:  &PinnedSource{Mirror: "zero-employee", Ref: "main", SHA: strings.Repeat("a", 40), Git: true, Base: "main"},
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
