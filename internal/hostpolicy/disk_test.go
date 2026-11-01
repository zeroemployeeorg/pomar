package hostpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func readTrustedFixture(path string) DiskLimit {
	return ReadDiskLimitWithOptions(path, ReadOptions{OwnerUID: func(os.FileInfo) (uint32, bool) { return 0, true }})
}

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
	if got := readTrustedFixture(p); got.Percent != 99 || got.Source != p {
		t.Fatalf("got %+v", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if got := readTrustedFixture(p); got.Percent != 85 || !strings.Contains(got.Source, "unreadable") {
		t.Fatalf("got %+v", got)
	}
}

func TestDiskLimitRejectsNonregularAndUnreadableFiles(t *testing.T) {
	d := t.TempDir()
	if got := readTrustedFixture(d); got.Percent != 85 {
		t.Fatal(got)
	}
	p := filepath.Join(d, "fifo")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	if got := readTrustedFixture(p); got.Percent != 85 {
		t.Fatal(got)
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if got := readTrustedFixture(link); got.Percent != 85 {
		t.Fatal(got)
	}
	file := filepath.Join(d, "unreadable")
	if err := os.WriteFile(file, []byte("99"), 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0600) })
	if os.Geteuid() != 0 {
		if got := readTrustedFixture(file); got.Percent != 85 {
			t.Fatal(got)
		}
	}
}

func TestHighFillSourceProfileIs99(t *testing.T) {
	got := readTrustedFixture("../../config/host-policy/high-fill/max-fill-percent")
	if got.Percent != 99 {
		t.Fatalf("profile %+v", got)
	}
}

// Metadata is injected, but the same production guard evaluates it. The mode
// bits are real on these private files; no root-owned files are changed.
func TestDiskLimitRequiresRootNonwritableFileAndParent(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		fileUID, parentUID   uint32
		fileMode, parentMode os.FileMode
		known                bool
		want                 int
		part                 string
	}{
		{"trusted", 0, 0, 0644, 0755, true, 99, ""},
		{"job owns file", 503, 0, 0644, 0755, true, 85, "file"},
		{"service owns file", 502, 0, 0644, 0755, true, 85, "file"},
		{"file group writable", 0, 0, 0664, 0755, true, 85, "file"},
		{"file world writable", 0, 0, 0666, 0755, true, 85, "file"},
		{"job owns parent", 0, 503, 0644, 0755, true, 85, "parent"},
		{"parent group writable", 0, 0, 0644, 0775, true, 85, "parent"},
		{"parent world writable", 0, 0, 0644, 0777, true, 85, "parent"},
		{"unknown ownership", 0, 0, 0644, 0755, false, 85, "parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "max-fill-percent")
			if err := os.WriteFile(path, []byte("99\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.fileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, tc.parentMode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			got := ReadDiskLimitWithOptions(path, ReadOptions{OwnerUID: func(info os.FileInfo) (uint32, bool) {
				if info.IsDir() {
					return tc.parentUID, tc.known
				}
				return tc.fileUID, tc.known
			}})
			if got.Percent != tc.want || !strings.Contains(got.Source, path) || (tc.part != "" && !strings.Contains(got.Source, tc.part)) {
				t.Fatalf("policy %+v", got)
			}
		})
	}
}

func TestDiskLimitDefaultReaderRejectsActualUnprivilegedOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this assertion needs the unprivileged fixture owner")
	}
	d := t.TempDir()
	p := filepath.Join(d, "max-fill-percent")
	if err := os.WriteFile(p, []byte("99"), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadDiskLimit(p)
	if got.Percent != 85 || !strings.Contains(got.Source, "parent must be uid 0") {
		t.Fatalf("policy %+v", got)
	}
}

func TestDiskLimitRejectsInRootSymlinkAndParentSymlink(t *testing.T) {
	d := t.TempDir()
	file := filepath.Join(d, "regular")
	if err := os.WriteFile(file, []byte("99"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readTrustedFixture(file); got.Percent != 99 {
		t.Fatal(got)
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink("regular", link); err != nil {
		t.Fatal(err)
	}
	if got := readTrustedFixture(link); got.Percent != 85 {
		t.Fatal(got)
	}
	parentAlias := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(d, parentAlias); err != nil {
		t.Fatal(err)
	}
	if got := readTrustedFixture(filepath.Join(parentAlias, "regular")); got.Percent != 85 || !strings.Contains(got.Source, "parent") {
		t.Fatal(got)
	}
}

func TestDiskLimitRejectsUnknownFileOwnership(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "max-fill-percent")
	if err := os.WriteFile(p, []byte("99"), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadDiskLimitWithOptions(p, ReadOptions{OwnerUID: func(info os.FileInfo) (uint32, bool) {
		if info.IsDir() {
			return 0, true
		}
		return 0, false
	}})
	if got.Percent != 85 || !strings.Contains(got.Source, "file must be uid 0") {
		t.Fatalf("policy %+v", got)
	}
}

func TestDiskLimitRejectsUnsafeOrUnknownOpenedACL(t *testing.T) {
	for _, bad := range []string{"parent", "file"} {
		t.Run(bad, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "max-fill-percent")
			if err := os.WriteFile(path, []byte("99\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var checked int
			got := ReadDiskLimitWithOptions(path, ReadOptions{OwnerUID: func(os.FileInfo) (uint32, bool) { return 0, true }, ACLSafe: func(f *os.File) bool {
				checked++
				info, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if bad == "parent" {
					return !info.IsDir()
				}
				return info.IsDir()
			}})
			if got.Percent != 85 || !strings.Contains(got.Source, "ACL") || checked == 0 {
				t.Fatalf("unknown ACL accepted: %+v checked%d", got, checked)
			}
		})
	}
}
