package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// WakePort is the UDP port magic packets go to (the discard port).
const WakePort = 9

// sendTimeout bounds one magic packet send.
const sendTimeout = 3 * time.Second

// Sender sends one Wake-on-LAN magic packet. Tests use a fake; only
// UDPSender opens a socket.
type Sender interface {
	Send(ctx context.Context, mac net.HardwareAddr, broadcast netip.Addr) error
}

// MagicPacket is 6 bytes of 0xFF, then mac 16 times: 102 bytes.
func MagicPacket(mac net.HardwareAddr) ([]byte, error) {
	if len(mac) != 6 {
		return nil, errors.New("a magic packet needs a 6-byte MAC")
	}
	p := make([]byte, 0, 6+16*6)
	for range 6 {
		p = append(p, 0xFF)
	}
	for range 16 {
		p = append(p, mac...)
	}
	return p, nil
}

// UDPSender sends the packet over UDP to the subnet broadcast address,
// port WakePort, from a socket with SO_BROADCAST set.
type UDPSender struct{}

// Send implements Sender.
func (UDPSender) Send(ctx context.Context, mac net.HardwareAddr, broadcast netip.Addr) error {
	pkt, err := MagicPacket(mac)
	if err != nil {
		return err
	}
	if !broadcast.Is4() {
		return errors.New("the broadcast address must be IPv4")
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	lc := net.ListenConfig{Control: setBroadcast}
	conn, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("open a UDP socket: %w", err)
	}
	defer conn.Close()
	dl, _ := ctx.Deadline()
	_ = conn.SetWriteDeadline(dl)
	if _, err := conn.WriteTo(pkt, net.UDPAddrFromAddrPort(netip.AddrPortFrom(broadcast, WakePort))); err != nil {
		return fmt.Errorf("send the magic packet: %w", err)
	}
	return nil
}
