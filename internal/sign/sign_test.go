package sign

import (
	"errors"
	"os"
	"testing"
)

func TestHasTrue(t *testing.T) {
	const on = `<?xml version="1.0"?><plist version="1.0"><dict><key>com.apple.security.virtualization</key>
	<true/></dict></plist>`
	const off = `<dict><key>com.apple.security.virtualization</key><false/></dict>`
	const other = `<dict><key>com.apple.security.get-task-allow</key><true/></dict>`
	if !HasTrue(on, Virtualization) {
		t.Error("true entitlement not seen")
	}
	if HasTrue(off, Virtualization) {
		t.Error("false entitlement accepted")
	}
	if HasTrue(other, Virtualization) {
		t.Error("other entitlement accepted")
	}
}

// The test binary itself is ad-hoc signed by the toolchain without the
// Virtualization entitlement, so Check must refuse it.
func TestCheckRefusesUnentitledBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(self); !errors.Is(err, ErrMissing) {
		t.Fatalf("Check(test binary) = %v, want ErrMissing", err)
	}
}
