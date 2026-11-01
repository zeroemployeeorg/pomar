package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/smoke"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

type fixture struct {
	parent, root, inputs, packet, pin string
	m                                 manifest
}

func sum(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("owner preparation refuses root")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{parent: parent, root: filepath.Join(parent, "new-venue"), inputs: filepath.Join(parent, "inputs"), packet: filepath.Join(parent, "inputs.json"), m: manifest{Schema: "pomar.native-cache-seed/v1"}}
	if err := os.Mkdir(f.inputs, 0700); err != nil {
		t.Fatal(err)
	}
	for i, raw := range []string{"state", "blob", "deb", "kernel"} {
		name := []string{"state", "blob", "deb", "kernel"}[i]
		target := []string{"store/state.json", "store/content/blobs/sha256/" + sum([]byte(raw)), "downloads/debs/" + sum([]byte(raw)) + ".deb", "kernels/" + smoke.KernelName}[i]
		if err := os.WriteFile(filepath.Join(f.inputs, name), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		f.m.Files = append(f.m.Files, inputFile{Source: name, Target: target, Bytes: int64(len(raw)), SHA256: sum([]byte(raw))})
	}
	f.save(t)
	return f
}

func (f *fixture) save(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(f.m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.packet, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.pin = sum(raw)
}

func prepareFixture(f *fixture, afterIntent func(cache) error) error {
	return prepare(f.root, f.inputs, f.packet, f.pin, &testHooks{afterIntent: afterIntent, diskGuard: func(*venue.Venue) error { return nil }})
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected target effect %s: %v", path, err)
	}
}

func TestIntentPrecedesEveryObjectAndOnlyStaticCachesAreCreated(t *testing.T) {
	f := newFixture(t)
	intents := 0
	err := prepareFixture(f, func(c cache) error {
		intents++
		raw, err := os.ReadFile(filepath.Join(f.root, "ledger.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var last venue.Entry
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
			t.Fatal(err)
		}
		if last.Op != venue.OpIntent || last.Kind != c.kind || last.Class != venue.ClassCache || last.ID != c.id {
			t.Fatalf("copy not preceded by its actual intent: %+v", last)
		}
		assertAbsent(t, filepath.Join(f.root, c.path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if intents != 4 {
		t.Fatalf("intents = %d", intents)
	}
	v, err := venue.Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.OpenObjects()) != 4 {
		t.Fatalf("unexpected objects: %+v", v.OpenObjects())
	}
	for _, object := range v.OpenObjects() {
		if object.Class != venue.ClassCache || object.Last != venue.OpCreated || object.Kind == venue.KindVM {
			t.Fatalf("not a completed static cache: %+v", object)
		}
	}
	for _, file := range f.m.Files {
		p := filepath.Join(f.root, file.Target)
		raw, err := os.ReadFile(p)
		if err != nil || sum(raw) != file.SHA256 {
			t.Fatalf("readback %s: %v", p, err)
		}
		s, err := os.Stat(p)
		if err != nil || s.Mode().Perm() != 0444 {
			t.Fatalf("mode %s: %v", p, err)
		}
	}
	unaccounted, err := v.Unaccounted()
	if err != nil || len(unaccounted) != 0 {
		t.Fatalf("unaccounted=%v err=%v", unaccounted, err)
	}
	assertAbsent(t, filepath.Join(f.root, "verified-boot"))
}

func TestAllSourcesVerifiedBeforeTargetEffects(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.inputs, "kernel"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareFixture(f, nil); err == nil {
		t.Fatal("accepted changed source")
	}
	assertAbsent(t, f.root)
}

func TestCopyChangeRemainsFailedAndPreservesPartialCustody(t *testing.T) {
	f := newFixture(t)
	err := prepareFixture(f, func(c cache) error {
		if c.id == "image-store" {
			return os.WriteFile(filepath.Join(f.inputs, "blob"), []byte("oops"), 0600)
		}
		return nil
	})
	if err == nil {
		t.Fatal("accepted changed copy")
	}
	v, err := venue.Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := v.ObjectState(venue.KindImage, "image-store")
	if !ok || object.Last != venue.OpFailed {
		t.Fatalf("failed custody lost: %+v %v", object, ok)
	}
	if _, err := os.Stat(filepath.Join(f.root, "store/state.json")); err != nil {
		t.Fatalf("partial evidence removed: %v", err)
	}
	if _, ok := v.ObjectState(venue.KindImage, "kernel"); ok {
		t.Fatal("advanced after failed cache")
	}
}

func TestExistingRootNeverReplaysOrOverwrites(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.root, "unknown-prior-operation")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareFixture(f, nil); err == nil {
		t.Fatal("replayed existing root")
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "preserve" {
		t.Fatal("old state changed")
	}
	assertAbsent(t, filepath.Join(f.root, "ledger.jsonl"))
}

func TestRedirectedAndNonregularInputsRefusedBeforeEffects(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := filepath.Join(f.inputs, "blob")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink("state", p); err != nil {
					t.Fatal(err)
				}
			} else if err := syscall.Mkfifo(p, 0600); err != nil {
				t.Fatal(err)
			}
			if err := prepareFixture(f, nil); err == nil {
				t.Fatal("accepted redirected/nonregular input")
			}
			assertAbsent(t, f.root)
		})
	}
}

func TestManifestCannotMintOtherObjectsOrEscape(t *testing.T) {
	for _, target := range []string{"../outside", "/tmp/outside", "bases/fake/rootfs.ext4", "bases/fake/verified-boot", "store/../elsewhere", "store/content/blobs/sha256/not-the-pin"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t)
			f.m.Files[1].Target = target
			f.save(t)
			if err := prepareFixture(f, nil); err == nil {
				t.Fatal("accepted target")
			}
			assertAbsent(t, f.root)
		})
	}
}

func TestIndependentManifestPinAndDuplicateTargetsRefused(t *testing.T) {
	f := newFixture(t)
	if err := prepare(f.root, f.inputs, f.packet, strings.Repeat("0", 64), nil); err == nil {
		t.Fatal("accepted wrong manifest pin")
	}
	assertAbsent(t, f.root)
	f.m.Files = append(f.m.Files, f.m.Files[0])
	f.save(t)
	if err := prepareFixture(f, nil); err == nil {
		t.Fatal("accepted duplicate target")
	}
	assertAbsent(t, f.root)
}

func TestCapacityRefusalCreatesNoCacheObject(t *testing.T) {
	f := newFixture(t)
	refusal := venue.ErrDiskFull
	err := prepare(f.root, f.inputs, f.packet, f.pin, &testHooks{diskGuard: func(*venue.Venue) error { return refusal }})
	if err != refusal {
		t.Fatalf("got %v, want capacity refusal", err)
	}
	v, e := venue.Open(f.root)
	if e != nil {
		t.Fatal(e)
	}
	if len(v.OpenObjects()) != 0 {
		t.Fatal("capacity refusal created a cache object")
	}
	assertAbsent(t, filepath.Join(f.root, "store"))
}

func TestConcurrentAttemptsCannotShareAFreshVenue(t *testing.T) {
	f := newFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- prepareFixture(f, nil) }()
	}
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("need one writer and one refusal: %v / %v", a, b)
	}
	v, err := venue.Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.OpenObjects()) != 4 {
		t.Fatalf("concurrent ledger changed: %+v", v.OpenObjects())
	}
}

func TestSecondCacheFailureLeavesNoCompletionAndConsumerRefuses(t *testing.T) {
	f := newFixture(t)
	err := prepareFixture(f, func(c cache) error {
		if c.id == "debs" {
			return errors.New("injected second-group failure")
		}
		return nil
	})
	if err == nil {
		t.Fatal("accepted failed second group")
	}
	assertAbsent(t, filepath.Join(f.root, receiptPath))
	if err := verifyPrepared(f.root, f.packet, f.pin); err == nil {
		t.Fatal("consumer accepted partial seed")
	}
	v, e := venue.Open(f.root)
	if e != nil {
		t.Fatal(e)
	}
	store, ok := v.ObjectState(venue.KindImage, "image-store")
	if !ok || store.Last != venue.OpCreated {
		t.Fatal("real completed first cache lost")
	}
	debs, ok := v.ObjectState(venue.KindDownload, "debs")
	if !ok || debs.Last != venue.OpFailed {
		t.Fatal("failed cache custody lost")
	}
	if _, ok := v.ObjectState(venue.KindDownload, receiptID); ok {
		t.Fatal("completion was attempted before all caches")
	}
}

func TestCompletionIntentWithoutCommitRefusesConsumer(t *testing.T) {
	f := newFixture(t)
	err := prepareFixture(f, func(c cache) error {
		if c.id == receiptID {
			return errors.New("stop before receipt bytes")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected failed completion")
	}
	assertAbsent(t, filepath.Join(f.root, receiptPath))
	if err := verifyPrepared(f.root, f.packet, f.pin); err == nil {
		t.Fatal("consumer accepted missing completion")
	}
}

func TestReadOnlyCompletionGateRejectsMissingDriftOrWrongRoot(t *testing.T) {
	for _, kind := range []string{"missing", "receipt-drift", "target-drift", "ledger-failed", "root-moved"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if err := prepareFixture(f, nil); err != nil {
				t.Fatal(err)
			}
			if err := verifyPrepared(f.root, f.packet, f.pin); err != nil {
				t.Fatal(err)
			}
			receipt := filepath.Join(f.root, receiptPath)
			switch kind {
			case "missing":
				if err := os.Remove(receipt); err != nil {
					t.Fatal(err)
				}
			case "receipt-drift":
				raw, e := os.ReadFile(receipt)
				if e != nil {
					t.Fatal(e)
				}
				var r completion
				if e = json.Unmarshal(raw, &r); e != nil {
					t.Fatal(e)
				}
				r.ManifestSHA256 = strings.Repeat("0", 64)
				raw, _ = json.Marshal(r)
				if e = os.Chmod(receipt, 0600); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(receipt, raw, 0444); e != nil {
					t.Fatal(e)
				}
			case "target-drift":
				p := filepath.Join(f.root, f.m.Files[1].Target)
				if e := os.Chmod(p, 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(p, []byte("oops"), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Chmod(p, 0444); e != nil {
					t.Fatal(e)
				}
			case "ledger-failed":
				v, e := venue.Open(f.root)
				if e != nil {
					t.Fatal(e)
				}
				if e = v.Failed(venue.KindDownload, receiptID, "uncertain final observation"); e != nil {
					t.Fatal(e)
				}
			case "root-moved":
				newRoot := filepath.Join(f.parent, "moved-venue")
				if e := os.Rename(f.root, newRoot); e != nil {
					t.Fatal(e)
				}
				f.root = newRoot
			}
			before, e := os.ReadFile(filepath.Join(f.root, "ledger.jsonl"))
			if e != nil {
				t.Fatal(e)
			}
			if e := verifyPrepared(f.root, f.packet, f.pin); e == nil {
				t.Fatal("consumer accepted invalid completion")
			}
			after, e := os.ReadFile(filepath.Join(f.root, "ledger.jsonl"))
			if e != nil || string(before) != string(after) {
				t.Fatal("verification changed ledger")
			}
		})
	}
}
