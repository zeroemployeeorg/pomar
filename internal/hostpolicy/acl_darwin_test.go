package hostpolicy

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeDiskPolicyACLRefusal(t *testing.T) {
	for _, parentACL := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "parent"}[parentACL], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "max-fill-percent")
			if err := os.WriteFile(path, []byte("99\n"), 0600); err != nil {
				t.Fatal(err)
			}
			// Only private fixtures are changed. Root ownership is injected; ACL reads
			// use the actual kernel descriptor path and Unix permissions stay private.
			chmod := func(args ...string) {
				t.Helper()
				b, err := exec.Command("/bin/chmod", args...).CombinedOutput()
				if err != nil {
					t.Fatalf("chmod fixture: %v %s", err, b)
				}
			}
			chmod("-N", dir)
			chmod("-N", path)
			opts := ReadOptions{OwnerUID: func(os.FileInfo) (uint32, bool) { return 0, true }}
			if got := ReadDiskLimitWithOptions(path, opts); got.Percent != 99 {
				t.Fatalf("ACL-free control: %+v", got)
			}
			target, ace := path, "everyone allow write"
			if parentACL {
				target, ace = dir, "everyone allow add_file,delete_child"
			}
			chmod("+a", ace, target)
			defer chmod("-N", target)
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0022 != 0 {
				t.Fatal("native ACL fixture unexpectedly changed mode bits")
			}
			if got := ReadDiskLimitWithOptions(path, opts); got.Percent != 85 || !strings.Contains(got.Source, "ACL") {
				t.Fatalf("named write ACL accepted: %+v", got)
			}
			chmod("-N", target)
			if got := ReadDiskLimitWithOptions(path, opts); got.Percent != 99 {
				t.Fatalf("removed ACL did not restore trusted control: %+v", got)
			}
		})
	}
}

func TestDarwinACLMetadataMustBeExactEmpty(t *testing.T) {
	good := make([]byte, 12)
	binary.LittleEndian.PutUint32(good[:4], 12)
	binary.LittleEndian.PutUint32(good[4:8], 8)
	if !emptyDarwinACL(good) {
		t.Fatal("empty ACL rejected")
	}
	for _, bad := range [][]byte{nil, good[:11], append(append([]byte{}, good...), 0)} {
		if emptyDarwinACL(bad) {
			t.Fatal("invalid size accepted")
		}
	}
	for _, offset := range []int{0, 4, 8} {
		b := append([]byte{}, good...)
		b[offset]++
		if emptyDarwinACL(b) {
			t.Fatalf("corrupt field at %d accepted", offset)
		}
	}
}
