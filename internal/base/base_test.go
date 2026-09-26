package base

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

const digest = "sha256:4220c5d84f685eb34a728d389bab47674b46f433b5218ae9a75a5fbd5e0be724"

// fakeBase ledgers and writes a base by hand, as Build would.
func fakeBase(t *testing.T, content string) (*Bases, *string) {
	t.Helper()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Init(); err != nil {
		t.Fatal(err)
	}
	boot := "1000"
	b := &Bases{Venue: v, BootID: func() (string, error) { return boot, nil }}
	h, _ := hexOf(digest)
	if err := v.Intent(venue.KindImage, venue.ClassCache, b.objID(h), filepath.Join(Dir, b.key(h)), ""); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(v.Root(), Dir, b.key(h))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte(content), 0o444); err != nil {
		t.Fatal(err)
	}
	sum, _ := fileSHA256(rootfs)
	if err := os.WriteFile(filepath.Join(dir, "rootfs.sha256"), []byte(sum+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := v.Created(venue.KindImage, b.objID(h)); err != nil {
		t.Fatal(err)
	}
	return b, &boot
}

// tamper rewrites a read-only base in place, as an attacker or a bug might.
func tamper(t *testing.T, path, content string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPerBoot(t *testing.T) {
	b, boot := fakeBase(t, "base-v1")
	p, err := b.Verify(digest)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	// Same boot: the hash is not re-read, so a change goes unseen until reboot.
	tamper(t, p, "base-v2")
	if _, err := b.Verify(digest); err != nil {
		t.Fatalf("same-boot verify re-hashed: %v", err)
	}
	// A new boot re-hashes and catches it.
	*boot = "2000"
	if _, err := b.Verify(digest); !errors.Is(err, ErrTampered) {
		t.Fatalf("new-boot verify = %v, want ErrTampered", err)
	}
}

func TestVerifyRefusesWritableBase(t *testing.T) {
	b, _ := fakeBase(t, "base-v1")
	p, _ := b.Path(digest)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(digest); err == nil {
		t.Fatal("writable base accepted")
	}
}

func TestVerifyRejects(t *testing.T) {
	b, _ := fakeBase(t, "x")
	if _, err := b.Verify("sha256:short"); err == nil {
		t.Error("malformed digest accepted")
	}
	other := "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := b.Verify(other); err == nil {
		t.Error("unknown base accepted")
	}
}

func TestBuildFailureLeavesNothing(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &Bases{Venue: v, HostBin: "/usr/bin/false", BootID: func() (string, error) { return "1", nil }}
	if err := b.Build(context.Background(), "store", "img", "sha256:x", digest, 1<<20); err == nil {
		t.Fatal("build with a failing host binary succeeded")
	}
	if b.Exists(digest) {
		t.Fatal("failed build left the base open")
	}
	if un, err := v.Unaccounted(); err != nil || len(un) != 0 {
		t.Fatalf("unaccounted after failed build: %v, %v", un, err)
	}
}

// A base of the requested capacity is kept; one of another capacity is torn
// down through the ledger, with a note, before the new one is built. The
// rebuild here fails (the host binary is false), which leaves nothing.
func TestBuildReplacesABaseOfAnotherCapacity(t *testing.T) {
	b, _ := fakeBase(t, "0123456789")
	b.HostBin = "/usr/bin/false"
	if err := b.Build(context.Background(), "store", "img", "sha256:x", digest, 10); err != nil {
		t.Fatalf("a base of the same capacity was not kept: %v", err)
	}
	if !b.Exists(digest) {
		t.Fatal("the same-capacity base is gone")
	}
	if err := b.Build(context.Background(), "store", "img", "sha256:x", digest, 1<<20); err == nil {
		t.Fatal("the rebuild with a failing host binary succeeded")
	}
	if b.Exists(digest) {
		t.Fatal("the old base survived a capacity change")
	}
	led, err := os.ReadFile(filepath.Join(b.Venue.Root(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(led), `"note":"replaced: capacity 10 bytes, now 1048576"`) {
		t.Fatalf("no noted replacement in the ledger:\n%s", led)
	}
}

// A package set is part of a base's key: the same image with and without
// the set, or with another set, are different bases.
func TestPackageSetKeysTheBase(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := &Bases{Venue: v}
	withA := &Bases{Venue: v, PackageSet: strings.Repeat("a", 64)}
	withB := &Bases{Venue: v, PackageSet: strings.Repeat("b", 64)}
	p0, _ := plain.Path(digest)
	pa, _ := withA.Path(digest)
	pb, _ := withB.Path(digest)
	if p0 == pa || pa == pb {
		t.Fatalf("paths do not differ: %s %s %s", p0, pa, pb)
	}
	if !strings.Contains(pa, "4220c5d84f68"+"5eb34a728d389bab47674b46f433b5218ae9a75a5fbd5e0be724-aaaaaaaaaaaaaaaa") {
		t.Fatalf("path %s lacks the set's hash", pa)
	}
	h, _ := hexOf(digest)
	if withA.objID(h) != "base-4220c5d84f68-aaaaaaaaaaaa" || plain.objID(h) != "base-4220c5d84f68" {
		t.Fatalf("ids %s %s", withA.objID(h), plain.objID(h))
	}
}

func TestRecordedSHA256ReadsTheRecordNotTheRootfs(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs.ext4")
	os.WriteFile(rootfs, []byte("not what the record says"), 0o400)
	sum := strings.Repeat("d", 64)
	os.WriteFile(filepath.Join(dir, "rootfs.sha256"), []byte(sum+"\n"), 0o400)
	if got, err := RecordedSHA256(rootfs); err != nil || got != sum {
		t.Fatalf("RecordedSHA256 = %q, %v", got, err)
	}
	os.Remove(filepath.Join(dir, "rootfs.sha256"))
	os.WriteFile(filepath.Join(dir, "rootfs.sha256"), []byte("garbage\n"), 0o600)
	if _, err := RecordedSHA256(rootfs); err == nil {
		t.Fatal("a record with no sha256 was accepted")
	}
	if _, err := RecordedSHA256(filepath.Join(t.TempDir(), "rootfs.ext4")); err == nil {
		t.Fatal("a missing record was accepted")
	}
}
