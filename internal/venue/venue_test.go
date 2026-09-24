package venue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) *Venue {
	t.Helper()
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenRejectsBadRoots(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("empty root accepted")
	}
	if _, err := Open("relative/dir"); err == nil {
		t.Error("relative root accepted")
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Error("file accepted as root")
	}
}

func TestLifecycleAndReplay(t *testing.T) {
	v := openTemp(t)
	if err := v.Intent(KindVolume, ClassAttempt, "vol1", "volumes/vol1", "test"); err != nil {
		t.Fatal(err)
	}
	// The intent is on disk before the object exists.
	b, err := os.ReadFile(filepath.Join(v.Root(), ledgerName))
	if err != nil || !strings.Contains(string(b), `"op":"intent"`) {
		t.Fatalf("intent not on disk: %q, %v", b, err)
	}
	dir := filepath.Join(v.Root(), "volumes", "vol1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.Created(KindVolume, "vol1"); err != nil {
		t.Fatal(err)
	}

	// A second process sees the same open object.
	v2, err := Open(v.Root())
	if err != nil {
		t.Fatal(err)
	}
	open := v2.OpenObjects()
	if len(open) != 1 || open[0].ID != "vol1" || open[0].Last != OpCreated {
		t.Fatalf("replayed open objects = %+v", open)
	}

	if err := v2.Teardown(KindVolume, "vol1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data not removed: %v", err)
	}
	v3, err := Open(v.Root())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(v3.OpenObjects()); n != 0 {
		t.Fatalf("open after teardown = %d", n)
	}
	// Sequence numbers continue across processes.
	if err := v3.Intent(KindVolume, ClassAttempt, "vol2", "volumes/vol2", ""); err != nil {
		t.Fatal(err)
	}
	if v3.seq != 4 {
		t.Fatalf("seq = %d, want 4", v3.seq)
	}
}

func TestIntentRejectsPathsOutsideRoot(t *testing.T) {
	v := openTemp(t)
	for _, p := range []string{"..", "../x", "a/../../x", ".", ledgerName, "/abs"} {
		if err := v.Intent(KindDownload, ClassAttempt, "d-"+p, p, ""); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
}

func TestDuplicateOpenIntentRefused(t *testing.T) {
	v := openTemp(t)
	if err := v.Intent(KindVM, ClassAttempt, "a1", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Intent(KindVM, ClassAttempt, "a1", "", ""); err == nil {
		t.Fatal("second intent for an open object accepted")
	}
}

func TestTeardownOnlyLedgeredObjects(t *testing.T) {
	v := openTemp(t)
	stray := filepath.Join(v.Root(), "stray")
	if err := os.WriteFile(stray, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.Teardown(KindVolume, "stray"); err == nil {
		t.Fatal("teardown of an unledgered object accepted")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("unledgered file touched: %v", err)
	}
}

func TestTeardownRefusesSymlinkedParent(t *testing.T) {
	v := openTemp(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep")
	if err := os.WriteFile(victim, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(v.Root(), "link")); err != nil {
		t.Fatal(err)
	}
	if err := v.Intent(KindDownload, ClassAttempt, "d1", "link/keep", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Teardown(KindDownload, "d1"); err == nil {
		t.Fatal("teardown through a symlinked parent accepted")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("file outside the root was removed: %v", err)
	}
}

func TestFailedStaysOpenUntilTeardown(t *testing.T) {
	v := openTemp(t)
	if err := v.Intent(KindVM, ClassAttempt, "a2", "vms/a2", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Failed(KindVM, "a2", "boot failed"); err != nil {
		t.Fatal(err)
	}
	if n := len(v.OpenObjects()); n != 1 {
		t.Fatalf("open = %d, want 1", n)
	}
	if err := v.Teardown(KindVM, "a2"); err != nil {
		t.Fatal(err)
	}
	if n := len(v.OpenObjects()); n != 0 {
		t.Fatalf("open = %d, want 0", n)
	}
}

func TestCheckHeavy(t *testing.T) {
	v := openTemp(t)
	pct, err := v.Fill()
	if err != nil || pct < 0 || pct > 100 {
		t.Fatalf("Fill() = %d, %v", pct, err)
	}
	if err := v.CheckHeavy(100); err != nil {
		t.Fatalf("CheckHeavy(100) = %v", err)
	}
	if pct > 0 {
		if err := v.CheckHeavy(pct - 1); !errors.Is(err, ErrDiskFull) {
			t.Fatalf("CheckHeavy(%d) = %v, want ErrDiskFull", pct-1, err)
		}
	}
}

func TestReusedIDTakesNewPath(t *testing.T) {
	v := openTemp(t)
	for _, p := range []string{"a/one", "b/two"} {
		if err := v.Intent(KindVolume, ClassAttempt, "same", p, ""); err != nil {
			t.Fatal(err)
		}
		if err := v.Teardown(KindVolume, "same"); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Intent(KindVolume, ClassAttempt, "same", "c/three", ""); err != nil {
		t.Fatal(err)
	}
	v2, err := Open(v.Root())
	if err != nil {
		t.Fatal(err)
	}
	open := v2.OpenObjects()
	if len(open) != 1 || open[0].Path != "c/three" {
		t.Fatalf("open = %+v, want path c/three", open)
	}
}

func mkfile(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIntentRequiresClass(t *testing.T) {
	v := openTemp(t)
	if err := v.Intent(KindVM, "", "x", "", ""); err == nil {
		t.Fatal("empty class accepted")
	}
	if err := v.Intent(KindVM, "structure", "x", "", ""); err == nil {
		t.Fatal("unknown class accepted")
	}
}

func TestInitIsIdempotentAndAccounted(t *testing.T) {
	v := openTemp(t)
	for i := 0; i < 2; i++ {
		if err := v.Init(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := v.Unaccounted()
	if err != nil || len(got) != 0 {
		t.Fatalf("fresh root unaccounted = %v, %v", got, err)
	}
}

func TestUnaccounted(t *testing.T) {
	v := openTemp(t)
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	root := v.Root()
	// A ledgered cache and a ledgered attempt nested two levels down.
	if err := v.Intent(KindImage, ClassCache, "store", "store", ""); err != nil {
		t.Fatal(err)
	}
	mkfile(t, root, "store/content/blob")
	if err := v.Intent(KindVM, ClassAttempt, "a1", "vms/a1", ""); err != nil {
		t.Fatal(err)
	}
	mkfile(t, root, "vms/a1/rootfs.ext4")
	// Strays: at the top, inside structure, beside an object on its path,
	// and the leftover of an object already torn down.
	mkfile(t, root, "stray.txt")
	mkfile(t, root, "downloads/unledgered.tar")
	mkfile(t, root, "vms/other/leftover")
	if err := v.Intent(KindDownload, ClassAttempt, "d1", "downloads/d1", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Teardown(KindDownload, "d1"); err != nil {
		t.Fatal(err)
	}
	mkfile(t, root, "downloads/d1")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := v.Unaccounted()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"downloads/d1", "downloads/unledgered.tar", "link", "stray.txt", "vms/other"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unaccounted = %v, want %v", got, want)
	}
	// Reporting never deletes.
	if _, err := os.Stat(filepath.Join(root, "stray.txt")); err != nil {
		t.Fatalf("stray removed: %v", err)
	}
}

func TestClassify(t *testing.T) {
	root := t.TempDir()
	// A ledger line written before classes existed.
	line := `{"seq":1,"time":"2026-01-01T00:00:00Z","op":"intent","kind":"image","id":"k","path":"kernels/k"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, ledgerName), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if o := v.OpenObjects(); len(o) != 1 || o[0].Class != "" {
		t.Fatalf("legacy object = %+v", o)
	}
	if err := v.Classify(KindImage, "k", ClassCache); err != nil {
		t.Fatal(err)
	}
	if err := v.Classify(KindImage, "k", ClassAttempt); err == nil {
		t.Fatal("reclassifying accepted")
	}
	v2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	o := v2.OpenObjects()
	if len(o) != 1 || o[0].Class != ClassCache || o[0].Last != OpIntent {
		t.Fatalf("after classify and replay = %+v", o)
	}
}

func TestRootIsNeverAnObject(t *testing.T) {
	v := openTemp(t)
	for _, p := range []string{".", "./", "a/.."} {
		if err := v.Intent(KindVolume, ClassCache, "root"+p, p, ""); err == nil {
			t.Errorf("root path %q accepted", p)
		}
	}
}

func TestCreatedTimeAndTeardownNote(t *testing.T) {
	v := openTemp(t)
	if err := v.Intent(KindVolume, ClassCache, "c1", "c1", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Created(KindVolume, "c1"); err != nil {
		t.Fatal(err)
	}
	v2, err := Open(v.Root())
	if err != nil {
		t.Fatal(err)
	}
	objs := v2.OpenObjects()
	if len(objs) != 1 || objs[0].Created.IsZero() {
		t.Fatalf("replayed objects = %+v, want one with a creation time", objs)
	}
	if err := v2.TeardownNote(KindVolume, "c1", "evicted: test"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(v.Root(), ledgerName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"op":"removed","kind":"volume","class":"cache","id":"c1","path":"c1","note":"evicted: test"`) {
		t.Fatalf("ledger lacks the noted removal:\n%s", b)
	}
}

func TestSameContainer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"/dev/disk3s5", "/dev/disk3s1s1", true},
		{"/dev/disk3s5", "/dev/disk6s1", false},
		{"/dev/disk13s1", "/dev/disk1s1", false},
		{"map auto_home", "/dev/disk3s5", true}, // unparsable counts as shared
		{"/dev/disk", "/dev/disk3s5", true},
	} {
		if got := SameContainer(tc.a, tc.b); got != tc.want {
			t.Errorf("SameContainer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSpaceReadsTheFilesystem(t *testing.T) {
	sp, err := openTemp(t).Space()
	if err != nil {
		t.Fatal(err)
	}
	if sp.Avail <= 0 || sp.Used <= 0 || !strings.HasPrefix(sp.Device, "/dev/") {
		t.Fatalf("Space() = %+v", sp)
	}
}
