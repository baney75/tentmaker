//go:build !windows

package linux

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testPolicy = NetPolicy{UID: 1234, Resolvers: []netip.Addr{
	StubResolver, netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("2001:db8::53"),
}}

func TestNftRules(t *testing.T) {
	want := `# Written by tentmaker setup; loaded at boot by tentmaker-net.service.
# CI jobs run as tentmaker-runner (uid 1234). They reach the internet, DNS and
# the source archive relay; never the tailnet, the LAN, link-local, this host's
# own addresses or the mesh port.
table inet tentmaker
delete table inet tentmaker
table inet tentmaker {
	chain output {
		type filter hook output priority filter; policy accept;
		meta skuid 1234 jump runner
	}
	chain runner {
		ip daddr 127.0.0.1 tcp dport 7434 accept comment "source archive relay"
		ip daddr { 127.0.0.53, 192.168.1.1 } udp dport 53 accept comment "DNS"
		ip daddr { 127.0.0.53, 192.168.1.1 } tcp dport 53 accept comment "DNS"
		ip6 daddr { 2001:db8::53 } udp dport 53 accept comment "DNS"
		ip6 daddr { 2001:db8::53 } tcp dport 53 accept comment "DNS"
		tcp dport 7433 reject comment "mesh port"
		udp dport 7433 reject comment "mesh port"
		oifname "lo" reject comment "this host"
		ip daddr 100.64.0.0/10 reject comment "tailnet"
		ip6 daddr fd7a:115c:a1e0::/48 reject comment "tailnet"
		ip daddr { 0.0.0.0/8, 127.0.0.0/8, 169.254.0.0/16 } reject comment "loopback, link-local"
		ip6 daddr { ::1/128, fc00::/7, fe80::/10 } reject comment "loopback, unique local, link-local"
		ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } reject comment "LAN"
	}
}
`
	if got := NftRules(testPolicy); got != want {
		t.Errorf("NftRules:\n%s\nwant:\n%s", got, want)
	}

	// allow_lan drops the RFC 1918 line and nothing else.
	lan := testPolicy
	lan.AllowLAN = true
	got := NftRules(lan)
	if got != strings.Replace(want, "\t\tip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } reject comment \"LAN\"\n", "", 1) {
		t.Errorf("NftRules with allow_lan:\n%s", got)
	}
}

func TestIptablesRules(t *testing.T) {
	want := `#!/bin/sh
# Written by tentmaker setup; loaded at boot by tentmaker-net.service. nft is missing,
# so this uses iptables. CI jobs run as tentmaker-runner (uid 1234). They reach the
# internet, DNS and the source archive relay; never the tailnet, the LAN,
# link-local, this host's own addresses or the mesh port.
set -eu
for t in '/usr/sbin/iptables' '/usr/sbin/ip6tables'; do
	"$t" -w 5 -N TENTMAKER 2>/dev/null || true
	"$t" -w 5 -F TENTMAKER
done
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 127.0.0.1/32 -p tcp --dport 7434 -j ACCEPT
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
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 100.64.0.0/10 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 0.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 127.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 169.254.0.0/16 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 10.0.0.0/8 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 172.16.0.0/12 -j REJECT
'/usr/sbin/iptables' -w 5 -A TENTMAKER -d 192.168.0.0/16 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d fd7a:115c:a1e0::/48 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d ::1/128 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d fc00::/7 -j REJECT
'/usr/sbin/ip6tables' -w 5 -A TENTMAKER -d fe80::/10 -j REJECT
for t in '/usr/sbin/iptables' '/usr/sbin/ip6tables'; do
	"$t" -w 5 -C OUTPUT -m owner --uid-owner 1234 -j TENTMAKER 2>/dev/null ||
		"$t" -w 5 -I OUTPUT 1 -m owner --uid-owner 1234 -j TENTMAKER
done
`
	if got := IptablesRules(testPolicy, "/usr/sbin/iptables", "/usr/sbin/ip6tables"); got != want {
		t.Errorf("IptablesRules:\n%s\nwant:\n%s", got, want)
	}
	lan := testPolicy
	lan.AllowLAN = true
	got := IptablesRules(lan, "/usr/sbin/iptables", "/usr/sbin/ip6tables")
	for _, r := range LANv4 {
		if strings.Contains(got, "-d "+r+" -j REJECT") {
			t.Errorf("allow_lan still rejects %s", r)
		}
	}
	for _, r := range []string{TailnetV4, TailnetV6, "169.254.0.0/16", "--dport 7433 -j REJECT", "-o lo -j REJECT"} {
		if !strings.Contains(got, r) {
			t.Errorf("allow_lan dropped %s", r)
		}
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

	// 100.100.100.100 (MagicDNS) is allowed only when it is the configured resolver.
	v := newEnv(t)
	if s := NftRules(v.e.policy(testUID)); strings.Contains(s, "100.100.100.100") || !strings.Contains(s, "{ 127.0.0.53 } udp dport 53 accept") {
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
	if string(b) != NftRules(NetPolicy{UID: testUID, Resolvers: []netip.Addr{StubResolver}}) {
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
	if string(b) != IptablesRules(NetPolicy{UID: testUID, Resolvers: []netip.Addr{StubResolver}}, v.o.Paths.Iptables, v.o.Paths.Ip6tables) {
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

	// Loaded, but more open than the config: allow_lan rules on a host
	// that wants the LAN blocked.
	lan := v.e.policy(testUID)
	lan.AllowLAN = true
	v.f.nft = NftRules(lan)
	if err := v.e.Start(ctx, spec("job-lan", "runner")); err == nil || !strings.Contains(err.Error(), "lack 10.0.0.0/8") {
		t.Fatalf("Start with LAN open: %v", err)
	}

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
