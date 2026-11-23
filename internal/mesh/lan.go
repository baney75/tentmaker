package mesh

import (
	"errors"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// LAN is this device's place on its local network, as Wake-on-LAN needs
// it: the interface a magic packet would wake, its MAC, and the IPv4
// subnet and broadcast address a packet to it goes to.
type LAN struct {
	Interface string `json:"interface"`
	MAC       string `json:"mac"`       // lower case, colon separated
	Subnet    string `json:"subnet"`    // "192.168.1.0/24"
	Broadcast string `json:"broadcast"` // "192.168.1.255"
	Wired     bool   `json:"wired"`
	// Wakeable says the interface can wake the device: on Linux the
	// adapter's wakeup is enabled in sysfs; elsewhere it is assumed (see
	// Note), and mesh.wake = false opts out.
	Wakeable bool `json:"wakeable"`
	// Note says how Wakeable was decided, or why it is false.
	Note string `json:"note,omitempty"`
}

// netIface is one interface as ProbeLAN reads it, so tests can feed any.
type netIface struct {
	Name  string
	Flags net.Flags
	MAC   net.HardwareAddr
	Addrs []netip.Prefix
}

// ifaceWake is what the OS says about an interface: whether it is a
// physical adapter at all, wired or wireless, and whether it can wake the
// device.
type ifaceWake struct {
	Physical bool
	Wired    bool
	Wakeable bool
	Note     string
}

// virtualPrefixes are interface names that are never a LAN adapter:
// container bridges, VPNs and tunnels, VM host networks.
var virtualPrefixes = []string{
	"lo", "docker", "veth", "br-", "virbr", "podman", "cni", "flannel", "cali", "kube",
	"tailscale", "utun", "tun", "tap", "wg", "zt", "vmnet", "vboxnet", "awdl", "llw",
	"bridge", "anpi", "ap", "gif", "stf", "vethernet", "vmware", "virtualbox", "hyper-v",
	"bluetooth", "isatap", "teredo", "npcap",
}

func virtualName(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// ProbeLAN finds this device's LAN interface: up, broadcast capable, a
// globally assigned 6-byte MAC and a private IPv4 subnet. A wired adapter
// beats a wireless one, and one that can wake beats one that can't. It
// reads only the host's interface list and, on Linux, sysfs.
func ProbeLAN() (LAN, error) {
	ifs, err := hostIfaces()
	if err != nil {
		return LAN{}, err
	}
	return pickLAN(ifs, osWakeCheck)
}

func hostIfaces() ([]netIface, error) {
	sys, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]netIface, 0, len(sys))
	for _, s := range sys {
		ni := netIface{Name: s.Name, Flags: s.Flags, MAC: s.HardwareAddr}
		addrs, err := s.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if p, ok := a.(*net.IPNet); ok {
				ip, ok := netip.AddrFromSlice(p.IP)
				if !ok {
					continue
				}
				bits, _ := p.Mask.Size()
				ip = ip.Unmap()
				if ip.Is4() && bits > 0 && bits <= 32 {
					ni.Addrs = append(ni.Addrs, netip.PrefixFrom(ip, bits))
				}
			}
		}
		out = append(out, ni)
	}
	return out, nil
}

// errNoLAN says no interface qualified.
var errNoLAN = errors.New("no LAN interface: none is up with a private IPv4 subnet and a hardware MAC")

// pickLAN chooses the LAN interface among ifs; check asks the OS about one.
func pickLAN(ifs []netIface, check func(name string) ifaceWake) (LAN, error) {
	type cand struct {
		lan   LAN
		score int
	}
	var cands []cand
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagBroadcast == 0 ||
			i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if !usableMAC(i.MAC) || virtualName(i.Name) {
			continue
		}
		pfx, ok := lanPrefix(i.Addrs)
		if !ok {
			continue
		}
		w := check(i.Name)
		if !w.Physical {
			continue
		}
		lan := LAN{
			Interface: i.Name,
			MAC:       strings.ToLower(i.MAC.String()),
			Subnet:    pfx.Masked().String(),
			Broadcast: broadcastOf(pfx).String(),
			Wired:     w.Wired,
			Wakeable:  w.Wakeable,
			Note:      w.Note,
		}
		score := 0
		if w.Wired {
			score += 2
		}
		if w.Wakeable {
			score++
		}
		cands = append(cands, cand{lan, score})
	}
	if len(cands) == 0 {
		return LAN{}, errNoLAN
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].score != cands[b].score {
			return cands[a].score > cands[b].score
		}
		return cands[a].lan.Interface < cands[b].lan.Interface
	})
	return cands[0].lan, nil
}

// usableMAC is a 6-byte, globally administered unicast MAC. Locally
// administered ones are virtual adapters or randomized Wi-Fi addresses,
// and no adapter wakes on those.
func usableMAC(mac net.HardwareAddr) bool {
	if len(mac) != 6 {
		return false
	}
	if mac[0]&0x01 != 0 || mac[0]&0x02 != 0 {
		return false
	}
	for _, b := range mac {
		if b != 0 {
			return true
		}
	}
	return false
}

// lanPrefix is the first private IPv4 subnet of 2 to 2^24 addresses: a
// /8 to a /30. Anything wider or narrower is no LAN to broadcast on.
func lanPrefix(addrs []netip.Prefix) (netip.Prefix, bool) {
	for _, p := range addrs {
		a := p.Addr()
		if a.Is4() && a.IsPrivate() && p.Bits() >= 8 && p.Bits() <= 30 {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// broadcastOf is the subnet's broadcast address: every host bit set.
func broadcastOf(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// SameSubnet reports whether a and b name the same IPv4 subnet.
func SameSubnet(a, b string) bool {
	pa, err1 := netip.ParsePrefix(a)
	pb, err2 := netip.ParsePrefix(b)
	return err1 == nil && err2 == nil && pa.Masked() == pb.Masked()
}
