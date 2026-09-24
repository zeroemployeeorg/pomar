// Package sign checks, before Pomar runs a VM-owning binary, that the binary
// carries the Virtualization entitlement. `swift test` relinks the host
// binary with a default signature that lacks it, so the check reads the
// binary as it is at the moment of use rather than trusting the build.
package sign

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Virtualization is the entitlement a VM-owning process needs.
const Virtualization = "com.apple.security.virtualization"

// ErrMissing is returned when the entitlement is absent or not true.
var ErrMissing = errors.New("sign: binary lacks the " + Virtualization + " entitlement; run `make swift-sign`")

// Check reads bin's entitlements with codesign and requires Virtualization.
func Check(bin string) error {
	out, err := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", bin).Output()
	if err != nil {
		return fmt.Errorf("%w (codesign: %v)", ErrMissing, err)
	}
	if !HasTrue(string(out), Virtualization) {
		return ErrMissing
	}
	return nil
}

// HasTrue reports whether an entitlements plist sets key to true.
func HasTrue(plist, key string) bool {
	i := strings.Index(plist, "<key>"+key+"</key>")
	if i < 0 {
		return false
	}
	rest := strings.TrimLeft(plist[i+len("<key>"+key+"</key>"):], " \t\r\n")
	return strings.HasPrefix(rest, "<true/>")
}
