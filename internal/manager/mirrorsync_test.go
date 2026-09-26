package manager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/mirror"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// gitEnv keeps the developer's git configuration out of the test.
func gitEnv(t *testing.T) []string {
	home := t.TempDir()
	return []string{
		"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}
}

func git(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, env []string, dir, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, env, dir, "add", "f.txt")
	git(t, env, dir, "commit", "--quiet", "-m", content)
	return git(t, env, dir, "rev-parse", "HEAD")
}

// With configured mirrors, a start names a mirror and a ref, the manager syncs
// the mirror from its configured URL, and a mirror it is not configured with
// is refused before anything is created.
func TestStartSyncsOnlyConfiguredMirrors(t *testing.T) {
	env := gitEnv(t)
	up := t.TempDir()
	git(t, env, up, "init", "--quiet", "--initial-branch=main")
	first := commit(t, env, up, "one")

	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	mirrors := &mirror.Mirrors{Venue: v, Env: env}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, Mirrors: mirrors,
		MirrorURLs: map[string]string{"up": "file://" + up}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()

	_, err = m.Start("a1", []string{"true"}, &Source{Mirror: "elsewhere", Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), "not one this manager syncs") {
		t.Fatalf("an unconfigured mirror: %v, want a refusal", err)
	}
	if _, err := os.Stat(mirrors.Path("elsewhere")); err == nil {
		t.Fatal("an unconfigured mirror was created")
	}

	// The start syncs the configured mirror. It then stops at the unentitled
	// test binary, after the sync, which is what this test observes.
	m.Start("a2", []string{"true"}, &Source{Mirror: "up", Ref: "main"})
	if got, err := mirrors.Resolve(ctx, "up", "main"); err != nil || got != first {
		t.Fatalf("after the first start, main is %q (%v), want %s", got, err, first)
	}
	second := commit(t, env, up, "two")
	m.Start("a3", []string{"true"}, &Source{Mirror: "up", Ref: "main"})
	if got, _ := mirrors.Resolve(ctx, "up", "main"); got != second {
		t.Fatalf("the next start did not fetch: main is %q, want %s", got, second)
	}
}
