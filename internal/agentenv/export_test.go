package agentenv

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func exportFixtureGit(t *testing.T, workspace string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture Git: %v %s", err, out)
	}
}

func TestResultRefusesTrackedAndUntrackedCredentialPaths(t *testing.T) {
	const canary = "SYNTHETIC-EXPORT-CREDENTIAL-CANARY"
	for _, name := range []string{".codex/auth.json", ".claude/.credentials.json", ".ssh/id_ed25519", ".env", "nested/.env.production", ".npmrc"} {
		for _, tracked := range []bool{false, true} {
			label := "untracked/"
			if tracked {
				label = "tracked/"
			}
			t.Run(label+name, func(t *testing.T) {
				workspace := t.TempDir()
				exportFixtureGit(t, workspace, "init", "-q")
				file := filepath.Join(workspace, name)
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if tracked {
					if err := os.WriteFile(file, []byte("synthetic baseline"), 0600); err != nil {
						t.Fatal(err)
					}
					exportFixtureGit(t, workspace, "add", "--", name)
				}
				exportFixtureGit(t, workspace, "commit", "--allow-empty", "-qm", "fixture")
				if err := os.WriteFile(file, []byte(canary), 0600); err != nil {
					t.Fatal(err)
				}
				b := NewBroker(newStore(t), workspace)
				w := httptest.NewRecorder()
				b.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/result", nil))
				if w.Code != 422 || !strings.Contains(w.Body.String(), "credential or private configuration path") || strings.Contains(w.Body.String(), canary) {
					t.Fatalf("credential export: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestResultExportsLiteralSourcePathsAndEnvironmentTemplates(t *testing.T) {
	workspace := t.TempDir()
	exportFixtureGit(t, workspace, "init", "-q")
	for _, name := range []string{"[example].txt", ".env.example"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	exportFixtureGit(t, workspace, "add", "--", "[example].txt", ".env.example")
	exportFixtureGit(t, workspace, "commit", "-qm", "fixture")
	if err := os.WriteFile(filepath.Join(workspace, "[example].txt"), []byte("literal source change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".env.example"), []byte("SYNTHETIC_SETTING=example"), 0600); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(newStore(t), workspace)
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/result", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "literal source change") || !strings.Contains(w.Body.String(), "SYNTHETIC_SETTING=example") {
		t.Fatalf("ordinary result: %d %s", w.Code, w.Body.String())
	}
}
