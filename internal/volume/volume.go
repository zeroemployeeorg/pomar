// Package volume makes bounded APFS volumes inside the data root: a sparse
// disk image, ledgered before it exists, attached with no Finder presence at
// a mount point inside its own object. A bounded volume lets a test fill a
// filesystem without filling the host's.
package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// Dir is the venue structure directory that holds bounded volumes.
const Dir = "volumes"

const (
	imageName = "volume.sparseimage"
	mountName = "mnt"
)

// MiB is 2^20 bytes.
const MiB = int64(1) << 20

// Volumes creates and removes bounded volumes in one venue.
type Volumes struct {
	Venue *venue.Venue
	// Hdiutil runs hdiutil; nil runs /usr/bin/hdiutil.
	Hdiutil func(ctx context.Context, args ...string) ([]byte, error)
}

func (vs *Volumes) hdiutil(ctx context.Context, args ...string) ([]byte, error) {
	if vs.Hdiutil != nil {
		return vs.Hdiutil(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("volume: hdiutil %s: %w: %s", args[0], err, out)
	}
	return out, nil
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func objectID(id string) string { return "bounded-" + id }

// MountPoint returns where volume id is attached.
func (vs *Volumes) MountPoint(id string) string {
	return filepath.Join(vs.Venue.Root(), Dir, id, mountName)
}

// Create makes a sparse APFS image of size bytes (whole MiB) and attaches it.
// It returns the mount point. A failure after the intent tears down what it
// made.
func (vs *Volumes) Create(ctx context.Context, id string, size int64) (string, error) {
	v := vs.Venue
	if !validID.MatchString(id) {
		return "", fmt.Errorf("volume: invalid id %q", id)
	}
	if size < 64*MiB || size%MiB != 0 {
		return "", fmt.Errorf("volume: size %d is not a whole number of MiB of at least 64", size)
	}
	if err := v.Init(); err != nil {
		return "", err
	}
	oid, rel := objectID(id), filepath.Join(Dir, id)
	if err := v.Intent(venue.KindVolume, venue.ClassAttempt, oid, rel, fmt.Sprintf("bounded volume, %d MiB", size/MiB)); err != nil {
		return "", err
	}
	dir := filepath.Join(v.Root(), rel)
	img, mnt := filepath.Join(dir, imageName), filepath.Join(dir, mountName)
	fail := func(err error) (string, error) {
		if Mounted(mnt) {
			vs.detach(ctx, mnt)
		}
		v.Failed(venue.KindVolume, oid, err.Error())
		if !Mounted(mnt) {
			v.Teardown(venue.KindVolume, oid)
		}
		return "", err
	}
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return fail(err)
	}
	if _, err := vs.hdiutil(ctx, "create", "-size", fmt.Sprintf("%dm", size/MiB), "-type", "SPARSE",
		"-fs", "APFS", "-volname", "pomar-"+id, img); err != nil {
		return fail(err)
	}
	if _, err := vs.hdiutil(ctx, "attach", "-nobrowse", "-noautoopen", "-owners", "on", "-mountpoint", mnt, img); err != nil {
		return fail(err)
	}
	if !Mounted(mnt) {
		return fail(errors.New("volume: attach reported success but nothing is mounted"))
	}
	if err := v.Created(venue.KindVolume, oid); err != nil {
		return fail(err)
	}
	return mnt, nil
}

func (vs *Volumes) detach(ctx context.Context, mnt string) error {
	if _, err := vs.hdiutil(ctx, "detach", mnt); err != nil {
		if _, err2 := vs.hdiutil(ctx, "detach", "-force", mnt); err2 != nil {
			return fmt.Errorf("%w; forced: %v", err, err2)
		}
	}
	return nil
}

// ErrStillMounted is returned when a volume cannot be detached. Its object
// is left open: tearing it down would delete through the mount.
var ErrStillMounted = errors.New("volume: still mounted; not torn down")

// Remove detaches volume id and tears its object down.
func (vs *Volumes) Remove(ctx context.Context, id string) error {
	oid := objectID(id)
	if !vs.Venue.IsOpen(venue.KindVolume, oid) {
		return fmt.Errorf("volume: %s is not an open ledgered volume", id)
	}
	mnt := vs.MountPoint(id)
	if Mounted(mnt) {
		if err := vs.detach(ctx, mnt); err != nil {
			return err
		}
	}
	if Mounted(mnt) {
		return fmt.Errorf("%w: %s", ErrStillMounted, id)
	}
	return vs.Venue.Teardown(venue.KindVolume, oid)
}

// Mounted reports whether dir is a mount point: it is on another device
// than its parent.
func Mounted(dir string) bool {
	var a, b syscall.Stat_t
	if syscall.Lstat(dir, &a) != nil || syscall.Lstat(filepath.Dir(dir), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}
