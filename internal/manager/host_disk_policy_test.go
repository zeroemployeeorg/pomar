package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/hostpolicy"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// One actual file drives both predicates; fake filesystem counters keep this
// independent of the host's real occupancy. The high value is never cached.
func TestHostDiskSettingFeedsHeavyWorkAndAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "max-fill-percent")
	if err := os.WriteFile(path, []byte("99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := venue.OpenWithOptions(t.TempDir(), venue.Options{
		ReadDiskLimit: func() hostpolicy.DiskLimit { return hostpolicy.ReadDiskLimit(path) },
		Statfs: func(_ string, s *syscall.Statfs_t) error {
			*s = syscall.Statfs_t{Bsize: 4096, Blocks: 1000 << 18, Bfree: 100 << 18, Bavail: 100 << 18}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour, Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	admit := func() error { m.mu.Lock(); defer m.mu.Unlock(); return m.admit(capacity.CI) }
	if err := v.CheckHostDisk(); err != nil {
		t.Fatal(err)
	}
	if err := admit(); err != nil {
		t.Fatal(err)
	}
	status, err := m.Capacity()
	if err != nil || status.Allocation.FillLimit.Percent != 99 || status.Allocation.FillLimit.Source != path {
		t.Fatalf("status %+v %v", status, err)
	}
	if err := os.WriteFile(path, []byte("85"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{v.CheckHostDisk(), admit()} {
		if err == nil || !strings.Contains(err.Error(), "85%") || !strings.Contains(err.Error(), path) {
			t.Fatalf("limit/source missing: %v", err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := v.CheckHostDisk(); !errors.Is(err, venue.ErrDiskFull) || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unreadable heavy: %v", err)
	}
	if err := admit(); !errors.Is(err, capacity.ErrRefused) || !strings.Contains(err.Error(), "85%") || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unreadable admission: %v", err)
	}
}
