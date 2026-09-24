package mirror

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// isolatedEnv keeps the developer's git configuration out of the tests.
func isolatedEnv(t *testing.T) []string {
	home := t.TempDir()
	return []string{
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}
}

func run(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstream makes a synthetic repository with one commit and returns its
// path and the commit SHA.
func upstream(t *testing.T, env []string) (string, string) {
	dir := t.TempDir()
	run(t, env, dir, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, dir, "add", "hello.txt")
	run(t, env, dir, "commit", "--quiet", "-m", "one")
	return dir, run(t, env, dir, "rev-parse", "HEAD")
}

func setup(t *testing.T) (*Mirrors, []string, string, string) {
	env := isolatedEnv(t)
	up, sha := upstream(t, env)
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Mirrors{Venue: v, Env: env}, env, up, sha
}

func TestSyncResolveSnapshot(t *testing.T) {
	m, env, up, sha1 := setup(t)
	ctx := context.Background()
	if err := m.Sync(ctx, "demo", "file://"+up); err != nil {
		t.Fatal(err)
	}
	if !m.Venue.IsOpen(venue.KindVolume, "mirror-demo") {
		t.Fatal("mirror not ledgered")
	}
	got, err := m.Resolve(ctx, "demo", "main")
	if err != nil || got != sha1 {
		t.Fatalf("Resolve(main) = %q, %v; want %s", got, err, sha1)
	}

	// A new upstream commit is invisible until the next sync, then resolves.
	if err := os.WriteFile(filepath.Join(up, "hello.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, up, "commit", "--quiet", "-am", "two")
	sha2 := run(t, env, up, "rev-parse", "HEAD")
	if got, _ := m.Resolve(ctx, "demo", "main"); got != sha1 {
		t.Fatalf("resolved %s before sync, want %s", got, sha1)
	}
	if err := m.Sync(ctx, "demo", "file://"+up); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Resolve(ctx, "demo", "main"); got != sha2 {
		t.Fatalf("after sync resolved %s, want %s", got, sha2)
	}

	// The snapshot of the first commit holds the first content only.
	dst := filepath.Join(t.TempDir(), "source.tar")
	if err := m.Snapshot(ctx, "demo", sha1, dst); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var content string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "hello.txt" {
			b, _ := io.ReadAll(tr)
			content = string(b)
		}
	}
	if content != "v1\n" {
		t.Fatalf("snapshot content = %q, want v1", content)
	}
	if err := m.Snapshot(ctx, "demo", sha1, dst); err == nil {
		t.Fatal("snapshot over an existing file accepted")
	}
}

func TestRejects(t *testing.T) {
	m, _, up, _ := setup(t)
	ctx := context.Background()
	if err := m.Sync(ctx, "Bad/Name", "file://"+up); err == nil {
		t.Error("invalid name accepted")
	}
	if _, err := m.Resolve(ctx, "missing", "main"); err == nil {
		t.Error("resolve on an unsynced mirror accepted")
	}
	if err := m.Sync(ctx, "demo", "file://"+up); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", "--all", "no-such-branch", strings.Repeat("0", 40)} {
		if _, err := m.Resolve(ctx, "demo", ref); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
	}
	if err := m.Snapshot(ctx, "demo", "main", filepath.Join(t.TempDir(), "x.tar")); err == nil {
		t.Error("snapshot of a non-SHA accepted")
	}
}

func TestFailedCloneLeavesNothing(t *testing.T) {
	m, _, _, _ := setup(t)
	if err := m.Sync(context.Background(), "gone", "file:///nonexistent-repo-path"); err == nil {
		t.Fatal("clone of a missing repo succeeded")
	}
	if m.Venue.IsOpen(venue.KindVolume, "mirror-gone") {
		t.Fatal("failed mirror left open in the ledger")
	}
	if _, err := os.Stat(m.Path("gone")); err == nil {
		t.Fatal("failed mirror left a directory")
	}
	un, err := m.Venue.Unaccounted()
	if err != nil || len(un) != 0 {
		t.Fatalf("unaccounted after failed clone: %v, %v", un, err)
	}
}
