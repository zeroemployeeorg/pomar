package seatdecl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func locations(t *testing.T) Locations {
	t.Helper()
	dir, err := os.MkdirTemp("", "seatloc")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return Locations{Dir: dir}
}

var decl = strings.Repeat("d", 64)

func TestFirstLocationNeedsNoEvidenceAndAMoveNeedsBoth(t *testing.T) {
	l := locations(t)
	if _, ok, err := l.Current("zeocreator"); ok || err != nil {
		t.Fatal(ok, err)
	}
	first, err := l.Move("zeocreator", 0, Location{Where: "mac-mini-naked", DeclarationSHA256: decl})
	if err != nil || first.Revision != 1 {
		t.Fatal(first, err)
	}
	for name, next := range map[string]Location{
		"no evidence":         {Where: "pomar:macbook", DeclarationSHA256: decl},
		"only the hand-over":  {Where: "pomar:macbook", DeclarationSHA256: decl, Handover: "rt-receipt-1"},
		"only the mechanism":  {Where: "pomar:macbook", DeclarationSHA256: decl, MechanismBinding: "env/session/inc"},
		"no declaration hash": {Where: "mac-mini-naked"},
		"an invalid place":    {Where: "Mac Mini", DeclarationSHA256: decl},
	} {
		if _, err := l.Move("zeocreator", 1, next); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	moved, err := l.Move("zeocreator", 1, Location{Where: "pomar:macbook", DeclarationSHA256: decl, Handover: "rt-receipt-1", MechanismBinding: "env/session/inc"})
	if err != nil || moved.Revision != 2 {
		t.Fatal(moved, err)
	}
	cur, ok, err := l.Current("zeocreator")
	if err != nil || !ok || cur.Where != "pomar:macbook" || cur.Handover != "rt-receipt-1" {
		t.Fatal(cur, err)
	}
	// Revisions are add-only: the first is still there, unchanged.
	b, _ := os.ReadFile(filepath.Join(l.Dir, "zeocreator", "location-000000000001.json"))
	if !strings.Contains(string(b), "mac-mini-naked") {
		t.Fatal("an earlier revision was rewritten")
	}
}

func TestAStaleRevisionIsRefused(t *testing.T) {
	l := locations(t)
	l.Move("zeocreator", 0, Location{Where: "mac-mini-naked", DeclarationSHA256: decl})
	if _, err := l.Move("zeocreator", 0, Location{Where: "mac-mini-naked", DeclarationSHA256: decl}); !errors.Is(err, ErrRevision) {
		t.Fatal(err)
	}
	if _, err := l.Move("zeocreator", 5, Location{Where: "mac-mini-naked", DeclarationSHA256: decl}); !errors.Is(err, ErrRevision) {
		t.Fatal(err)
	}
}

// Of concurrent movers from one revision, exactly one wins.
func TestConcurrentMoversOneWins(t *testing.T) {
	l := locations(t)
	l.Move("zeocreator", 0, Location{Where: "mac-mini-naked", DeclarationSHA256: decl})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Move("zeocreator", 1, Location{Where: "mac-mini-naked", DeclarationSHA256: decl}); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d movers won", wins)
	}
	if cur, _, err := l.Current("zeocreator"); err != nil || cur.Revision != 2 {
		t.Fatal(cur, err)
	}
}

// A gap, a stray file or a non-private directory is refused, never read
// past.
func TestTheRecordIsCheckedBeforeItIsRead(t *testing.T) {
	l := locations(t)
	l.Move("zeocreator", 0, Location{Where: "mac-mini-naked", DeclarationSHA256: decl})
	os.WriteFile(filepath.Join(l.Dir, "zeocreator", "location-000000000003.json"), []byte(`{}`), 0o600)
	if _, _, err := l.Current("zeocreator"); err == nil {
		t.Fatal("a gap was accepted")
	}
	os.Remove(filepath.Join(l.Dir, "zeocreator", "location-000000000003.json"))
	os.WriteFile(filepath.Join(l.Dir, "zeocreator", ".pending-x"), nil, 0o600)
	if _, _, err := l.Current("zeocreator"); err == nil {
		t.Fatal("a pending file was accepted")
	}
	os.Remove(filepath.Join(l.Dir, "zeocreator", ".pending-x"))
	os.Chmod(l.Dir, 0o755)
	if _, _, err := l.Current("zeocreator"); err == nil {
		t.Fatal("a non-private directory was accepted")
	}
	if _, _, err := l.Current("../etc"); err == nil {
		t.Fatal("an invalid seat name was accepted")
	}
}
