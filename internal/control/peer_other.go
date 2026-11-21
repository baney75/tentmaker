//go:build !windows && !linux && !darwin

package control

import (
	"errors"
	"net"
)

// peerUID has no implementation here, so every caller is refused.
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("this OS can't name a control API caller")
}
