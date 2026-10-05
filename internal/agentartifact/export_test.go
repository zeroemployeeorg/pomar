package agentartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

type fixtureResult struct {
	Session   agentenv.State    `json:"session"`
	Diff      string            `json:"diff"`
	Untracked map[string]string `json:"untracked"`
}

func fixture() (Binding, fixtureResult, Manifest, map[string][]byte) {
	binding := Binding{"env", "session", "incarnation", "workspace", "scope", strings.Repeat("a", 40), "release-task", strings.Repeat("b", 64)}
	s := agentenv.State{ContractVersion: "pomar.agent/v1", EnvironmentID: binding.Environment, SessionID: binding.Session, Incarnation: binding.Incarnation, WorkspaceID: binding.Workspace, ScopeID: binding.Scope, SourceSHA: binding.Source, Operations: map[string]agentenv.Operation{binding.Operation: {ID: binding.Operation, Incarnation: binding.Incarnation, InputHash: binding.InputHash, State: "finished", Completion: "completed", ActorAcknowledged: true}}}
	result := fixtureResult{Session: s, Untracked: map[string]string{}}
	// These bytes include invalid UTF-8, NUL and high bytes that JSON string
	// conversion would corrupt. The text carrier must return them unchanged.
	files := map[string][]byte{"example-1.0-py3-none-any.whl": {0, 0xff, 0xc0, 0xaf, 0x50, 0x4b}, "example-1.0.tar.gz": {0x1f, 0x8b, 0xfe, 0, 0x80}}
	manifest := Manifest{Version: "pomar.candidate/v1"}
	for _, item := range []struct{ kind, name string }{{"wheel", "example-1.0-py3-none-any.whl"}, {"sdist", "example-1.0.tar.gz"}} {
		data := files[item.name]
		hash := sha256.Sum256(data)
		a := Artifact{item.kind, item.name, "pomar-export/" + item.kind + ".b64", int64(len(data)), hex.EncodeToString(hash[:])}
		manifest.Artifacts = append(manifest.Artifacts, a)
		result.Untracked[a.EncodingPath] = base64.StdEncoding.EncodeToString(data)
	}
	return binding, result, manifest, files
}

func payload(t *testing.T, r fixtureResult, m Manifest) []byte {
	t.Helper()
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r.Untracked[ManifestPath] = string(manifest)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReceiveBinaryBytesAndPrivateReceipt(t *testing.T) {
	binding, r, m, files := fixture()
	output := filepath.Join(t.TempDir(), "candidate")
	receipt, err := Receive(bytes.NewReader(payload(t, r, m)), binding, output)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Binding != binding || len(receipt.Artifacts) != 2 {
		t.Fatal("missing receipt binding/artifacts")
	}
	for name, expected := range files {
		got, err := os.ReadFile(filepath.Join(output, name))
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("binary %s corrupted: %v", name, err)
		}
		info, err := os.Stat(filepath.Join(output, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("artifact permissions: %v", err)
		}
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(output, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Receipt
	if err := json.Unmarshal(got, &saved); err != nil || saved.Binding != binding {
		t.Fatal("receipt not bound")
	}
	if _, err := Receive(bytes.NewReader(payload(t, r, m)), binding, output); err == nil {
		t.Fatal("overwrote existing candidate")
	}
}

func TestReceiveRefusesUnboundCorruptAndOversizedCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Binding, *fixtureResult, *Manifest)
	}{
		{"wrong-session", func(_ *Binding, r *fixtureResult, _ *Manifest) { r.Session.SessionID = "other" }},
		{"wrong-scope", func(_ *Binding, r *fixtureResult, _ *Manifest) { r.Session.ScopeID = "other" }},
		{"wrong-source", func(_ *Binding, r *fixtureResult, _ *Manifest) { r.Session.SourceSHA = strings.Repeat("c", 40) }},
		{"wrong-input", func(b *Binding, _ *fixtureResult, _ *Manifest) { b.InputHash = strings.Repeat("c", 64) }},
		{"incomplete-binding", func(b *Binding, _ *fixtureResult, _ *Manifest) { b.Workspace = "" }},
		{"failed-turn", func(b *Binding, r *fixtureResult, _ *Manifest) {
			op := r.Session.Operations[b.Operation]
			op.Completion = "failed"
			r.Session.Operations[b.Operation] = op
		}},
		{"unacknowledged", func(b *Binding, r *fixtureResult, _ *Manifest) {
			op := r.Session.Operations[b.Operation]
			op.ActorAcknowledged = false
			r.Session.Operations[b.Operation] = op
		}},
		{"missing-artifact", func(_ *Binding, r *fixtureResult, _ *Manifest) { delete(r.Untracked, "pomar-export/wheel.b64") }},
		{"tampered-bytes", func(_ *Binding, r *fixtureResult, _ *Manifest) {
			r.Untracked["pomar-export/wheel.b64"] = base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4, 5, 6})
		}},
		{"bad-base64", func(_ *Binding, r *fixtureResult, _ *Manifest) { r.Untracked["pomar-export/wheel.b64"] = "!!!!!!!!" }},
		{"wrong-size", func(_ *Binding, _ *fixtureResult, m *Manifest) { m.Artifacts[0].Size++ }},
		{"escape-name", func(_ *Binding, _ *fixtureResult, m *Manifest) { m.Artifacts[0].Name = "../escape.whl" }},
		{"escape-carrier", func(_ *Binding, _ *fixtureResult, m *Manifest) { m.Artifacts[0].EncodingPath = "../escape.b64" }},
		{"duplicate-kind", func(_ *Binding, _ *fixtureResult, m *Manifest) { m.Artifacts[1] = m.Artifacts[0] }},
		{"extra-artifact", func(_ *Binding, _ *fixtureResult, m *Manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) }},
		{"file-count", func(_ *Binding, r *fixtureResult, _ *Manifest) {
			for i := range 32 {
				r.Untracked[string(rune('a'+i))] = "x"
			}
		}},
		{"untracked-budget", func(_ *Binding, r *fixtureResult, _ *Manifest) {
			r.Untracked["log"] = strings.Repeat("x", UntrackedLimit)
		}},
		{"artifact-budget", func(_ *Binding, r *fixtureResult, m *Manifest) {
			data := bytes.Repeat([]byte{0xfe}, DecodedLimit)
			hash := sha256.Sum256(data)
			m.Artifacts[0].Size = int64(len(data))
			m.Artifacts[0].SHA256 = hex.EncodeToString(hash[:])
			r.Untracked[m.Artifacts[0].EncodingPath] = base64.StdEncoding.EncodeToString(data)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, r, m, _ := fixture()
			tc.mutate(&b, &r, &m)
			output := filepath.Join(t.TempDir(), "candidate")
			if _, err := Receive(bytes.NewReader(payload(t, r, m)), b, output); err == nil {
				t.Fatal("unsafe candidate accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("invalid result created output")
			}
		})
	}
}

func TestReceiveRefusesSymlinkDestination(t *testing.T) {
	b, r, m, _ := fixture()
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "candidate")
	if err := os.Symlink(target, output); err != nil {
		t.Fatal(err)
	}
	if _, err := Receive(bytes.NewReader(payload(t, r, m)), b, output); err == nil {
		t.Fatal("followed existing symlink")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote through symlink")
	}
}
