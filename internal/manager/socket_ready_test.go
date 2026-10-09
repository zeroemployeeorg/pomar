package manager

import (
	"net"
	"os"
	"testing"
	"time"
)

// A socket pathname appears before listen has applied the final mode. Tests
// must wait for that mode and a real connection, not just pathname existence.
func testSocketReady(path string, mode os.FileMode) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != mode {
		return false
	}
	conn, err := net.DialTimeout("unix", path, 10*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func waitTestSocket(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if testSocketReady(path, mode) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket did not become connectable with mode %o", mode)
}
