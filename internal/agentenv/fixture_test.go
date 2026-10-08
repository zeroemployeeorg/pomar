package agentenv

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRoot makes a private host root with a fixtures directory and one
// synthetic fixture in it.
func fixtureRoot(t *testing.T) (string, QualificationFixture) {
	t.Helper()
	root, err := os.MkdirTemp("", "pomar-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	os.Chmod(root, 0o700)
	if err := os.Mkdir(filepath.Join(root, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"claudeAiOauth":{"accessToken":"pomar-canary-test","refreshToken":"pomar-canary-test","expiresAt":4102444800000}}`)
	src := filepath.Join(root, "fixtures", "claude-canary.json")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return root, QualificationFixture{Source: src, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), Destination: ClaudeQualificationDestination}
}

func TestValidateFixtureAcceptsOwnerCustody(t *testing.T) {
	root, f := fixtureRoot(t)
	if err := ValidateFixture(root, f, "claude", os.Getuid()); err != nil {
		t.Fatal(err)
	}
}

// Every custody rule refuses: the description, the location, symlinks at the
// file or its directory, a hard link, modes, the owner, and the bytes.
func TestValidateFixtureRefusals(t *testing.T) {
	for name, mutate := range map[string]func(t *testing.T, root string, f *QualificationFixture) (agent string, uid int){
		"another agent": func(_ *testing.T, _ string, _ *QualificationFixture) (string, int) { return "codex", os.Getuid() },
		"another destination": func(_ *testing.T, _ string, f *QualificationFixture) (string, int) {
			f.Destination = "/etc/passwd"
			return "claude", os.Getuid()
		},
		"a wrong digest": func(_ *testing.T, _ string, f *QualificationFixture) (string, int) {
			f.SHA256 = strings.Repeat("0", 64)
			return "claude", os.Getuid()
		},
		"a wrong size": func(_ *testing.T, _ string, f *QualificationFixture) (string, int) {
			f.Size++
			return "claude", os.Getuid()
		},
		"too large": func(_ *testing.T, _ string, f *QualificationFixture) (string, int) {
			f.Size = maxFixtureSize + 1
			return "claude", os.Getuid()
		},
		"outside the fixtures directory": func(t *testing.T, root string, f *QualificationFixture) (string, int) {
			other := filepath.Join(root, "elsewhere.json")
			data, _ := os.ReadFile(f.Source)
			os.WriteFile(other, data, 0o600)
			f.Source = other
			return "claude", os.Getuid()
		},
		"a symlinked file": func(t *testing.T, root string, f *QualificationFixture) (string, int) {
			real := filepath.Join(root, "real.json")
			os.Rename(f.Source, real)
			os.Symlink(real, f.Source)
			return "claude", os.Getuid()
		},
		"a symlinked fixtures directory": func(t *testing.T, root string, f *QualificationFixture) (string, int) {
			moved := filepath.Join(root, "moved")
			os.Rename(filepath.Join(root, "fixtures"), moved)
			os.Symlink(moved, filepath.Join(root, "fixtures"))
			return "claude", os.Getuid()
		},
		"a hard link": func(t *testing.T, root string, f *QualificationFixture) (string, int) {
			os.Link(f.Source, filepath.Join(root, "second-link.json"))
			return "claude", os.Getuid()
		},
		"a readable file": func(_ *testing.T, _ string, f *QualificationFixture) (string, int) {
			os.Chmod(f.Source, 0o644)
			return "claude", os.Getuid()
		},
		"an open fixtures directory": func(_ *testing.T, root string, _ *QualificationFixture) (string, int) {
			os.Chmod(filepath.Join(root, "fixtures"), 0o755)
			return "claude", os.Getuid()
		},
		"another owner": func(_ *testing.T, _ string, _ *QualificationFixture) (string, int) { return "claude", os.Getuid() + 1 },
	} {
		root, f := fixtureRoot(t)
		agent, uid := mutate(t, root, &f)
		if err := ValidateFixture(root, f, agent, uid); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A source replaced between the read and the identity recheck is refused,
// never followed.
func TestValidateFixtureRefusesReplacement(t *testing.T) {
	for name, replace := range map[string]func(root string, f QualificationFixture){
		"the file": func(_ string, f QualificationFixture) {
			data, _ := os.ReadFile(f.Source)
			tmp := f.Source + ".new"
			os.WriteFile(tmp, data, 0o600)
			os.Rename(tmp, f.Source) // same bytes, another file
		},
		"the fixtures directory": func(root string, f QualificationFixture) {
			dir := filepath.Join(root, "fixtures")
			data, _ := os.ReadFile(f.Source)
			os.Rename(dir, filepath.Join(root, "old-fixtures"))
			os.Mkdir(dir, 0o700)
			os.WriteFile(f.Source, data, 0o600)
		},
	} {
		root, f := fixtureRoot(t)
		fixtureAfterRead = func() { replace(root, f) }
		err := ValidateFixture(root, f, "claude", os.Getuid())
		fixtureAfterRead = func() {}
		if err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Errorf("%s replaced: %v", name, err)
		}
	}
}

// Only a qualification profile carries a fixture into a VM spec; the host
// configuration itself cannot, nor can a Codex profile.
func TestQualificationFixtureProfileRules(t *testing.T) {
	root, f := fixtureRoot(t)
	profile := EnvironmentProfile{Base: filepath.Join(root, "b.ext4"), ImageDigest: "sha256:" + strings.Repeat("a", 64), SourceBundle: filepath.Join(root, "s.bundle"), SourceSHA: strings.Repeat("b", 40), AllowedHosts: []string{"api.anthropic.com"},
		Agent: "claude", AgentVersion: "2.1.280", Qualification: true, QualificationFixture: &f}
	profile.ImageRef = "docker.io/library/golang@" + profile.ImageDigest
	base := HostConfig{Root: root, MaxLive: 1, CPUs: 2, MemoryBytes: 4 << 30, SourceSHA: strings.Repeat("c", 40), CodexArchive: "/a.tar.gz", CodexArchiveSHA256: strings.Repeat("d", 64)}

	cfg := base
	cfg.Profiles = map[string]EnvironmentProfile{"claude-qualification": profile}
	h, err := OpenHost(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.profileConfig("claude-qualification")
	h.Close()
	if err != nil || got.QualificationFixture == nil || *got.QualificationFixture != f {
		t.Fatalf("the qualification profile's spec: %+v, %v", got.QualificationFixture, err)
	}

	for name, mutate := range map[string]func(c *HostConfig){
		"a profile not marked qualification": func(c *HostConfig) {
			p := profile
			p.Qualification = false
			c.Profiles = map[string]EnvironmentProfile{"p": p}
		},
		"a Codex profile": func(c *HostConfig) {
			p := profile
			p.Agent, p.AgentVersion = "", ""
			c.Profiles = map[string]EnvironmentProfile{"p": p}
		},
		"the host configuration itself": func(c *HostConfig) { c.QualificationFixture = &f },
	} {
		c := base
		mutate(&c)
		if h, err := OpenHost(c); err == nil {
			h.Close()
			t.Errorf("%s: accepted", name)
		}
	}

	// A profile without a fixture puts none in its spec.
	plain := profile
	plain.Qualification, plain.QualificationFixture = false, nil
	cfg = base
	cfg.Profiles = map[string]EnvironmentProfile{"plain": plain}
	if h, err = OpenHost(cfg); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if got, _ := h.profileConfig("plain"); got.QualificationFixture != nil {
		t.Fatal("a plain profile carried a fixture")
	}
	if got, _ := h.profileConfig(""); got.QualificationFixture != nil {
		t.Fatal("the default spec carried a fixture")
	}
}
