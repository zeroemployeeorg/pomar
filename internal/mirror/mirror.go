// Package mirror keeps host-side bare mirrors of source repositories under
// the data root, resolves refs to commit SHAs at admission, and produces the
// per-attempt source snapshot a guest sees. Guests never see the mirror
// itself, a remote, or any credential; fetching uses the host's git
// configuration.
package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// Dir is the venue structure directory that holds mirrors.
const Dir = "mirrors"

var (
	validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	fullSHA   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ErrNoMirror is a mirror that is not there: never synced, or not ledgered.
// ErrUnknownCommit is a commit the mirror does not have, or does not reach
// the way a start requires. The manager names them apart when it refuses a
// start (the elders' ruling of 2026-09-27 17:21Z §4).
var (
	ErrNoMirror      = errors.New("mirror: no such mirror")
	ErrUnknownCommit = errors.New("mirror: unknown commit")
)

// Mirrors operates on the mirrors in one data root.
type Mirrors struct {
	Venue *venue.Venue
	// Git is the git binary; empty means "git" on PATH.
	Git string
	// Env is added to git's environment (tests use it to isolate config).
	Env []string
}

func (m *Mirrors) git(ctx context.Context, dir string, args ...string) (string, error) {
	bin := m.Git
	if bin == "" {
		bin = "git"
	}
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), m.Env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mirror: git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func rel(name string) string { return filepath.Join(Dir, name+".git") }

// Path returns a mirror's absolute path.
func (m *Mirrors) Path(name string) string { return filepath.Join(m.Venue.Root(), rel(name)) }

func id(name string) string { return "mirror-" + name }

// Sync creates the mirror on first use (ledgered as a cache before it
// exists) and fetches every ref, pruning deleted ones, afterwards.
func (m *Mirrors) Sync(ctx context.Context, name, url string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("mirror: invalid name %q", name)
	}
	v := m.Venue
	if err := v.Init(); err != nil {
		return err
	}
	if v.IsOpen(venue.KindVolume, id(name)) {
		if _, err := os.Stat(m.Path(name)); err == nil {
			_, err := m.git(ctx, m.Path(name), "remote", "update", "--prune")
			return err
		}
		return fmt.Errorf("mirror: %s is ledgered but missing on disk; tear it down first", name)
	}
	if err := v.CheckHeavy(venue.DefaultMaxFillPercent); err != nil {
		return err
	}
	if err := v.Intent(venue.KindVolume, venue.ClassCache, id(name), rel(name), "source mirror"); err != nil {
		return err
	}
	if _, err := m.git(ctx, "", "clone", "--mirror", "--quiet", url, m.Path(name)); err != nil {
		v.Failed(venue.KindVolume, id(name), err.Error())
		v.Teardown(venue.KindVolume, id(name))
		return err
	}
	return v.Created(venue.KindVolume, id(name))
}

// Remote returns the URL the mirror fetches from, for the result document.
func (m *Mirrors) Remote(ctx context.Context, name string) (string, error) {
	if !m.Venue.IsOpen(venue.KindVolume, id(name)) {
		return "", fmt.Errorf("mirror: no mirror %q", name)
	}
	return m.git(ctx, m.Path(name), "config", "--get", "remote.origin.url")
}

// Resolve pins ref to a full commit SHA in the mirror. A full SHA must exist
// in the mirror; anything else is resolved as a ref.
func (m *Mirrors) Resolve(ctx context.Context, name, ref string) (string, error) {
	if !m.Venue.IsOpen(venue.KindVolume, id(name)) {
		return "", fmt.Errorf("%w %q; sync it first", ErrNoMirror, name)
	}
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("mirror: invalid ref %q", ref)
	}
	sha, err := m.git(ctx, m.Path(name), "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %s has no commit %q", ErrUnknownCommit, name, ref)
	}
	if !fullSHA.MatchString(sha) {
		return "", fmt.Errorf("mirror: unexpected rev-parse output %q", sha)
	}
	return sha, nil
}

// Snapshot writes the tree of commit sha as a tar archive to dst, which must
// not exist. Only that snapshot, never the mirror, reaches a guest.
func (m *Mirrors) Snapshot(ctx context.Context, name, sha, dst string) error {
	if !fullSHA.MatchString(sha) {
		return fmt.Errorf("mirror: snapshot needs a full commit SHA, got %q", sha)
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("mirror: %s already exists", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := m.git(ctx, m.Path(name), "archive", "--format=tar", "-o", dst, sha)
	return err
}

// validBranch is a branch name as bundles take it: no leading dash, no dot
// segments, nothing git would read as a revision expression.
var validBranch = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// Bundle writes a git bundle to dst, which must not exist, holding branch
// (the history of the commit an attempt was pinned to) and base (the ref
// the job compares against, for example main), for a guest that needs a
// repository rather than a tree. It refuses when sha is not in the bundled
// branch's history, so the guest can always check out exactly the pinned
// commit. The guest fetches it with no remote configured.
func (m *Mirrors) Bundle(ctx context.Context, name, sha, branch, base, dst string) error {
	if !fullSHA.MatchString(sha) {
		return fmt.Errorf("mirror: bundle needs a full commit SHA, got %q", sha)
	}
	for _, b := range []string{branch, base} {
		if !validBranch.MatchString(b) || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
			return fmt.Errorf("mirror: invalid branch %q", b)
		}
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("mirror: %s already exists", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := m.Path(name)
	if _, err := m.git(ctx, dir, "merge-base", "--is-ancestor", sha, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("mirror: %s is not in the history of %s: %w", sha, branch, err)
	}
	refs := []string{"refs/heads/" + branch}
	if base != branch {
		refs = append(refs, "refs/heads/"+base)
	}
	_, err := m.git(ctx, dir, append([]string{"bundle", "create", "-q", dst}, refs...)...)
	return err
}

// ResolveAt checks that sha, a full commit SHA the caller names, is a commit of
// mirror name and in the history of its branch, and returns it. A start that
// names its exact commit gets that commit, never whatever the branch has
// moved to since (the elders' ruling r30 §4.2).
func (m *Mirrors) ResolveAt(ctx context.Context, name, branch, sha string) (string, error) {
	if !fullSHA.MatchString(sha) {
		return "", fmt.Errorf("mirror: %q is not a full commit SHA", sha)
	}
	if !validBranch.MatchString(branch) || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") {
		return "", fmt.Errorf("mirror: invalid branch %q", branch)
	}
	got, err := m.Resolve(ctx, name, sha)
	if err != nil {
		return "", err
	}
	if got != sha {
		return "", fmt.Errorf("mirror: %s resolved to %s", sha, got)
	}
	if _, err := m.git(ctx, m.Path(name), "merge-base", "--is-ancestor", sha, "refs/heads/"+branch); err != nil {
		return "", fmt.Errorf("%w: %s is not in the history of %s's %s", ErrUnknownCommit, sha, name, branch)
	}
	return sha, nil
}

// Commit checks that sha, a full commit SHA, is a commit the mirror has: a
// branch's, or a pull request's head (the mirror fetches refs/pull/*).
func (m *Mirrors) Commit(ctx context.Context, name, sha string) error {
	if !fullSHA.MatchString(sha) {
		return fmt.Errorf("mirror: %q is not a full commit SHA", sha)
	}
	got, err := m.Resolve(ctx, name, sha)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("mirror: %s resolved to %s", sha, got)
	}
	return nil
}

// BundleAt writes a git bundle of exactly two commits, head and base, and
// the history they reach, as refs/heads/head and refs/heads/base: a guest
// that fetches it can check out head and run `git diff base...HEAD`, the
// merge base included. The bundle is made in a throwaway bare repository
// that borrows the mirror's objects (git alternates), so the shared mirror
// is never written. dst must not exist (the elders' ruling of 2026-09-27
// §2.4).
func (m *Mirrors) BundleAt(ctx context.Context, name, head, base, dst string) error {
	for _, sha := range []string{head, base} {
		if err := m.Commit(ctx, name, sha); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("mirror: %s already exists", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), ".bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := m.git(ctx, "", "init", "--quiet", "--bare", tmp); err != nil {
		return err
	}
	objects := filepath.Join(m.Path(name), "objects")
	if err := os.WriteFile(filepath.Join(tmp, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return err
	}
	for ref, sha := range map[string]string{"refs/heads/head": head, "refs/heads/base": base} {
		if _, err := m.git(ctx, tmp, "update-ref", ref, sha); err != nil {
			return err
		}
	}
	_, err = m.git(ctx, tmp, "bundle", "create", "-q", dst, "refs/heads/head", "refs/heads/base")
	return err
}

// MaxReachRefs caps how many of the refs that reach a head are recorded; the
// count records how many there were.
const MaxReachRefs = 8

// Reach is how a start's commits were found in the mirror when it was
// admitted: the refs that reached them, each with its tip at that moment. A
// ref that moves later does not change what was accepted.
type Reach struct {
	Head         string   `json:"head"`
	HeadRefs     []string `json:"head_refs"`      // "REF TIP", sorted, at most MaxReachRefs
	HeadRefCount int      `json:"head_ref_count"` // every ref that reached the head
	Base         string   `json:"base,omitempty"`
	BaseRef      string   `json:"base_ref,omitempty"` // "refs/heads/BRANCH TIP"
}

// Reachable checks that head is a commit of mirror name reachable from a ref
// the mirror syncs (a branch, refs/heads/*, or a pull request's,
// refs/pull/*), and, when base is set, that base is in the history of
// refs/heads/baseBranch. It is judged now, so the caller calls it right after
// the sync that admits the attempt; the Reach it returns records the refs
// and their tips as they were (the elders' ruling of 2026-09-27 17:21Z §4).
func (m *Mirrors) Reachable(ctx context.Context, name, head, base, baseBranch string) (Reach, error) {
	if !m.Venue.IsOpen(venue.KindVolume, id(name)) {
		return Reach{}, fmt.Errorf("%w %q; sync it first", ErrNoMirror, name)
	}
	if err := m.Commit(ctx, name, head); err != nil {
		return Reach{}, err
	}
	out, err := m.git(ctx, m.Path(name), "for-each-ref", "--contains", head,
		"--format=%(refname) %(objectname)", "refs/heads", "refs/pull")
	if err != nil {
		return Reach{}, err
	}
	var refs []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			refs = append(refs, l)
		}
	}
	if len(refs) == 0 {
		return Reach{}, fmt.Errorf("%w: %s is in %s but no branch or pull request reaches it", ErrUnknownCommit, head, name)
	}
	sort.Strings(refs)
	r := Reach{Head: head, HeadRefs: refs[:min(len(refs), MaxReachRefs)], HeadRefCount: len(refs)}
	if base == "" {
		return r, nil
	}
	if !validBranch.MatchString(baseBranch) || strings.HasPrefix(baseBranch, "-") || strings.Contains(baseBranch, "..") {
		return Reach{}, fmt.Errorf("mirror: invalid branch %q", baseBranch)
	}
	if err := m.Commit(ctx, name, base); err != nil {
		return Reach{}, err
	}
	ref := "refs/heads/" + baseBranch
	tip, err := m.git(ctx, m.Path(name), "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return Reach{}, fmt.Errorf("%w: %s has no branch %s", ErrUnknownCommit, name, baseBranch)
	}
	if _, err := m.git(ctx, m.Path(name), "merge-base", "--is-ancestor", base, ref); err != nil {
		return Reach{}, fmt.Errorf("%w: the base %s is not in the history of %s's %s", ErrUnknownCommit, base, name, baseBranch)
	}
	r.Base, r.BaseRef = base, ref+" "+tip
	return r, nil
}
