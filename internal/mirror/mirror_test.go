package mirror

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
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

// A bundle holds the branch and base; fetched into a fresh repository with
// no remote, it checks out exactly the pinned commit and has origin/main.
func TestBundle(t *testing.T) {
	m, env, up, sha1 := setup(t)
	ctx := context.Background()
	if err := m.Sync(ctx, "demo", up); err != nil {
		t.Fatal(err)
	}
	run(t, env, up, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(up, "hello.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, up, "commit", "--quiet", "-am", "two")
	sha2 := run(t, env, up, "rev-parse", "HEAD")
	if err := m.Sync(ctx, "demo", up); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "s.bundle")
	if err := m.Bundle(ctx, "demo", sha2, "feature", "main", dst); err != nil {
		t.Fatal(err)
	}
	heads := run(t, env, "", "bundle", "list-heads", dst)
	if !strings.Contains(heads, sha2+" refs/heads/feature") || !strings.Contains(heads, sha1+" refs/heads/main") {
		t.Fatalf("list-heads:\n%s", heads)
	}
	work := t.TempDir()
	run(t, env, work, "init", "--quiet")
	run(t, env, work, "fetch", "--quiet", dst, "refs/heads/*:refs/remotes/origin/*")
	run(t, env, work, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", sha2)
	if got := run(t, env, work, "rev-parse", "HEAD"); got != sha2 {
		t.Fatalf("HEAD %s, want %s", got, sha2)
	}
	if got := run(t, env, work, "rev-parse", "origin/main"); got != sha1 {
		t.Fatalf("origin/main %s, want %s", got, sha1)
	}
	if remotes := run(t, env, work, "remote"); remotes != "" {
		t.Fatalf("the guest repository has remotes: %q", remotes)
	}
	// A commit outside the branch's history is refused, and so is a
	// branch that looks like an option or a revision range.
	if err := m.Bundle(ctx, "demo", sha2, "main", "main", filepath.Join(t.TempDir(), "x.bundle")); err == nil {
		t.Error("a SHA outside the branch was bundled")
	}
	for _, bad := range []string{"--all", "main..feature", "a b"} {
		if err := m.Bundle(ctx, "demo", sha2, bad, "main", filepath.Join(t.TempDir(), "y.bundle")); err == nil {
			t.Errorf("branch %q accepted", bad)
		}
	}
	if err := m.Bundle(ctx, "demo", sha2, "feature", "main", dst); err == nil {
		t.Error("an existing destination was overwritten")
	}
}

// ResolveAt returns exactly the named commit when it is in the branch's
// history, and refuses anything else.
func TestResolveAt(t *testing.T) {
	m, env, up, sha1 := setup(t)
	ctx := context.Background()
	run(t, env, up, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(up, "hello.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, up, "commit", "--quiet", "-am", "two")
	sha2 := run(t, env, up, "rev-parse", "HEAD")
	if err := m.Sync(ctx, "demo", up); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ branch, sha string }{{"main", sha1}, {"feature", sha1}, {"feature", sha2}} {
		if got, err := m.ResolveAt(ctx, "demo", tc.branch, tc.sha); err != nil || got != tc.sha {
			t.Errorf("ResolveAt(%s, %s) = %q, %v", tc.branch, tc.sha[:8], got, err)
		}
	}
	for name, tc := range map[string]struct{ branch, sha string }{
		"a commit outside the branch": {"main", sha2},
		"an unknown commit":           {"main", strings.Repeat("0", 40)},
		"a short SHA":                 {"main", sha1[:12]},
		"a ref, not a SHA":            {"main", "main"},
		"a branch like an option":     {"--all", sha1},
		"a revision range":            {"main..feature", sha1},
		"a missing branch":            {"nope", sha1},
	} {
		if got, err := m.ResolveAt(ctx, "demo", tc.branch, tc.sha); err == nil {
			t.Errorf("%s: accepted, %q", name, got)
		}
	}
}

// A pull request's head, which is on no branch, and a base are bundled with
// the history between them; the guest-side checkout sees head and can diff
// against base; the mirror itself gains no ref.
func TestBundleAtPullRequestHead(t *testing.T) {
	m, env, up, sha1 := setup(t)
	ctx := context.Background()
	// A commit on no branch, reachable only as a pull request's head.
	run(t, env, up, "checkout", "--quiet", "--detach")
	if err := os.WriteFile(filepath.Join(up, "pr.txt"), []byte("from a fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, up, "add", "pr.txt")
	run(t, env, up, "commit", "--quiet", "-m", "pr")
	head := run(t, env, up, "rev-parse", "HEAD")
	run(t, env, up, "update-ref", "refs/pull/1/head", head)
	run(t, env, up, "checkout", "--quiet", "main")
	if err := m.Sync(ctx, "demo", up); err != nil {
		t.Fatal(err)
	}
	before := run(t, env, m.Path("demo"), "for-each-ref", "--format=%(refname)")

	dst := filepath.Join(t.TempDir(), "s.bundle")
	if err := m.BundleAt(ctx, "demo", head, sha1, dst); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	run(t, env, work, "init", "--quiet")
	run(t, env, work, "fetch", "--quiet", dst, "refs/heads/*:refs/remotes/origin/*")
	run(t, env, work, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", head)
	if got := run(t, env, work, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD %s, want %s", got, head)
	}
	if got := run(t, env, work, "diff", "--name-only", sha1+"...HEAD"); got != "pr.txt" {
		t.Fatalf("diff base...HEAD = %q, want pr.txt", got)
	}
	if after := run(t, env, m.Path("demo"), "for-each-ref", "--format=%(refname)"); after != before {
		t.Fatalf("the mirror's refs changed:\n%s\n---\n%s", before, after)
	}
	if ents, _ := os.ReadDir(filepath.Dir(dst)); len(ents) != 1 {
		t.Fatalf("the scratch repository was left behind: %v", ents)
	}
	for name, tc := range map[string]struct{ head, base string }{
		"an unknown head": {strings.Repeat("0", 40), sha1},
		"an unknown base": {head, strings.Repeat("0", 40)},
		"a short head":    {head[:12], sha1},
		"a ref as base":   {head, "main"},
	} {
		if err := m.BundleAt(ctx, "demo", tc.head, tc.base, filepath.Join(t.TempDir(), "x.bundle")); err == nil {
			t.Errorf("%s: bundled", name)
		}
	}
	if err := m.BundleAt(ctx, "demo", head, sha1, dst); err == nil {
		t.Error("an existing destination was overwritten")
	}
}

// Reachable judges, at the sync, that a head is reached by a synced branch or
// pull request and a base by its branch, and records the refs with their
// tips; it names a commit it cannot place apart from a missing mirror (the
// elders' ruling of 2026-09-27 17:21Z §4).
func TestReachable(t *testing.T) {
	m, env, up, sha1 := setup(t)
	ctx := context.Background()
	if _, err := m.Reachable(ctx, "demo", sha1, "", ""); !errors.Is(err, ErrNoMirror) {
		t.Fatalf("before any sync: %v, want ErrNoMirror", err)
	}
	// A pull request's head, on no branch.
	run(t, env, up, "checkout", "--quiet", "--detach")
	os.WriteFile(filepath.Join(up, "pr.txt"), []byte("pr\n"), 0o644)
	run(t, env, up, "add", "pr.txt")
	run(t, env, up, "commit", "--quiet", "-m", "pr")
	pr := run(t, env, up, "rev-parse", "HEAD")
	run(t, env, up, "update-ref", "refs/pull/7/head", pr)
	// A commit on a side branch only, not in main's history.
	run(t, env, up, "checkout", "--quiet", "-b", "side", "main")
	os.WriteFile(filepath.Join(up, "side.txt"), []byte("side\n"), 0o644)
	run(t, env, up, "add", "side.txt")
	run(t, env, up, "commit", "--quiet", "-m", "side")
	side := run(t, env, up, "rev-parse", "HEAD")
	run(t, env, up, "checkout", "--quiet", "main")
	if err := m.Sync(ctx, "demo", up); err != nil {
		t.Fatal(err)
	}
	// A commit the mirror has that nothing reaches: made in the mirror itself.
	tree := run(t, env, m.Path("demo"), "rev-parse", sha1+"^{tree}")
	dangling := run(t, env, m.Path("demo"), "commit-tree", tree, "-p", sha1, "-m", "dangling")

	r, err := m.Reachable(ctx, "demo", pr, sha1, "main")
	if err != nil {
		t.Fatal(err)
	}
	if r.Head != pr || r.HeadRefCount != 1 || r.HeadRefs[0] != "refs/pull/7/head "+pr || r.Base != sha1 || r.BaseRef != "refs/heads/main "+sha1 {
		t.Fatalf("reach = %+v", r)
	}
	if r, err := m.Reachable(ctx, "demo", sha1, "", ""); err != nil || r.HeadRefCount != 3 || r.Base != "" {
		// main, side and the pull request all reach the first commit.
		t.Fatalf("the first commit: %+v, %v", r, err)
	}
	for name, tc := range map[string]struct{ head, base, branch string }{
		"a commit the mirror lacks":           {strings.Repeat("0", 40), "", ""},
		"a commit nothing reaches":            {dangling, "", ""},
		"a base not in main's history":        {pr, side, "main"},
		"a base on a branch the mirror lacks": {pr, sha1, "nope"},
	} {
		if _, err := m.Reachable(ctx, "demo", tc.head, tc.base, tc.branch); !errors.Is(err, ErrUnknownCommit) {
			t.Errorf("%s: %v, want ErrUnknownCommit", name, err)
		}
	}
	if _, err := m.Reachable(ctx, "demo", pr, sha1, "-x"); err == nil || errors.Is(err, ErrUnknownCommit) {
		t.Errorf("an invalid branch name: %v", err)
	}
}

// Only MaxReachRefs refs are recorded, with the count of all of them.
func TestReachableCapsTheRefsItRecords(t *testing.T) {
	m, env, up, sha1 := setup(t)
	ctx := context.Background()
	for i := 0; i < MaxReachRefs+4; i++ {
		run(t, env, up, "update-ref", fmt.Sprintf("refs/pull/%d/head", i), sha1)
	}
	if err := m.Sync(ctx, "demo", up); err != nil {
		t.Fatal(err)
	}
	r, err := m.Reachable(ctx, "demo", sha1, "", "")
	if err != nil || len(r.HeadRefs) != MaxReachRefs || r.HeadRefCount != MaxReachRefs+5 {
		t.Fatalf("reach = %d refs of %d, %v", len(r.HeadRefs), r.HeadRefCount, err)
	}
}

// Show reads a file's exact bytes at a commit, from the mirror, and names a
// path the commit lacks as os.ErrNotExist.
func TestShow(t *testing.T) {
	m, env, up, _ := setup(t)
	ctx := context.Background()
	lock := "{\n  \"lockfileVersion\": 3\n}\n\n" // trailing newlines kept, byte for byte
	os.WriteFile(filepath.Join(up, "package-lock.json"), []byte(lock), 0o644)
	os.MkdirAll(filepath.Join(up, "sub"), 0o755)
	os.WriteFile(filepath.Join(up, "sub", "x.txt"), []byte("x"), 0o644)
	run(t, env, up, "add", "package-lock.json", "sub/x.txt")
	run(t, env, up, "commit", "--quiet", "-m", "lock")
	sha := run(t, env, up, "rev-parse", "HEAD")
	if err := m.Sync(ctx, "demo", up); err != nil {
		t.Fatal(err)
	}
	b, err := m.Show(ctx, "demo", sha, "package-lock.json")
	if err != nil || string(b) != lock {
		t.Fatalf("Show = %q, %v", b, err)
	}
	if _, err := m.Show(ctx, "demo", sha, "no-such-file"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing path: %v, want os.ErrNotExist", err)
	}
	if _, err := m.Show(ctx, "demo", sha, "sub"); err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Fatalf("a directory: %v", err)
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "-x", "a:b"} {
		if _, err := m.Show(ctx, "demo", sha, p); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("path %q: %v, want an invalid-path refusal", p, err)
		}
	}
	if _, err := m.Show(ctx, "demo", strings.Repeat("0", 40), "package-lock.json"); !errors.Is(err, ErrUnknownCommit) {
		t.Fatalf("an unknown commit: %v", err)
	}
}
