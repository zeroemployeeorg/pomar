package hostpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDiskLimitParsingAndFallback(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"99\n", 99}, {"85", 85}, {"1", 1}, {" 98 \n", 98},
		{"", 85}, {"0", 85}, {"100", 85}, {"-1", 85}, {"+99", 85},
		{"99 85", 85}, {"99x", 85}, {strings.Repeat("9", 33), 85},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got := parseDiskLimit("fixture-policy", []byte(tc.text))
			if got.Percent != tc.want || !strings.Contains(got.Source, "fixture-policy") {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDiskLimitIsReadAgainAndMissingFallsBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "max-fill-percent")
	if err := os.WriteFile(p, []byte("99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ReadDiskLimit(p); got.Percent != 99 || got.Source != p {
		t.Fatalf("got %+v", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if got := ReadDiskLimit(p); got.Percent != 85 || !strings.Contains(got.Source, "unreadable") {
		t.Fatalf("got %+v", got)
	}
}

func TestDiskLimitRejectsNonregularAndUnreadableFiles(t *testing.T) {
	d := t.TempDir()
	if got := ReadDiskLimit(d); got.Percent != 85 {
		t.Fatal(got)
	}
	p := filepath.Join(d, "fifo")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	if got := ReadDiskLimit(p); got.Percent != 85 {
		t.Fatal(got)
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if got := ReadDiskLimit(link); got.Percent != 85 {
		t.Fatal(got)
	}
	file := filepath.Join(d, "unreadable")
	if err := os.WriteFile(file, []byte("99"), 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0600) })
	if os.Geteuid() != 0 {
		if got := ReadDiskLimit(file); got.Percent != 85 {
			t.Fatal(got)
		}
	}
}

func TestHighFillSourceProfileIs99(t *testing.T) {
	got := ReadDiskLimit("../../config/host-policy/high-fill/max-fill-percent")
	if got.Percent != 99 {
		t.Fatalf("profile %+v", got)
	}
}
