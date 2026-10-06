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

func TestValidateAgent(t *testing.T) {
	for _, ok := range []HostConfig{{}, {Agent: "codex"}, {Agent: "claude", AgentVersion: "2.1.280"}, {Agent: "claude", AgentVersion: "2.1.280", ControllerCapabilities: []string{"inbox"}}} {
		if err := ValidateAgent(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []HostConfig{
		{Agent: "claude"},
		{Agent: "claude", AgentVersion: "latest"},
		{Agent: "claude", AgentVersion: "2.1.280; rm"},
		// The controller bridge is qualified per Claude Code version.
		{Agent: "claude", AgentVersion: "2.1.279", ControllerCapabilities: []string{"inbox"}},
		{Agent: "codex", AgentVersion: "1.0.0"},
		{Agent: "other"},
	} {
		if err := ValidateAgent(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// A Claude Code profile carries its agent, version and archive into the VM
// spec the Swift owner reads; the host's Codex default is untouched.
func TestClaudeProfileReachesTheVMSpec(t *testing.T) {
	root, err := os.MkdirTemp("", "pomar-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	os.Chmod(root, 0o700)
	p := EnvironmentProfile{Base: filepath.Join(root, "node.ext4"), ImageDigest: "sha256:" + strings.Repeat("a", 64), SourceBundle: filepath.Join(root, "src.bundle"),
		SourceSHA: strings.Repeat("b", 40), AllowedHosts: []string{"api.anthropic.com"},
		Agent: "claude", AgentVersion: "2.1.280", AgentArchive: filepath.Join(root, "claude.tar.gz"), AgentArchiveSHA256: strings.Repeat("d", 64)}
	p.ImageRef = "docker.io/library/node@" + p.ImageDigest
	cfg := HostConfig{Root: root, MaxLive: 1, CPUs: 2, MemoryBytes: 4 << 30, SourceSHA: strings.Repeat("c", 40), CodexArchive: "/codex.tar.gz", CodexArchiveSHA256: strings.Repeat("e", 64),
		AllowedHosts: []string{"auth.example"}, Profiles: map[string]EnvironmentProfile{"claude-dev": p}}
	h, err := OpenHost(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	r := httptest.NewRecorder()
	h.Handler().ServeHTTP(r, httptest.NewRequest("POST", "/v1/environments", bytes.NewBufferString(`{"id":"cc","operation_id":"create-cc","profile":"claude-dev"}`)))
	if r.Code != 201 {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var e struct {
		Spec map[string]any `json:"spec"`
	}
	json.Unmarshal(r.Body.Bytes(), &e)
	if e.Spec["agent"] != "claude" || e.Spec["agentVersion"] != "2.1.280" || e.Spec["codexArchive"] != p.AgentArchive || e.Spec["codexArchiveSHA256"] != p.AgentArchiveSHA256 {
		t.Fatalf("spec %v", e.Spec)
	}
	r = httptest.NewRecorder()
	h.Handler().ServeHTTP(r, httptest.NewRequest("POST", "/v1/environments", bytes.NewBufferString(`{"id":"cx","operation_id":"create-cx"}`)))
	var d struct {
		Spec map[string]any `json:"spec"`
	}
	json.Unmarshal(r.Body.Bytes(), &d)
	if _, set := d.Spec["agent"]; set || d.Spec["codexArchive"] != "/codex.tar.gz" {
		t.Fatalf("the default spec changed: %v", d.Spec)
	}
}

func TestBadAgentProfilesAreRefused(t *testing.T) {
	root, _ := os.MkdirTemp("", "pomar-agent-")
	t.Cleanup(func() { os.RemoveAll(root) })
	os.Chmod(root, 0o700)
	base := EnvironmentProfile{Base: filepath.Join(root, "b.ext4"), ImageDigest: "sha256:" + strings.Repeat("a", 64), SourceBundle: filepath.Join(root, "s.bundle"), SourceSHA: strings.Repeat("b", 40), AllowedHosts: []string{"api.anthropic.com"}}
	base.ImageRef = "docker.io/library/node@" + base.ImageDigest
	for name, mutate := range map[string]func(*EnvironmentProfile){
		"claude without a version": func(p *EnvironmentProfile) { p.Agent = "claude" },
		"claude with capabilities on an unqualified version": func(p *EnvironmentProfile) {
			p.Agent, p.AgentVersion, p.ControllerCapabilities = "claude", "2.1.279", []string{"inbox"}
		},
		"an archive without its sha256": func(p *EnvironmentProfile) {
			p.Agent, p.AgentVersion, p.AgentArchive = "claude", "2.1.280", "/a.tar.gz"
		},
		"a relative archive": func(p *EnvironmentProfile) {
			p.AgentArchive, p.AgentArchiveSHA256 = "a.tar.gz", strings.Repeat("d", 64)
		},
	} {
		p := base
		mutate(&p)
		cfg := HostConfig{Root: root, MaxLive: 1, CPUs: 2, MemoryBytes: 4 << 30, Profiles: map[string]EnvironmentProfile{"p": p}}
		if h, err := OpenHost(cfg); err == nil {
			h.Close()
			t.Errorf("%s: accepted", name)
		}
	}
}
