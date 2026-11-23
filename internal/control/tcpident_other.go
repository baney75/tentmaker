//go:build !linux && !windows

package control

import (
	"errors"
	"net"
)

// TCPIdentify can't name a TCP caller on this OS (macOS has no per-socket
// owner table Tentmaker reads without cgo), so the metrics listener
// refuses every caller here. Scrape this device from another tailnet
// device instead (mesh.viewers).
func TCPIdentify(string) Identify {
	return func(net.Conn) (Caller, error) {
		return Caller{}, errors.New("this OS can't name a TCP caller, so the metrics listener serves no one; scrape over the tailnet")
	}
}
