package distribution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// canonicalTempDir returns t.TempDir(), resolved via filepath.EvalSymlinks.
// On macOS, t.TempDir() returns an unresolved path under /var/folders/...,
// and /var -> private/var is itself a symlink (standard, permanent macOS
// furniture, not a defect) -- secureDirectory's own ancestor walk (by
// design) refuses any symlink component, so an unresolved prefix always
// failed here, for a reason having nothing to do with the behavior under
// test. This is a test-fixture fix only; secureDirectory and PrivateDir
// are unchanged -- a prefix reached through a symlink must still be
// refused, which is exactly what TestInstallFailsBeforeActivation's own
// "prefix-link" case deliberately constructs and continues to rely on.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func fixture(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range Files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if strings.HasPrefix(name, "bin/") {
			mode = 0755
		}
		if err := os.WriteFile(p, []byte("bytes "+name+version), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteManifest(root, version, testCommit); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestInstallVersionsAndRollback(t *testing.T) {
	prefix := filepath.Join(canonicalTempDir(t), "install")
	a := fixture(t, "v0.1.0")
	b := fixture(t, "v0.2.0-rc.1")
	for _, bundle := range []string{a, a, b, a} {
		m, err := Install(bundle, prefix)
		if err != nil {
			t.Fatal(err)
		}
		link, err := os.Readlink(filepath.Join(prefix, "current"))
		if err != nil || link != filepath.Join("releases", m.Version) {
			t.Fatalf("selection %s: %v", link, err)
		}
	}
	for _, v := range []string{"v0.1.0", "v0.2.0-rc.1"} {
		if _, err := Verify(filepath.Join(prefix, "releases", v)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentGuidesAreVerifiedBeforeActivation(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "docs/health-and-diagnostics.md", "docs/attempt-progress.md", "docs/job-failure-diagnostics.md", "docs/service-upgrade.md", "docs/sessions.md", "docs/sessions-protocol.md"} {
		t.Run(name, func(t *testing.T) {
			bundle := fixture(t, "v0.1.0")
			prefix := filepath.Join(canonicalTempDir(t), "install")
			if _, err := Install(bundle, prefix); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(prefix, "current", filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bundle, filepath.FromSlash(name)), []byte("changed agent instructions"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(bundle, prefix); err == nil {
				t.Fatal("changed guide was accepted")
			}
			after, err := os.ReadFile(filepath.Join(prefix, "current", filepath.FromSlash(name)))
			if err != nil || string(before) != string(after) {
				t.Fatal("refused install changed activated instructions")
			}
		})
	}
}
func TestInstallFailsBeforeActivation(t *testing.T) {
	for _, kind := range []string{"tamper", "unexpected", "symlink", "missing", "mode", "manifest-path", "existing-version", "prefix-link", "existing-current", "locked"} {
		t.Run(kind, func(t *testing.T) {
			a := fixture(t, "v0.1.0")
			prefix := filepath.Join(canonicalTempDir(t), "install")
			if _, err := Install(a, prefix); err != nil {
				t.Fatal(err)
			}
			b := fixture(t, "v0.2.0")
			p := filepath.Join(b, "bin", "pomar")
			switch kind {
			case "tamper":
				os.WriteFile(p, []byte("bad"), 0755)
			case "unexpected":
				os.WriteFile(filepath.Join(b, "extra"), nil, 0644)
			case "symlink":
				os.Remove(p)
				os.Symlink(filepath.Join(a, "bin", "pomar"), p)
			case "missing":
				os.Remove(p)
			case "mode":
				os.Chmod(p, 0777)
			case "manifest-path":
				raw, _ := os.ReadFile(filepath.Join(b, "release.json"))
				var m Manifest
				json.Unmarshal(raw, &m)
				m.Files["../escape"] = m.Files["bin/pomar"]
				delete(m.Files, "bin/pomar")
				raw, _ = json.Marshal(m)
				os.WriteFile(filepath.Join(b, "release.json"), raw, 0644)
			case "existing-version":
				b = fixture(t, "v0.1.0")
				os.WriteFile(filepath.Join(b, "bin", "pomar"), []byte("replacement"), 0755)
				WriteManifest(b, "v0.1.0", testCommit)
			case "prefix-link":
				link := filepath.Join(t.TempDir(), "linked")
				os.Symlink(prefix, link)
				prefix = link
			case "existing-current":
				os.Remove(filepath.Join(prefix, "current"))
				os.WriteFile(filepath.Join(prefix, "current"), []byte("retain me"), 0644)
			case "locked":
				os.Mkdir(filepath.Join(prefix, ".install-lock"), 0700)
			}
			if _, err := Install(b, prefix); err == nil {
				t.Fatal("accepted unsafe/conflicting installation")
			}
			if kind == "existing-current" {
				bytes, _ := os.ReadFile(filepath.Join(prefix, "current"))
				if string(bytes) != "retain me" {
					t.Fatal("overwrote foreign current")
				}
			} else {
				target, err := os.Readlink(filepath.Join(prefix, "current"))
				if err != nil || target != "releases/v0.1.0" {
					t.Fatalf("changed selection: %s %v", target, err)
				}
			}
		})
	}
}
func TestIdentityRejectsPathsAndFloatingVersions(t *testing.T) {
	for _, v := range []string{"latest", "main", "../v0.1.0", "v0.1.0/x", "v0.1", "v0.1.0\n", "v0.1.0+mutable"} {
		if ValidIdentity(v, testCommit) {
			t.Errorf("accepted %q", v)
		}
	}
	if ValidIdentity("v0.1.0", "abcd") {
		t.Fatal("accepted abbreviated commit")
	}
}
