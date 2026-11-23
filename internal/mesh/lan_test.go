package mesh

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPickLANPrefersWiredWakeable(t *testing.T) {
	up := net.FlagUp | net.FlagBroadcast
	ifs := []netIface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, MAC: nil, Addrs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
		{Name: "docker0", Flags: up, MAC: mustMAC(t, "02:42:ac:11:00:01"), Addrs: []netip.Prefix{netip.MustParsePrefix("172.17.0.1/16")}},
		{Name: "tailscale0", Flags: net.FlagUp | net.FlagPointToPoint, Addrs: []netip.Prefix{netip.MustParsePrefix("100.64.0.1/32")}},
		{Name: "wlp2s0", Flags: up, MAC: mustMAC(t, "a4:5e:60:00:00:02"), Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.40/24")}},
		{Name: "enp3s0", Flags: up, MAC: mustMAC(t, "A4:5E:60:00:00:01"), Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.41/24")}},
		{Name: "enp4s0", Flags: up, MAC: mustMAC(t, "a4:5e:60:00:00:03"), Addrs: []netip.Prefix{netip.MustParsePrefix("8.8.8.8/24")}},
		{Name: "eth9", Flags: net.FlagBroadcast, MAC: mustMAC(t, "a4:5e:60:00:00:04"), Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/8")}},
		{Name: "eth8", Flags: up, MAC: mustMAC(t, "a6:5e:60:00:00:05"), Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.3/8")}}, // locally administered
	}
	checks := map[string]ifaceWake{
		"wlp2s0":  {Physical: true, Wired: false, Wakeable: true},
		"enp3s0":  {Physical: true, Wired: true, Wakeable: true, Note: "sysfs"},
		"enp4s0":  {Physical: true, Wired: true, Wakeable: true},
		"eth9":    {Physical: true, Wired: true, Wakeable: true},
		"eth8":    {Physical: true, Wired: true, Wakeable: true},
		"docker0": {Physical: true, Wired: true, Wakeable: true},
	}
	lan, err := pickLAN(ifs, func(n string) ifaceWake { return checks[n] })
	if err != nil {
		t.Fatal(err)
	}
	want := LAN{Interface: "enp3s0", MAC: "a4:5e:60:00:00:01", Subnet: "192.168.1.0/24", Broadcast: "192.168.1.255", Wired: true, Wakeable: true, Note: "sysfs"}
	if lan != want {
		t.Fatalf("lan %+v, want %+v", lan, want)
	}

	// Wired but can't wake still beats wireless: a wired adapter is where
	// WoL can be turned on, and status says how.
	checks["enp3s0"] = ifaceWake{Physical: true, Wired: true, Note: "wakeup is disabled"}
	lan, _ = pickLAN(ifs, func(n string) ifaceWake { return checks[n] })
	if lan.Interface != "enp3s0" || lan.Wakeable {
		t.Errorf("wired, not wakeable: %+v", lan)
	}
	// Not physical (no device link in sysfs): skipped.
	checks["enp3s0"] = ifaceWake{}
	lan, _ = pickLAN(ifs, func(n string) ifaceWake { return checks[n] })
	if lan.Interface != "wlp2s0" || lan.Wired {
		t.Errorf("only wireless left: %+v", lan)
	}
	if _, err := pickLAN(ifs[:3], func(string) ifaceWake { return ifaceWake{Physical: true} }); err == nil {
		t.Error("no LAN interface, no error")
	}
}

func TestBroadcastOf(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.41/24": "192.168.1.255",
		"10.1.2.3/8":      "10.255.255.255",
		"172.16.5.9/22":   "172.16.7.255",
		"192.168.0.5/30":  "192.168.0.7",
	} {
		if got := broadcastOf(netip.MustParsePrefix(in)).String(); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if !SameSubnet("192.168.1.0/24", "192.168.1.77/24") || SameSubnet("192.168.1.0/24", "192.168.2.0/24") ||
		SameSubnet("192.168.1.0/24", "192.168.1.0/23") || SameSubnet("", "") {
		t.Error("SameSubnet")
	}
}

func TestMagicPacket(t *testing.T) {
	mac := mustMAC(t, "a4:5e:60:00:00:01")
	p, err := MagicPacket(mac)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 102 || !bytes.Equal(p[:6], bytes.Repeat([]byte{0xFF}, 6)) {
		t.Fatalf("header: % x", p[:6])
	}
	for i := 0; i < 16; i++ {
		if !bytes.Equal(p[6+i*6:12+i*6], mac) {
			t.Fatalf("repeat %d: % x", i, p[6+i*6:12+i*6])
		}
	}
	if _, err := MagicPacket(net.HardwareAddr{1, 2, 3}); err == nil {
		t.Error("short MAC accepted")
	}
}
