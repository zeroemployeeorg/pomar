package volume

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func openVenue(t *testing.T) *venue.Venue {
	t.Helper()
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCreateRefusesBadInput(t *testing.T) {
	vs := &Volumes{Venue: openVenue(t), Hdiutil: func(context.Context, ...string) ([]byte, error) {
		t.Fatal("hdiutil called for bad input")
		return nil, nil
	}}
	for _, tc := range []struct {
		id   string
		size int64
	}{{"Bad/id", 64 * MiB}, {"ok", 63 * MiB}, {"ok", 64*MiB + 1}} {
		if _, err := vs.Create(context.Background(), tc.id, tc.size); err == nil {
			t.Errorf("Create(%q, %d) accepted", tc.id, tc.size)
		}
	}
}

// With an hdiutil that "succeeds" but mounts nothing, Create must not
// ledger a created volume, and must leave nothing behind.
func TestCreateTearsDownWhenNothingMounts(t *testing.T) {
	v := openVenue(t)
	var calls []string
	vs := &Volumes{Venue: v, Hdiutil: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args[0])
		return nil, nil
	}}
	if _, err := vs.Create(context.Background(), "t1", 64*MiB); err == nil {
		t.Fatal("Create succeeded without a mount")
	}
	if strings.Join(calls, ",") != "create,attach" {
		t.Fatalf("hdiutil calls = %v", calls)
	}
	if v.IsOpen(venue.KindVolume, "bounded-t1") {
		t.Fatal("failed volume left open")
	}
	if un, err := v.Unaccounted(); err != nil || len(un) != 0 {
		t.Fatalf("Unaccounted = %v, %v", un, err)
	}
}

func TestCreateReportsHdiutilFailure(t *testing.T) {
	v := openVenue(t)
	vs := &Volumes{Venue: v, Hdiutil: func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("boom")
	}}
	if _, err := vs.Create(context.Background(), "t2", 64*MiB); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Create = %v, want the hdiutil error", err)
	}
	if v.IsOpen(venue.KindVolume, "bounded-t2") {
		t.Fatal("failed volume left open")
	}
}

func TestRemoveOnlyLedgeredVolumes(t *testing.T) {
	vs := &Volumes{Venue: openVenue(t)}
	if err := vs.Remove(context.Background(), "never"); err == nil {
		t.Fatal("Remove of an unledgered volume succeeded")
	}
}

// The mounted case is covered by TestRealVolume.
func TestMounted(t *testing.T) {
	if Mounted(t.TempDir()) {
		t.Error("a plain directory reads as a mount point")
	}
	if Mounted("/nonexistent/pomar") {
		t.Error("a missing path reads as a mount point")
	}
}

// TestRealVolume attaches a real image. It runs only when asked, since it
// mounts a filesystem: POMAR_TEST_HDIUTIL=1.
func TestRealVolume(t *testing.T) {
	if os.Getenv("POMAR_TEST_HDIUTIL") != "1" {
		t.Skip("set POMAR_TEST_HDIUTIL=1 to attach a real disk image")
	}
	v := openVenue(t)
	vs := &Volumes{Venue: v}
	ctx := context.Background()
	mnt, err := vs.Create(ctx, "real", 64*MiB)
	if err != nil {
		t.Fatal(err)
	}
	if !Mounted(mnt) {
		t.Fatal("not mounted after Create")
	}
	if err := vs.Remove(ctx, "real"); err != nil {
		t.Fatal(err)
	}
	if un, err := v.Unaccounted(); err != nil || len(un) != 0 {
		t.Fatalf("Unaccounted after Remove = %v, %v", un, err)
	}
}
