// Package base builds and verifies base root filesystems: one ext4 image per
// guest image, keyed by the image's arm64 manifest digest, built once and
// never written again. Attempts boot from APFS clones of a base (see the
// helper), so an attempt costs only the blocks it writes.
package base

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// Dir is the venue structure directory that holds bases.
const Dir = "bases"

var digestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// Bases operates on the bases in one data root.
type Bases struct {
	Venue   *venue.Venue
	HostBin string // the signed pomar-host, which does the unpacking
	// BootID returns an identifier of the current machine boot; the default
	// reads kern.boottime. Tests replace it.
	BootID func() (string, error)
	// PackageSet is the hash of the pinned packages installed into the base
	// (debs.SetHash), or "" for the image alone. It is part of the base's
	// key.
	PackageSet string
	// Layers are the packages' data archives, unpacked after the image's
	// layers when the base is built.
	Layers []string
}

func hexOf(digest string) (string, error) {
	m := digestRE.FindStringSubmatch(digest)
	if m == nil {
		return "", fmt.Errorf("base: %q is not a sha256 digest", digest)
	}
	return m[1], nil
}

// key names a base's directory: the image's arm64 manifest hash, and with
// packages, the package set's hash too, so a changed set is a new base.
func (b *Bases) key(h string) string {
	if b.PackageSet == "" {
		return h
	}
	return h + "-" + b.PackageSet[:16]
}

func (b *Bases) objID(h string) string {
	if b.PackageSet == "" {
		return "base-" + h[:12]
	}
	return "base-" + h[:12] + "-" + b.PackageSet[:12]
}

// Path returns the root filesystem path of the base for digest.
func (b *Bases) Path(digest string) (string, error) {
	h, err := hexOf(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(b.Venue.Root(), Dir, b.key(h), "rootfs.ext4"), nil
}

// Exists reports whether a base for digest is ledgered and open.
func (b *Bases) Exists(digest string) bool {
	h, err := hexOf(digest)
	return err == nil && b.Venue.IsOpen(venue.KindImage, b.objID(h))
}

// Build unpacks the image into a new base, records its sha256, and makes it
// read-only. arm64Digest must be what pomar-host reports for the image.
func (b *Bases) Build(ctx context.Context, store, imageRef, imageDigest, arm64Digest string, sizeBytes uint64) error {
	h, err := hexOf(arm64Digest)
	if err != nil {
		return err
	}
	v := b.Venue
	if err := v.Init(); err != nil {
		return err
	}
	if b.Exists(arm64Digest) {
		// A base of another capacity is replaced, through the ledger; its
		// clones are independent copies, so live attempts are unaffected.
		p, _ := b.Path(arm64Digest)
		fi, err := os.Stat(p)
		if err == nil && uint64(fi.Size()) == sizeBytes {
			return nil
		}
		was := "unreadable"
		if err == nil {
			was = fmt.Sprint(fi.Size())
		}
		if err := v.TeardownNote(venue.KindImage, b.objID(h), fmt.Sprintf("replaced: capacity %s bytes, now %d", was, sizeBytes)); err != nil {
			return err
		}
	}
	if err := v.CheckHeavy(venue.DefaultMaxFillPercent); err != nil {
		return err
	}
	rel := filepath.Join(Dir, b.key(h))
	note := "base rootfs for " + arm64Digest
	if b.PackageSet != "" {
		note += " with package set " + b.PackageSet
	}
	if err := v.Intent(venue.KindImage, venue.ClassCache, b.objID(h), rel, note); err != nil {
		return err
	}
	fail := func(err error) error {
		v.Failed(venue.KindImage, b.objID(h), err.Error())
		v.Teardown(venue.KindImage, b.objID(h))
		return err
	}
	dir := filepath.Join(v.Root(), rel)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	cmd := exec.CommandContext(ctx, b.HostBin, "build-base", "--store", store,
		"--image", imageRef, "--image-digest", imageDigest, "--out", rootfs,
		"--size-bytes", fmt.Sprint(sizeBytes))
	if len(b.Layers) > 0 {
		for _, l := range b.Layers {
			if strings.Contains(l, ",") {
				return fail(fmt.Errorf("base: layer path %q contains a comma", l))
			}
		}
		cmd.Args = append(cmd.Args, "--extra-layers", strings.Join(b.Layers, ","))
	}
	cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(v.Root(), "tmp")+string(filepath.Separator))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fail(fmt.Errorf("base: build-base: %w: %s", err, strings.TrimSpace(string(out))))
	}
	if !strings.Contains(string(out), "arm64_manifest="+arm64Digest) {
		return fail(fmt.Errorf("base: built image's arm64 manifest is not %s: %s", arm64Digest, strings.TrimSpace(string(out))))
	}
	sum, err := fileSHA256(rootfs)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.sha256"), []byte(sum+"\n"), 0o400); err != nil {
		return fail(err)
	}
	if err := os.Chmod(rootfs, 0o444); err != nil {
		return fail(err)
	}
	if err := v.Created(venue.KindImage, b.objID(h)); err != nil {
		return err
	}
	return b.markVerified(dir)
}

// ErrTampered is returned when a base no longer matches its recorded hash.
var ErrTampered = errors.New("base: rootfs does not match its recorded sha256")

// Verify checks a base before use. The hash is checked on the first use
// after each machine boot; later uses in the same boot check only that the
// file is still read-only. It returns the rootfs path to clone.
func (b *Bases) Verify(arm64Digest string) (string, error) {
	h, err := hexOf(arm64Digest)
	if err != nil {
		return "", err
	}
	if !b.Exists(arm64Digest) {
		return "", fmt.Errorf("base: no base for %s", arm64Digest)
	}
	dir := filepath.Join(b.Venue.Root(), Dir, b.key(h))
	rootfs := filepath.Join(dir, "rootfs.ext4")
	fi, err := os.Stat(rootfs)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o222 != 0 {
		return "", fmt.Errorf("base: %s is writable; bases must be read-only", rootfs)
	}
	boot, err := b.bootID()
	if err != nil {
		return "", err
	}
	if marker, err := os.ReadFile(filepath.Join(dir, "verified-boot")); err == nil && strings.TrimSpace(string(marker)) == boot {
		return rootfs, nil
	}
	want, err := os.ReadFile(filepath.Join(dir, "rootfs.sha256"))
	if err != nil {
		return "", err
	}
	got, err := fileSHA256(rootfs)
	if err != nil {
		return "", err
	}
	if got != strings.TrimSpace(string(want)) {
		return "", fmt.Errorf("%w: %s", ErrTampered, rootfs)
	}
	return rootfs, b.markVerified(dir)
}

func (b *Bases) markVerified(dir string) error {
	boot, err := b.bootID()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "verified-boot"), []byte(boot+"\n"), 0o600)
}

func (b *Bases) bootID() (string, error) {
	if b.BootID != nil {
		return b.BootID()
	}
	out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return "", fmt.Errorf("base: sysctl kern.boottime: %w", err)
	}
	// "{ sec = 1790211316, usec = 876591 } …": the seconds identify the boot.
	m := regexp.MustCompile(`sec = ([0-9]+)`).FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("base: unexpected kern.boottime %q", out)
	}
	return string(m[1]), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RecordedSHA256 returns the sha256 recorded when the base at rootfs (a path
// Path or Verify returned) was built. It reads the record; it does not hash
// the rootfs, which is gigabytes (elders' ruling r26 §4.1).
func RecordedSHA256(rootfs string) (string, error) {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(rootfs), "rootfs.sha256"))
	if err != nil {
		return "", err
	}
	sum := strings.TrimSpace(string(b))
	if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return "", fmt.Errorf("base: %s holds no sha256", filepath.Join(filepath.Dir(rootfs), "rootfs.sha256"))
	}
	return sum, nil
}

// ErrUnverified is returned when a base has not been verified since this
// machine booted: its hash is checked when the manager starts, never on an
// attempt's path (elders' note of 2026-09-26 on POMAR-SOW-06 §9).
var ErrUnverified = errors.New("base: not verified since this boot")

// Verified returns the rootfs path of a base that was verified since this
// boot and is still read-only. It never hashes the rootfs: an attempt never
// waits on a rehash. Anything else is ErrUnverified.
func (b *Bases) Verified(arm64Digest string) (string, error) {
	h, err := hexOf(arm64Digest)
	if err != nil {
		return "", err
	}
	if !b.Exists(arm64Digest) {
		return "", fmt.Errorf("base: no base for %s", arm64Digest)
	}
	dir := filepath.Join(b.Venue.Root(), Dir, b.key(h))
	rootfs := filepath.Join(dir, "rootfs.ext4")
	fi, err := os.Stat(rootfs)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o222 != 0 {
		return "", fmt.Errorf("%w: %s is writable", ErrUnverified, rootfs)
	}
	boot, err := b.bootID()
	if err != nil {
		return "", err
	}
	marker, err := os.ReadFile(filepath.Join(dir, "verified-boot"))
	if err != nil || strings.TrimSpace(string(marker)) != boot {
		return "", fmt.Errorf("%w: %s", ErrUnverified, rootfs)
	}
	return rootfs, nil
}
