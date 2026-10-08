package manager

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/mirror"
	"github.com/zeroemployeeorg/pomar/internal/sign"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func TestClassCannotFetchAnotherProjectsMirror(t *testing.T) {
	env, up := gitEnv(t), t.TempDir()
	git(t, env, up, "init", "--quiet", "--initial-branch=main")
	sha := commit(t, env, up, "source")
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ms := &mirror.Mirrors{Venue: v, Env: env}
	ci, site := JobClass{Class: capacity.CI, SourceMirrors: []string{"up"}}, JobClass{Class: capacity.CI, SourceMirrors: []string{"site"}}
	site.Class.Name = "site"
	site.SourceRef, site.Command = "main", []string{"make", "verify"}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 8, MemoryBytes: 16 * capacity.GiB}, Classes: []JobClass{ci, site}, Mirrors: ms,
		MirrorURLs: map[string]string{"up": "file://" + up, "site": "file://" + up}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for i, tc := range []struct {
		class   string
		command []string
		src     *Source
		want    string
	}{
		{"ci", []string{"true"}, &Source{Mirror: "site", Ref: "main", SHA: sha}, "not allowed"},
		{"site", []string{"make", "verify"}, &Source{Mirror: "up", Ref: "main", SHA: sha}, "not allowed"},
		{"site", []string{"make", "build"}, &Source{Mirror: "site", Ref: "main", SHA: sha}, "fixed command"},
		{"site", []string{"make", "verify"}, &Source{Mirror: "site", Ref: "main"}, "explicit full source SHA"},
		{"site", []string{"make", "verify"}, &Source{Mirror: "site", Ref: "other", SHA: sha}, "requires ref"},
		{"site", []string{"make", "verify"}, nil, "requires ref"},
	} {
		_, err := m.StartIn(tc.class, "refuse-"+string(rune('a'+i)), tc.command, tc.src)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("case %d: %v", i, err)
		}
		if len(m.List()) != 0 {
			t.Fatal("refused workload created an attempt")
		}
	}
	for _, name := range []string{"up", "site"} {
		if _, err := os.Stat(ms.Path(name)); !os.IsNotExist(err) {
			t.Fatalf("refusal fetched %s: %v", name, err)
		}
	}
	// An allowed, explicitly pinned workload proceeds through the sync and
	// reaches the real entitlement boundary without launching a VM.
	_, err = m.StartIn("site", "allowed-1", []string{"make", "verify"}, &Source{Mirror: "site", Ref: "main", SHA: sha})
	if !errors.Is(err, sign.ErrMissing) {
		t.Fatalf("allowed workload: %v", err)
	}
	if got, err := ms.Resolve(context.Background(), "site", "main"); err != nil || got != sha {
		t.Fatalf("allowed sync: %s %v", got, err)
	}
}

func TestMultiClassSourceConfigurationFailsClosed(t *testing.T) {
	a, b := JobClass{Class: capacity.CI, SourceMirrors: []string{"up"}}, JobClass{Class: capacity.CI}
	b.Class.Name = "site"
	cfg := Config{Classes: []JobClass{a, b}, CtlSocket: "/run/control.sock", Mirrors: &mirror.Mirrors{}, MirrorURLs: map[string]string{"up": "file:///source", "site": "file:///site"}}
	if err := checkClassSources(cfg); err == nil {
		t.Fatal("unrestricted second source class accepted")
	}
	b.SourceMirrors = []string{"site"}
	cfg.Classes[1] = b
	if err := checkClassSources(cfg); err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{{"site", "site"}, {"unknown"}, {"../site"}} {
		cfg.Classes[1].SourceMirrors = names
		if err := checkClassSources(cfg); err == nil {
			t.Fatalf("bad list accepted: %v", names)
		}
	}
}

// The installed manager already supports this source route. A fully qualified
// required ref uses head-and-base export with the same main-reachable commit;
// no alternate base, helper spawn, service upgrade or live fetch is needed.
func TestQualifiedClassRefExportsSelfBaseWithoutChangingMirror(t *testing.T) {
	env, up := gitEnv(t), t.TempDir()
	git(t, env, up, "init", "--quiet", "--initial-branch=main")
	sha := commit(t, env, up, "source")
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ms := &mirror.Mirrors{Venue: v, Env: env}
	if err := ms.Sync(context.Background(), "site", up); err != nil {
		t.Fatal(err)
	}
	jc := JobClass{Class: capacity.CI, SourceMirrors: []string{"site"}, SourceRef: "refs/heads/main", Command: []string{"make", "verify"}}
	m := &Manager{cfg: Config{Venue: v, Mirrors: ms}}
	src := &Source{Mirror: "site", Ref: "refs/heads/main", SHA: sha, BaseSHA: sha, Git: true}
	if err := checkExactSource(src); err != nil {
		t.Fatal(err)
	}
	if err := jc.checkWorkload([]string{"make", "verify"}, src); err != nil {
		t.Fatal(err)
	}
	before := git(t, env, ms.Path("site"), "for-each-ref", "--format=%(refname) %(objectname)")
	got, reach, err := m.settleSource("self-base-fixture", src)
	if err != nil || got != sha || reach.BaseRef != "refs/heads/main "+sha {
		t.Fatalf("pinned main reachability: %s %+v %v", got, reach, err)
	}
	rec := t.TempDir()
	if err := m.writeSource(context.Background(), src, sha, rec); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	git(t, env, work, "init", "--quiet")
	git(t, env, work, "fetch", "--quiet", rec+"/source.bundle", "refs/heads/*:refs/remotes/origin/*")
	git(t, env, work, "checkout", "--quiet", "--detach", sha)
	if git(t, env, work, "rev-parse", "HEAD") != sha {
		t.Fatal("exported another commit")
	}
	if git(t, env, work, "remote") != "" {
		t.Fatal("guest received a remote")
	}
	if git(t, env, ms.Path("site"), "for-each-ref", "--format=%(refname) %(objectname)") != before {
		t.Fatal("export altered owner mirror")
	}
}
