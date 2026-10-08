//go:build !darwin

package localclient

import (
	"errors"
	"net"
)

func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("native peer verification unavailable on this platform")
}
