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
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// Dir is the venue structure directory that holds mirrors.
const Dir = "mirrors"

var (
	validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	fullSHA   = regexp.MustCompile(`^[0-9a-f]{40}$`)
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

// Resolve pins ref to a full commit SHA in the mirror. A full SHA must exist
// in the mirror; anything else is resolved as a ref.
func (m *Mirrors) Resolve(ctx context.Context, name, ref string) (string, error) {
	if !m.Venue.IsOpen(venue.KindVolume, id(name)) {
		return "", fmt.Errorf("mirror: no mirror %q; sync it first", name)
	}
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("mirror: invalid ref %q", ref)
	}
	sha, err := m.git(ctx, m.Path(name), "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("mirror: %s has no commit %q", name, ref)
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
