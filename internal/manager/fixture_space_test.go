package manager

import (
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/hostpolicy"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

// A fixture owns its disk counters. Mirror preparation and default admission
// must see the same healthy filesystem regardless of the build host's fill.
// Tests for full disks still override Config.Space explicitly.
func openFixtureVenue(root string) (*venue.Venue, error) {
	return venue.OpenWithOptions(root, venue.Options{ReadDiskLimit: hostpolicy.DefaultDiskLimit, Statfs: func(_ string, s *syscall.Statfs_t) error {
		*s = syscall.Statfs_t{Bsize: 4096, Blocks: 200 << 18, Bfree: 100 << 18, Bavail: 100 << 18}
		return nil
	}})
}
