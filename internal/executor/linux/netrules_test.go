//go:build !windows

package linux

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var testPolicy = NetPolicy{UID: 1234, SubUIDs: []IDRange{{165536, 231071}}, Resolvers: []netip.Addr{
	StubResolver, netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("2001:db8::53"),
}, TailscaleIfaces: []string{"tailscale0", "ts-alt"}, OnLinkV6: []netip.Prefix{netip.MustParsePrefix("2001:db8:5::/64")}}

const testNft = `# Written by tentmaker setup; loaded at boot by tentmaker-net.service.
# CI jobs run as tentmaker-runner (uid 1234, rootless containers as its subordinate
# uids). They reach the internet, DNS and the source archive relay; never the
# tailnet, the LAN, multicast, link-local, this host's own addresses or the
# mesh port.
table inet tentmaker
delete table inet tentmaker
table inet tentmaker {
	chain output {
		type filter hook output priority filter; policy accept;
		meta skuid { 1234, 165536-231071 } jump runner
	}
	chain runner {
		ip daddr 127.0.0.1 tcp dport 7435 accept comment "source archive relay"
		ip daddr { 127.0.0.53, 192.168.1.1 } udp dport 53 accept comment "DNS"
		ip daddr { 127.0.0.53, 192.168.1.1 } tcp dport 53 accept comment "DNS"
		ip6 daddr { 2001:db8::53 } udp dport 53 accept comment "DNS"
		ip6 daddr { 2001:db8::53 } tcp dport 53 accept comment "DNS"
		tcp dport 7433 reject comment "mesh port"
		udp dport 7433 reject comment "mesh port"
		oifname "lo" reject comment "this host"
		ip daddr 100.64.0.0/10 reject comment "tailnet"
		ip6 daddr fd7a:115c:a1e0::/48 reject comment "tailnet"
		oifname { "tailscale0", "ts-alt" } ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 } reject comment "tailnet subnet routes"
		oifname { "tailscale0", "ts-alt" } ip6 daddr { fc00::/7 } reject comment "tailnet subnet routes"
		ip daddr { 0.0.0.0/8, 127.0.0.0/8, 169.254.0.0/16 } reject comment "loopback, link-local"
		ip6 daddr { ::1/128, fc00::/7, fe80::/10 } reject comment "loopback, unique local, link-local"
		ip daddr { 224.0.0.0/4, 255.255.255.255 } reject comment "multicast, broadcast"
		ip6 daddr { ff00::/8 } reject comment "multicast"
		ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } reject comment "LAN"
		ip6 daddr { 2001:db8:5::/64 } reject comment "LAN (on-link global IPv6)"
	}
}
`

func TestNftRules(t *testing.T) {
	if got := NftRules(testPolicy); got != testNft {
		t.Errorf("NftRules:\n%s\nwant:\n%s", got, testNft)
	}

	// allow_lan drops the RFC 1918 and on-link IPv6 lines and nothing else:
	// what tailscale0 routes (other devices' subnets) stays closed.
	lan := testPolicy
	lan.AllowLAN = true
	want := strings.Replace(testNft, "\t\tip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } reject comment \"LAN\"\n", "", 1)
	want = strings.Replace(want, "\t\tip6 daddr { 2001:db8:5::/64 } reject comment \"LAN (on-link global IPv6)\"\n", "", 1)
	if got := NftRules(lan); got != want {
		t.Errorf("NftRules with allow_lan:\n%s", got)
	}

	// No subordinate uids, no on-link prefix, no Tailscale interface found:
	// the bare uid in a set, no empty LAN set, tailscale0 still closed.
	bare := NftRules(NetPolicy{UID: 7})
	for _, s := range []string{"meta skuid { 7 } jump runner", `oifname { "tailscale0" } ip daddr`} {
		if !strings.Contains(bare, s) {
			t.Errorf("bare policy lacks %q:\n%s", s, bare)
		}
	}
	if strings.Contains(bare, "on-link") {
		t.Errorf("an empty on-link set:\n%s", bare)
	}
}

func TestIptablesRules(t *testing.T) {
	want := `#!/bin/sh
# Written by tentmaker setup; loaded at boot by tentmaker-net.service. nft is missing,
# so this uses iptables. CI jobs run as tentmaker-runner (uid 1234, rootless
# containers as its subordinate uids). They reach the internet, DNS and the
# source archive relay; never the tailnet, the LAN, multicast, link-local, this
# host's own addresses or the mesh port.
set -eu
for t in '/usr/sbin/iptables' '/usr/sbin/ip6tables'; do
	"$t" -w 5 -N TENTMAKER 2>/dev/null || true
	"$t" -w 5 -F TENTMAKER
done
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 127.0.0.1/32 -p tcp --dport 7435 -j ACCEPT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 127.0.0.53 -p udp --dport 53 -j ACCEPT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 127.0.0.53 -p tcp --dport 53 -j ACCEPT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 192.168.1.1 -p udp --dport 53 -j ACCEPT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 192.168.1.1 -p tcp --dport 53 -j ACCEPT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d 2001:db8::53 -p udp --dport 53 -j ACCEPT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d 2001:db8::53 -p tcp --dport 53 -j ACCEPT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -p tcp --dport 7433 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -p udp --dport 7433 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o lo -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -p tcp --dport 7433 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -p udp --dport 7433 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -o lo -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'tailscale0' -d 10.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'tailscale0' -d 172.16.0.0/12 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'tailscale0' -d 192.168.0.0/16 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'tailscale0' -d 100.64.0.0/10 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -o 'tailscale0' -d fc00::/7 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'ts-alt' -d 10.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'ts-alt' -d 172.16.0.0/12 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'ts-alt' -d 192.168.0.0/16 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -o 'ts-alt' -d 100.64.0.0/10 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -o 'ts-alt' -d fc00::/7 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 100.64.0.0/10 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 0.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 127.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 169.254.0.0/16 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 224.0.0.0/4 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 255.255.255.255 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 10.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 172.16.0.0/12 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 192.168.0.0/16 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d fd7a:115c:a1e0::/48 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d ::1/128 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d fc00::/7 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d fe80::/10 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d ff00::/8 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d 2001:db8:5::/64 -j REJECT
for t in '/usr/sbin/iptables' '/usr/sbin/ip6tables'; do
	for o in 1234 165536-231071; do
		"$t" -w 5 -C OUTPUT -m owner --uid-owner "$o" -j TENTMAKER 2>/dev/null ||
			"$t" -w 5 -I OUTPUT 1 -m owner --uid-owner "$o" -j TENTMAKER
	done
done
`
	if got := IptablesRules(testPolicy, "/usr/sbin/iptables", "/usr/sbin/ip6tables"); got != want {
		t.Errorf("IptablesRules:\n%s\nwant:\n%s", got, want)
	}
	lan := testPolicy
	lan.AllowLAN = true
	got := IptablesRules(lan, "/usr/sbin/iptables", "/usr/sbin/ip6tables")
	for _, r := range append(LANv4, "2001:db8:5::/64") {
		if strings.Contains(got, "-A TENTMAKER -d "+r+" -j REJECT") {
			t.Errorf("allow_lan still rejects %s", r)
		}
	}
	for _, r := range []string{TailnetV4, TailnetV6, "169.254.0.0/16", "--dport 7433 -j REJECT", "-o lo -j REJECT",
		"-o 'tailscale0' -d 192.168.0.0/16 -j REJECT", "-o 'ts-alt' -d fc00::/7 -j REJECT", "-d 224.0.0.0/4 -j REJECT"} {
		if !strings.Contains(got, r) {
			t.Errorf("allow_lan dropped %s", r)
		}
	}
}

func TestSubIDRanges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "subuid")
	must(t, os.WriteFile(p, []byte("alice:100000:65536\n"+User+":165536:65536\nbad\n"+User+":x:1\n"+User+":300000:0\n"+User+":400000:10\n"), 0o644))
	got := subIDRanges(p, User)
	if len(got) != 2 || got[0] != (IDRange{165536, 231071}) || got[1] != (IDRange{400000, 400009}) {
		t.Errorf("ranges %v", got)
	}
	if subIDRanges(filepath.Join(t.TempDir(), "missing"), User) != nil {
		t.Error("a missing subuid file gave ranges")
	}
}

// Lines in the kernel's own format (from a host with Tailscale, docker
// bridges and a LAN with a global IPv6 prefix).
const testIfInet6 = `00000000000000000000000000000001 01 80 10 80       lo
fe80000000000000fc5400fffeb51101 592 40 20 80    vnet7
fd7a115ca1e0000000000000d33b1729 3b 80 00 80 tailscale0
fd7a115ca1e0000000000000000000aa 3c 80 00 80   ts-alt
20010db8000500000000000000000002 02 40 00 80     eth0
`

const testIPv6Route = `fd7a115ca1e000000000000000000000 30 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000001 tailscale0
20010db8000900000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000001 tailscale0
fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0
20010db8000500000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000003 00000000 00040001     eth0
20010db8000500000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0
20010db8000700000000000000000000 30 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001  docker0
20010db8000600000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth1
20010db8000500000000000000000077 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000001 00000000 01000001     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000002 00000000 00450003     eth0
20010db8000800000000000000000000 40 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0
20010db8000a00000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00200201     eth0
20010db8000500000000000000000002 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000005 00000000 80200001     eth0
20010db8000500000000000000000000 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80100001     eth0
20010db8000500000000000000000099 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000001     eth0
20010db8000700010000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth1
00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000004 00000000 80200001       lo
20010db8000b00000000000000000007 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001     eth0
ff000000000000000000000000000000 08 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000003 00000000 00000001     eth0
`

func TestTailscaleIfacesAndOnLinkV6(t *testing.T) {
	dir := t.TempDir()
	ifs, routes := filepath.Join(dir, "if_inet6"), filepath.Join(dir, "ipv6_route")
	must(t, os.WriteFile(ifs, []byte(testIfInet6), 0o644))
	must(t, os.WriteFile(routes, []byte(testIPv6Route), 0o644))

	ts := tailscaleIfaces(ifs)
	if strings.Join(ts, " ") != "tailscale0 ts-alt" {
		t.Errorf("tailscale interfaces %v", ts)
	}
	if got := tailscaleIfaces(filepath.Join(dir, "missing")); strings.Join(got, " ") != TailscaleIface {
		t.Errorf("no if_inet6: %v", got)
	}

	// eth0's and eth1's prefixes and docker0's /48 are the LAN; not the
	// prefix tailscale0 routes, not a gateway route, a reject route, a
	// cached /128, the host's own addresses or the subnet anycast address
	// (the kernel lists them on eth0, not lo, even with no prefix route),
	// link-local or multicast. A
	// route inside another one is left out: nft merges it into the larger
	// prefix, and checkNet would then call the loaded rules stale forever.
	var got []string
	for _, p := range onLinkV6(routes, ts) {
		got = append(got, p.String())
	}
	if strings.Join(got, " ") != "2001:db8:5::/64 2001:db8:6::/64 2001:db8:7::/48" {
		t.Errorf("on-link prefixes %v", got)
	}
	if onLinkV6(filepath.Join(dir, "missing"), ts) != nil {
		t.Error("no ipv6_route gave prefixes")
	}
}

// A real listing from nft 1.1.7 of NftRules output: sets sorted, single
// addresses without a length, "reject with icmp ...".
const testNftListing = `table inet tentmaker {
	chain output {
		type filter hook output priority filter; policy accept;
		meta skuid { 1234, 165536-231071 } jump runner
	}

	chain runner {
		ip daddr 127.0.0.1 tcp dport 7435 accept comment "source archive relay"
		ip daddr { 127.0.0.53, 192.168.1.1 } udp dport 53 accept comment "DNS"
		ip daddr { 127.0.0.53, 192.168.1.1 } tcp dport 53 accept comment "DNS"
		ip6 daddr 2001:db8::53 udp dport 53 accept comment "DNS"
		ip6 daddr 2001:db8::53 tcp dport 53 accept comment "DNS"
		tcp dport 7433 reject comment "mesh port"
		udp dport 7433 reject comment "mesh port"
		oifname "lo" reject comment "this host"
		ip daddr 100.64.0.0/10 reject with icmp port-unreachable comment "tailnet"
		ip6 daddr fd7a:115c:a1e0::/48 reject with icmpv6 port-unreachable comment "tailnet"
		oifname { "tailscale0", "ts-alt" } ip daddr { 10.0.0.0/8, 100.64.0.0/10, 172.16.0.0/12, 192.168.0.0/16 } reject with icmp port-unreachable comment "tailnet subnet routes"
		oifname { "tailscale0", "ts-alt" } ip6 daddr fc00::/7 reject with icmpv6 port-unreachable comment "tailnet subnet routes"
		ip daddr { 0.0.0.0/8, 127.0.0.0/8, 169.254.0.0/16 } reject with icmp port-unreachable comment "loopback, link-local"
		ip6 daddr { ::1, fc00::/7, fe80::/10 } reject with icmpv6 port-unreachable comment "loopback, unique local, link-local"
		ip daddr { 224.0.0.0/4, 255.255.255.255 } reject with icmp port-unreachable comment "multicast, broadcast"
		ip6 daddr ff00::/8 reject with icmpv6 port-unreachable comment "multicast"
		ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } reject with icmp port-unreachable comment "LAN"
		ip6 daddr 2001:db8:5::/64 reject with icmpv6 port-unreachable comment "LAN (on-link global IPv6)"
	}
}
`

func TestCheckNft(t *testing.T) {
	for name, out := range map[string]string{"listing": testNftListing, "file": testNft} {
		if err := checkNft(out, testPolicy); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	stale := func(name string, p NetPolicy, out, want string) {
		t.Helper()
		err := checkNft(out, p)
		var se *errStaleNet
		if !errors.As(err, &se) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want stale with %q", name, err, want)
		}
	}
	p := testPolicy
	p.SubUIDs = append(slices.Clone(p.SubUIDs), IDRange{300000, 300009})
	stale("new subuid range", p, testNftListing, "uids 300000-300009")
	p = testPolicy
	p.OnLinkV6 = append(slices.Clone(p.OnLinkV6), netip.MustParsePrefix("2001:db8:6::/64"))
	stale("new on-link prefix", p, testNftListing, "2001:db8:6::/64")
	p = testPolicy
	p.TailscaleIfaces = []string{"tailscale0", "ts-new"}
	stale("new Tailscale interface", p, testNftListing, `"ts-new"`)
	// Rules from before multicast and subnet routes were blocked.
	old := strings.Replace(testNftListing, "224.0.0.0/4, ", "", 1)
	stale("older rules", testPolicy, old, "224.0.0.0/4")
	// Rules from before the relay moved off the control API's port 7434.
	oldRelay := strings.Replace(testNftListing, "tcp dport 7435 accept", "tcp dport 7434 accept", 1)
	stale("old relay port", testPolicy, oldRelay, "7435 in the \"source archive relay\" rule")
	lan := testPolicy
	lan.AllowLAN = true
	stale("allow_lan rules on a strict host", testPolicy, NftRules(lan), "10.0.0.0/8")

	// No jump for the runner: not stale, refused.
	for name, out := range map[string]string{
		"no jump":   strings.Replace(testNftListing, "jump runner", "accept", 1),
		"other uid": strings.Replace(testNftListing, "{ 1234, ", "{ 999, ", 1),
	} {
		err := checkNft(out, testPolicy)
		var se *errStaleNet
		if err == nil || errors.As(err, &se) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	// nft may spell the runner's uid as its name.
	if err := checkNft(strings.Replace(testNftListing, "{ 1234, ", `{ "`+User+`", `, 1), testPolicy); err != nil {
		t.Errorf("uid by name: %v", err)
	}
}

func TestReadResolvers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	must(t, os.WriteFile(p, []byte("# x\nnameserver 100.100.100.100\nnameserver fe80::1%eth0\nnameserver ::ffff:10.0.0.2\nsearch tail.ts.net\nnameserver bogus\nnameserver 2001:db8::1\n"), 0o644))
	var got []string
	for _, a := range readResolvers(p) {
		got = append(got, a.String())
	}
	if strings.Join(got, " ") != "100.100.100.100 10.0.0.2 2001:db8::1" {
		t.Errorf("resolvers %v", got)
	}
	if readResolvers(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Error("a missing resolv.conf gave resolvers")
	}

	// 100.100.100.100 (MagicDNS) is allowed only when it is the configured
	// resolver. Behind the stub, the upstream servers jobs query from their
	// own namespace are allowed (resolved.conf's, not its loopback one).
	v := newEnv(t)
	if s := NftRules(v.e.policy(testUID)); strings.Contains(s, "100.100.100.100") || !strings.Contains(s, "{ 127.0.0.53, 192.0.2.53 } udp dport 53 accept") {
		t.Errorf("default policy DNS:\n%s", s)
	}
	must(t, os.WriteFile(v.o.Paths.ResolvConf, []byte("nameserver 100.100.100.100\nnameserver 127.0.0.53\n"), 0o644))
	if s := NftRules(v.e.policy(testUID)); !strings.Contains(s, "ip daddr { 127.0.0.53, 100.100.100.100 } udp dport 53 accept") {
		t.Errorf("MagicDNS resolver missing:\n%s", s)
	}
}

func TestInstallLoadsNetRules(t *testing.T) {
	v := newEnv(t)
	v.hostFixture(t)
	ctx := context.Background()
	must(t, Install(ctx, v.o))
	nft := v.o.Paths.Nft
	file := filepath.Join(v.dir, "lib", "net.nft")
	b, err := os.ReadFile(file)
	must(t, err)
	// The runner's subordinate uids from the subuid file install wrote.
	if string(b) != NftRules(NetPolicy{UID: testUID, SubUIDs: []IDRange{{165536, 231071}}, Resolvers: []netip.Addr{StubResolver, netip.MustParseAddr("192.0.2.53")}}) {
		t.Errorf("net.nft:\n%s", b)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o644 || v.f.chowns[file] != [2]int{0, 0} {
		t.Errorf("net.nft mode %o owner %v", st.Mode().Perm(), v.f.chowns[file])
	}
	unit, err := os.ReadFile(filepath.Join(v.dir, "systemd", NetUnit))
	must(t, err)
	if string(unit) != NetUnitFile(nft+" -f "+file) || !strings.Contains(string(unit), "ExecStart="+nft+" -f "+file+"\n") ||
		!strings.Contains(string(unit), "Before=network-pre.target tentmaker.service") {
		t.Errorf("unit:\n%s", unit)
	}
	cmds := strings.Join(v.f.commands(), "\n")
	for _, s := range []string{nft + " -f " + file, "systemctl enable " + NetUnit, nft + " list table inet tentmaker"} {
		if !strings.Contains(cmds, s) {
			t.Errorf("Install did not run %q", s)
		}
	}
	if _, err := os.Stat(filepath.Join(v.dir, "lib", "net-iptables.sh")); err == nil {
		t.Error("an iptables script next to the nft rules")
	}

	// Rerun: the same file, loaded again, no duplicate state.
	n := len(v.f.cmds)
	must(t, Install(ctx, v.o))
	b2, _ := os.ReadFile(file)
	if string(b2) != string(b) {
		t.Error("rerun changed the rules")
	}
	if !strings.Contains(strings.Join(v.f.commands()[n:], "\n"), nft+" -f "+file) {
		t.Error("rerun did not reload the rules")
	}
}

func TestInstallFallsBackToIptables(t *testing.T) {
	v := newEnv(t)
	v.hostFixture(t)
	must(t, os.Remove(v.o.Paths.Nft))
	ctx := context.Background()
	must(t, Install(ctx, v.o))
	file := filepath.Join(v.dir, "lib", "net-iptables.sh")
	b, err := os.ReadFile(file)
	must(t, err)
	if string(b) != IptablesRules(NetPolicy{UID: testUID, SubUIDs: []IDRange{{165536, 231071}}, Resolvers: []netip.Addr{StubResolver, netip.MustParseAddr("192.0.2.53")}}, v.o.Paths.Iptables, v.o.Paths.Ip6tables) {
		t.Errorf("net-iptables.sh:\n%s", b)
	}
	unit, _ := os.ReadFile(filepath.Join(v.dir, "systemd", NetUnit))
	if !strings.Contains(string(unit), "ExecStart=/bin/sh "+file+"\n") {
		t.Errorf("unit:\n%s", unit)
	}
	must(t, os.MkdirAll(filepath.Join(v.dir, "run-user", "1234", "podman"), 0o700))
	must(t, os.WriteFile(filepath.Join(v.dir, "run-user", "1234", "podman", "podman.sock"), nil, 0o600))
	if ps := New(v.o).Preflight(ctx); len(ps) != 0 {
		t.Fatalf("Preflight with iptables rules: %+v", ps)
	}

	must(t, Uninstall(ctx, v.o, false))
	cmds := strings.Join(v.f.commands(), "\n")
	for _, s := range []string{
		v.o.Paths.Iptables + " -w 5 -D OUTPUT -m owner --uid-owner 1234 -j TENTMAKER",
		v.o.Paths.Ip6tables + " -w 5 -D OUTPUT -m owner --uid-owner 1234 -j TENTMAKER",
		v.o.Paths.Iptables + " -w 5 -D OUTPUT -m owner --uid-owner 165536-231071 -j TENTMAKER",
		v.o.Paths.Ip6tables + " -w 5 -D OUTPUT -m owner --uid-owner 165536-231071 -j TENTMAKER",
		v.o.Paths.Iptables + " -w 5 -X TENTMAKER",
		v.o.Paths.Ip6tables + " -w 5 -X TENTMAKER",
	} {
		if !strings.Contains(cmds, s) {
			t.Errorf("Uninstall did not run %q", s)
		}
	}
	if _, err := os.Stat(filepath.Join(v.dir, "systemd", NetUnit)); err == nil {
		t.Error("the boot unit survived uninstall")
	}
}

func TestInstallRefusesWithoutAFirewall(t *testing.T) {
	v := newEnv(t)
	v.hostFixture(t)
	for _, p := range []string{v.o.Paths.Nft, v.o.Paths.Ip6tables} {
		must(t, os.Remove(p))
	}
	err := Install(context.Background(), v.o)
	if err == nil || !strings.Contains(err.Error(), "neither nft nor iptables") {
		t.Fatalf("Install without nft or ip6tables: %v", err)
	}
	if _, err := os.Stat(filepath.Join(v.dir, "systemd", NetUnit)); err == nil {
		t.Error("a boot unit without rules")
	}
}

func TestUninstallRemovesNetRules(t *testing.T) {
	v := newEnv(t)
	v.hostFixture(t)
	ctx := context.Background()
	must(t, Install(ctx, v.o))
	must(t, Uninstall(ctx, v.o, false))
	if v.f.nft != "" {
		t.Error("the nft table is still loaded")
	}
	if !strings.Contains(strings.Join(v.f.commands(), "\n"), "systemctl disable "+NetUnit) {
		t.Error("the boot unit was not disabled")
	}
	for _, p := range []string{"systemd/" + NetUnit, "lib/net.nft"} {
		if _, err := os.Stat(filepath.Join(v.dir, p)); err == nil {
			t.Errorf("%s survived uninstall", p)
		}
	}
	// Rerun with nothing left: no error, nothing deleted twice.
	n := len(v.f.cmds)
	must(t, Uninstall(ctx, v.o, false))
	if strings.Contains(strings.Join(v.f.commands()[n:], "\n"), "delete table") {
		t.Error("uninstall deleted a table that was not there")
	}
}

func TestNoNetRulesNoJob(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()

	// Unloaded (a reboot without the unit, someone flushed the ruleset).
	v.f.nft = ""
	err := v.e.Start(ctx, spec("job-nonet", "runner"))
	if err == nil || !strings.Contains(err.Error(), "network rules for CI jobs are not loaded") {
		t.Fatalf("Start without rules: %v", err)
	}
	for _, c := range v.f.commands() {
		if strings.HasPrefix(c, "systemd-run") {
			t.Fatal("a job started without network rules")
		}
	}
	found := false
	for _, p := range v.e.Preflight(ctx) {
		if p.Check == "network" && strings.Contains(p.What, "not loaded") {
			found = true
		}
	}
	if !found {
		t.Error("Preflight did not report the missing rules")
	}

	// Loaded, but more open than the config (allow_lan rules on a host
	// that wants the LAN blocked): Start rewrites and reloads them, then
	// runs the job.
	lan := v.e.policy(testUID)
	lan.AllowLAN = true
	v.f.nft = NftRules(lan)
	must(t, v.e.Start(ctx, spec("job-lan", "runner")))
	if v.f.nft != NftRules(v.e.policy(testUID)) {
		t.Errorf("Start did not reload the strict rules:\n%s", v.f.nft)
	}

	// A reload that fails refuses the job.
	v.f.nft = NftRules(lan)
	v.f.nftFail = true
	if err := v.e.Start(ctx, spec("job-lan2", "runner")); err == nil || !strings.Contains(err.Error(), "load the network rules") {
		t.Fatalf("Start with a failed reload: %v", err)
	}
	v.f.nftFail = false

	// Never installed at all.
	must(t, os.Remove(v.e.nftRules()))
	if err := v.e.Start(ctx, spec("job-none", "runner")); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("Start with no rules file: %v", err)
	}

	// The stricter rules pass for a host with allow_lan.
	v.f.nft = NftRules(v.e.policy(testUID))
	must(t, os.WriteFile(v.e.nftRules(), []byte(v.f.nft), 0o644))
	o := v.o
	o.AllowLAN = true
	if err := New(o).checkNet(ctx, testUID); err != nil {
		t.Errorf("strict rules on an allow_lan host: %v", err)
	}
}

// The host changes under loaded rules: a new subordinate uid range (setup
// for another user, or usermod by hand), a new LAN prefix (a router
// advertised one) and tailscaled on a second interface. Start reloads the
// rules with them before the job runs; doctor reports them until then.
func TestStartReloadsStaleNetRules(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	must(t, os.WriteFile(v.o.Paths.Subuid, []byte(User+":165536:65536\n"), 0o644))
	must(t, os.MkdirAll(filepath.Dir(v.o.Paths.IPv6Route), 0o755))
	must(t, os.WriteFile(v.o.Paths.IfInet6, []byte(testIfInet6), 0o644))
	must(t, os.WriteFile(v.o.Paths.IPv6Route, []byte(testIPv6Route), 0o644))

	found := false
	for _, p := range v.e.Preflight(ctx) {
		if p.Check == "network" && strings.Contains(p.What, "out of date") && strings.Contains(p.What, "165536-231071") {
			found = true
		}
	}
	if !found {
		t.Errorf("Preflight did not report stale rules: %+v", v.e.Preflight(ctx))
	}

	must(t, v.e.Start(ctx, spec("job-stale", "runner")))
	for _, s := range []string{"meta skuid { 1234, 165536-231071 } jump runner", `oifname { "tailscale0", "ts-alt" }`,
		"ip6 daddr { 2001:db8:5::/64, 2001:db8:6::/64, 2001:db8:7::/48 } reject"} {
		if !strings.Contains(v.f.nft, s) {
			t.Errorf("reloaded rules lack %q:\n%s", s, v.f.nft)
		}
	}
	if b, _ := os.ReadFile(v.e.nftRules()); string(b) != v.f.nft {
		t.Error("the rules file on disk is not what was loaded, so a reboot loads the old rules")
	}
	if !strings.Contains(v.logs.String(), "reloading the network rules") {
		t.Error("the reload was not logged")
	}
	if err := v.e.checkNet(ctx, testUID); err != nil {
		t.Errorf("after the reload: %v", err)
	}
}

func TestUserManagerHidesTailscale(t *testing.T) {
	for _, s := range []string{"After=tailscaled.service", "Slice=tentmaker.slice",
		"InaccessiblePaths=-/run/tailscale\n", "InaccessiblePaths=-/var/run/tailscale\n", "InaccessiblePaths=-/var/lib/tailscale\n"} {
		if !strings.Contains(userManagerDropIn, s) {
			t.Errorf("user manager drop-in lacks %q:\n%s", s, userManagerDropIn)
		}
	}
}

// The host's DNS server changes under loaded rules (a laptop joins a new
// network and DHCP hands it a new resolver). The old allow-list would send
// a job's lookups to a server the LAN rule rejects and keep the old server
// open, so Start reloads the rules and the job's resolv.conf with the new
// one, on either backend.
func TestStartReloadsOnNewResolver(t *testing.T) {
	for _, be := range []string{"nft", "iptables"} {
		t.Run(be, func(t *testing.T) {
			v := newEnv(t)
			v.installed(t)
			ctx := context.Background()
			loaded := func() string {
				v.f.mu.Lock()
				defer v.f.mu.Unlock()
				if be == "nft" {
					return v.f.nft
				}
				return v.f.ipt
			}
			if be == "iptables" {
				must(t, os.Remove(v.o.Paths.Nft))
				must(t, v.e.installNet(ctx, testUID))
			}
			must(t, v.e.checkNet(ctx, testUID))
			if !strings.Contains(loaded(), "192.0.2.53") {
				t.Fatalf("the first rules lack the first resolver:\n%s", loaded())
			}

			must(t, os.WriteFile(v.o.Paths.ResolvedConf, []byte("nameserver 10.0.0.1\n"), 0o644))
			var se *errStaleNet
			if err := v.e.checkNet(ctx, testUID); !errors.As(err, &se) || !strings.Contains(err.Error(), "10.0.0.1") {
				t.Fatalf("checkNet after the resolver changed: %v, want stale naming 10.0.0.1", err)
			}
			must(t, v.e.Start(ctx, spec("job-dns", "")))
			if r := loaded(); !strings.Contains(r, "10.0.0.1") || strings.Contains(r, "192.0.2.53") {
				t.Errorf("reloaded rules do not allow exactly the new resolver:\n%s", r)
			}
			b, err := os.ReadFile(v.e.jobResolvConf())
			must(t, err)
			if string(b) != "# Written by tentmaker for CI jobs.\nnameserver 10.0.0.1\n" {
				t.Errorf("job resolv.conf:\n%s", b)
			}
			must(t, v.e.checkNet(ctx, testUID))
		})
	}
}
