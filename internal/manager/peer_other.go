//go:build !darwin

package manager

import (
	"errors"
	"net"
)

// peerUID is not implemented off macOS, where Pomar runs; a caller whose
// identity cannot be read is refused (fail closed).
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("manager: peer credentials are read on macOS only")
}
