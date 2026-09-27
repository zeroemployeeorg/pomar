package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/mirror"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// A source the mirror cannot place is refused by name before capacity is
// claimed, apart from a mirror that cannot be synced; a placeable source on
// a full host meets admission as before (the elders' ruling of 2026-09-27
// 17:21Z §4).
func TestSourceIsSettledBeforeAdmission(t *testing.T) {
	env := gitEnv(t)
	up := t.TempDir()
	git(t, env, up, "init", "--quiet", "--initial-branch=main")
	first := commit(t, env, up, "one")
	git(t, env, up, "checkout", "--quiet", "--detach")
	pr := commit(t, env, up, "pr")
	git(t, env, up, "update-ref", "refs/pull/3/head", pr)
	git(t, env, up, "checkout", "--quiet", "main")

	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	mirrors := &mirror.Mirrors{Venue: v, Env: env}
	self, _ := os.Executable()
	// Room for one CI attempt (2 vCPUs), taken below by a live one.
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 2, MemoryBytes: 8 * capacity.GiB}, Mirrors: mirrors,
		MirrorURLs: map[string]string{"up": "file://" + up, "gone": "file://" + filepath.Join(up, "no-such-repo")}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.mu.Lock()
	m.t.entries["busy"] = &Entry{Attempt: "busy", State: StateRunning, Class: capacity.CI, Created: time.Now()}
	m.mu.Unlock()

	reason := func(err error) string {
		var ref *capacity.Refusal
		if errors.As(err, &ref) {
			return ref.Reason
		}
		return "none: " + err.Error()
	}
	// The first start syncs the mirror; then a commit it has, but that nothing
	// reaches, is made in the mirror itself.
	if _, err := m.Start("s0", []string{"true"}, &Source{Mirror: "up", Ref: "main"}); reason(err) != capacity.ReasonCPU {
		t.Fatalf("a known ref on a full host: %v, want cpu-slots", err)
	}
	tree := git(t, env, mirrors.Path("up"), "rev-parse", first+"^{tree}")
	dangling := git(t, env, mirrors.Path("up"), "commit-tree", tree, "-p", first, "-m", "dangling")

	for name, tc := range map[string]struct {
		src  *Source
		want string
	}{
		"a mirror that cannot be synced":  {&Source{Mirror: "gone", Ref: "main"}, capacity.ReasonMirrorUnavailable},
		"a ref the mirror lacks":          {&Source{Mirror: "up", Ref: "no-such-branch"}, capacity.ReasonCommitUnknown},
		"a SHA the mirror lacks":          {&Source{Mirror: "up", Ref: "main", SHA: strings.Repeat("0", 40)}, capacity.ReasonCommitUnknown},
		"a raw SHA as ref, unreached":     {&Source{Mirror: "up", Ref: dangling}, capacity.ReasonCommitUnknown},
		"a head nothing reaches":          {&Source{Mirror: "up", SHA: dangling, BaseSHA: first, Git: true}, capacity.ReasonCommitUnknown},
		"a base not in main's history":    {&Source{Mirror: "up", SHA: pr, BaseSHA: pr, Git: true}, capacity.ReasonCommitUnknown},
		"a pull request's head, reached":  {&Source{Mirror: "up", SHA: pr, BaseSHA: first, Git: true}, capacity.ReasonCPU},
		"a SHA in main's history reached": {&Source{Mirror: "up", Ref: "main", SHA: first}, capacity.ReasonCPU},
	} {
		if _, err := m.Start("s1", []string{"true"}, tc.src); reason(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("%d entries; a refused start created one", n)
	}
}

// source.json records the base SHA a head-and-base start named, not a
// branch, and the reach at admission.
func TestSourceJSONRecordsTheBaseSHA(t *testing.T) {
	sha, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	reach := &mirror.Reach{Head: sha, HeadRefs: []string{"refs/pull/3/head " + sha}, HeadRefCount: 1, Base: base, BaseRef: "refs/heads/main " + base}
	got := sourceJSON(&Source{Mirror: "up", SHA: sha, BaseSHA: base, Git: true}, sha, reach)
	if got["base_sha"] != base || got["reach"] != reach {
		t.Fatalf("head and base: %v", got)
	}
	if _, ok := got["base"]; ok {
		t.Fatalf("head and base: a branch was recorded as the base: %v", got)
	}
	got = sourceJSON(&Source{Mirror: "up", Ref: "feature", Git: true}, sha, nil)
	if got["base"] != "main" || got["base_sha"] != nil {
		t.Fatalf("a branch with git: %v", got)
	}
	got = sourceJSON(&Source{Mirror: "up", Ref: "main"}, sha, nil)
	if _, ok := got["base"]; ok {
		t.Fatalf("a tree: a base was recorded: %v", got)
	}
}
