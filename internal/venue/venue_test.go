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
	if err := v.Intent(KindVolume, "vol1", "volumes/vol1", "test"); err != nil {
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
	if err := v3.Intent(KindVolume, "vol2", "volumes/vol2", ""); err != nil {
		t.Fatal(err)
	}
	if v3.seq != 4 {
		t.Fatalf("seq = %d, want 4", v3.seq)
	}
}

func TestIntentRejectsPathsOutsideRoot(t *testing.T) {
	v := openTemp(t)
	for _, p := range []string{"..", "../x", "a/../../x", ".", ledgerName, "/abs"} {
		if err := v.Intent(KindDownload, "d-"+p, p, ""); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
}

func TestDuplicateOpenIntentRefused(t *testing.T) {
	v := openTemp(t)
	if err := v.Intent(KindVM, "a1", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Intent(KindVM, "a1", "", ""); err == nil {
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
	if err := v.Intent(KindDownload, "d1", "link/keep", ""); err != nil {
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
	if err := v.Intent(KindVM, "a2", "vms/a2", ""); err != nil {
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
		if err := v.Intent(KindVolume, "same", p, ""); err != nil {
			t.Fatal(err)
		}
		if err := v.Teardown(KindVolume, "same"); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Intent(KindVolume, "same", "c/three", ""); err != nil {
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
