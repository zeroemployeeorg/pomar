package agentenv

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectProfileKeepsSeparateSourceAndNetworkOnReopen(t *testing.T) {
	// Darwin Unix sockets have a short path bound; test names can exceed it.
	root, err := os.MkdirTemp("", "pomar-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := EnvironmentProfile{Base: filepath.Join(root, "python.ext4"), ImageDigest: "sha256:" + strings.Repeat("a", 64), SourceBundle: filepath.Join(root, "zeocore.bundle"), SourceSHA: strings.Repeat("b", 40), AllowedHosts: []string{"pypi.org"}}
	p.ImageRef = "docker.io/library/python@" + p.ImageDigest
	cfg := HostConfig{Root: root, MaxLive: 1, CPUs: 2, MemoryBytes: 4 << 30, SourceSHA: strings.Repeat("c", 40), AllowedHosts: []string{"auth.example"}, Profiles: map[string]EnvironmentProfile{"python-project": p}}
	h, err := OpenHost(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if h != nil {
			h.Close()
		}
	}()
	create := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.Handler().ServeHTTP(r, httptest.NewRequest("POST", "/v1/environments", bytes.NewBufferString(body)))
		return r
	}
	for _, body := range []string{`{"id":"python","operation_id":"create-python","profile":"missing"}`, `{"id":"python","operation_id":"create-python","sourceSHA":"evil"}`} {
		if r := create(body); r.Code != 400 {
			t.Fatalf("unowned inputs admitted: %d %s", r.Code, r.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "python")); !os.IsNotExist(err) {
		t.Fatal("rejected creation allocated environment", err)
	}
	body := `{"id":"python","operation_id":"create-python","profile":"python-project"}`
	r := create(body)
	if r.Code != 201 {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var e Environment
	if err := json.Unmarshal(r.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Spec.SourceSHA != p.SourceSHA || e.Spec.ImageDigest != p.ImageDigest || e.Spec.Profile != "python-project" || e.Spec.Profiles != nil {
		t.Fatal("project inputs leaked or lost", e.Spec)
	}
	if r := create(body); r.Code != 200 {
		t.Fatal("healthy duplicate refused", r.Code)
	}
	if r := create(`{"id":"python","operation_id":"create-python"}`); r.Code != 409 {
		t.Fatal("duplicate switched project", r.Code)
	}
	if r := create(`{"id":"go","operation_id":"create-go"}`); r.Code != 201 {
		t.Fatal("default refused", r.Code)
	}
	if h.envs["go"].Spec.SourceSHA != cfg.SourceSHA {
		t.Fatal("default source changed")
	}
	if err := h.gate(h.envs["python"]); err != nil {
		t.Fatal(err)
	}
	if !h.gates["python"].allowed["pypi.org"] || h.gates["python"].allowed["auth.example"] {
		t.Fatal("used another project's network policy")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h = nil
	// Owner artifact refresh may not silently replace a retained project's input.
	p.SourceSHA = strings.Repeat("d", 40)
	cfg.Profiles["python-project"] = p
	h, err = OpenHost(cfg)
	if err != nil {
		t.Fatal(err)
	}
	retained := h.envs["python"]
	before := retained.Spec.Incarnation
	if err := h.start(retained); err == nil || !strings.Contains(err.Error(), "retained project inputs changed") {
		t.Fatal("changed project relaunched", err)
	}
	if retained.Spec.Incarnation != before || retained.Phase != "created" || retained.Spec.SourceSHA != e.Spec.SourceSHA {
		t.Fatal("refused launch mutated identity")
	}
	delete(h.config.Profiles, "python-project")
	if err := h.start(retained); err == nil {
		t.Fatal("missing owner profile relaunched")
	}
}
