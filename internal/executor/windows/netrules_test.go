package windows

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

func TestFirewallScript(t *testing.T) {
	p := NetPolicy{SID: testSID, Resolvers: []netip.Addr{
		netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("100.100.100.100"),
	}, Self: []netip.Addr{netip.MustParseAddr("192.168.1.20"), netip.MustParseAddr("2001:db8:1::20"), netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")}}
	want := `# tentmaker: install network rules
Get-NetFirewallRule -Group 'Tentmaker' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
$u = 'D:(A;;CC;;;S-1-5-21-1-2-3-1001)'
$d = 'Written by tentmaker setup (net-v2, user S-1-5-21-1-2-3-1001, allow_lan=false). CI jobs never reach the tailnet, the LAN, this host, loopback, link-local or the mesh port.'
New-NetFirewallRule -Name 'Tentmaker-Block-Mesh-TCP' -DisplayName 'Tentmaker: CI jobs never reach the mesh port (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemotePort @('7433') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Mesh-UDP' -DisplayName 'Tentmaker: CI jobs never reach the mesh port (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemotePort @('7433') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Ranges' -DisplayName 'Tentmaker: CI jobs never reach the tailnet, the LAN, link-local or loopback' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -RemoteAddress @('100.64.0.0-100.100.100.99', '100.100.100.101-100.127.255.255', 'fd7a:115c:a1e0::/48', '0.0.0.0/8', '127.0.0.0', '127.0.0.2-127.255.255.255', '169.254.0.0/16', 'fc00::/7', 'fe80::/10', '10.0.0.0/8', '172.16.0.0/12', '192.168.0.0-192.168.1.0', '192.168.1.2-192.168.255.255') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Self' -DisplayName 'Tentmaker: CI jobs never reach this host''s own addresses' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -RemoteAddress @('2001:db8:1::20') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Host0-TCP' -DisplayName 'Tentmaker: CI jobs reach 127.0.0.1 only on allowed ports (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('127.0.0.1') -RemotePort @('1-7433', '7435-65535') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Host0-UDP' -DisplayName 'Tentmaker: CI jobs reach 127.0.0.1 only on allowed ports (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('127.0.0.1') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Host1-TCP' -DisplayName 'Tentmaker: CI jobs reach 192.168.1.1 only on allowed ports (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('192.168.1.1') -RemotePort @('1-52', '54-65535') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Host1-UDP' -DisplayName 'Tentmaker: CI jobs reach 192.168.1.1 only on allowed ports (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('192.168.1.1') -RemotePort @('1-52', '54-65535') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Host2-TCP' -DisplayName 'Tentmaker: CI jobs reach 100.100.100.100 only on allowed ports (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('100.100.100.100') -RemotePort @('1-52', '54-65535') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Block-Host2-UDP' -DisplayName 'Tentmaker: CI jobs reach 100.100.100.100 only on allowed ports (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('100.100.100.100') -RemotePort @('1-52', '54-65535') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Allow-Relay' -DisplayName 'Tentmaker: CI jobs reach the source archive relay' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Allow -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('127.0.0.1') -RemotePort @('7434') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Allow-DNS-UDP' -DisplayName 'Tentmaker: CI jobs reach the DNS servers (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Allow -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('168.63.129.16', '192.168.1.1', '100.100.100.100') -RemotePort @('53') | Out-Null
New-NetFirewallRule -Name 'Tentmaker-Allow-DNS-TCP' -DisplayName 'Tentmaker: CI jobs reach the DNS servers (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Allow -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('168.63.129.16', '192.168.1.1', '100.100.100.100') -RemotePort @('53') | Out-Null
`
	if got := FirewallScript(p); got != want {
		t.Errorf("FirewallScript:\n%s\nwant:\n%s", got, want)
	}

	// allow_lan: the RFC 1918 ranges go, nothing else does, the LAN
	// resolver needs no hole, and the host's own LAN address joins its
	// global one in the self rule.
	p.AllowLAN = true
	got := FirewallScript(p)
	for _, r := range []string{"'10.0.0.0/8'", "'172.16.0.0/12'", "192.168.0.0", "Host1-TCP' -DisplayName 'Tentmaker: CI jobs reach 192.168.1.1"} {
		if strings.Contains(got, r) {
			t.Errorf("allow_lan still has %s", r)
		}
	}
	for _, r := range []string{"'100.64.0.0-100.100.100.99'", "'fd7a:115c:a1e0::/48'", "'169.254.0.0/16'", "'fc00::/7'", "-RemotePort @('7433')", "allow_lan=true",
		"-RemoteAddress @('192.168.1.20', '2001:db8:1::20')"} {
		if !strings.Contains(got, r) {
			t.Errorf("allow_lan dropped %s", r)
		}
	}
}

// New-NetFirewallRule refuses the whole rule when -RemoteAddress names an
// IPv6 loopback, unspecified or multicast address ("An unspecified,
// multicast, broadcast, or loopback IPv6 address was specified"), which
// failed setup on every Windows host.
func TestFirewallRulesUseAddressesWindowsAccepts(t *testing.T) {
	p := NetPolicy{SID: testSID, Resolvers: []netip.Addr{
		netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("::1"), netip.MustParseAddr("fd7a:115c:a1e0::53"),
	}}
	for _, lan := range []bool{false, true} {
		p.AllowLAN = lan
		for _, r := range FirewallRules(p) {
			for _, a := range r.RemoteAddress {
				for _, end := range strings.Split(a, "-") {
					addr, err := netip.ParseAddr(end)
					if err != nil {
						pf, perr := netip.ParsePrefix(end)
						if perr != nil {
							t.Fatalf("%s: %q is not an address, prefix or range", r.Name, a)
						}
						addr = pf.Addr()
					}
					if addr.Is6() && (addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast()) {
						t.Errorf("%s (allow_lan=%v) names %s, which Windows Firewall refuses", r.Name, lan, a)
					}
				}
			}
		}
	}
}

func TestCarve(t *testing.T) {
	for _, c := range []struct {
		prefix string
		holes  []string
		want   string
	}{
		{"10.0.0.0/8", nil, "10.0.0.0/8"},
		{"10.0.0.0/8", []string{"192.168.1.1"}, "10.0.0.0/8"},
		{"10.0.0.0/8", []string{"10.0.0.0"}, "10.0.0.1-10.255.255.255"},
		{"10.0.0.0/8", []string{"10.255.255.255", "10.0.0.5", "10.0.0.5"}, "10.0.0.0-10.0.0.4 10.0.0.6-10.255.255.254"},
		{"fc00::/7", []string{"fd00::53"}, "fc00::-fd00::52 fd00::54-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
		{"::1/128", []string{"::1"}, ""},
	} {
		var hs []netip.Addr
		for _, h := range c.holes {
			hs = append(hs, netip.MustParseAddr(h))
		}
		if got := strings.Join(carve(netip.MustParsePrefix(c.prefix), hs), " "); got != c.want {
			t.Errorf("carve(%s, %v) = %q, want %q", c.prefix, c.holes, got, c.want)
		}
	}
	if got := strings.Join(otherPorts([]uint16{53, 7434}), ","); got != "1-52,54-7433,7435-65535" {
		t.Errorf("otherPorts = %s", got)
	}
	if got := otherPorts([]uint16{1, 65535}); strings.Join(got, ",") != "2-65534" {
		t.Errorf("otherPorts edges = %v", got)
	}
}

func TestInstallAddsFirewallRules(t *testing.T) {
	v := newEnv(t)
	v.install()
	p := NetPolicy{SID: testSID, Resolvers: []netip.Addr{netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("192.168.1.1")},
		Self: []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("192.168.1.20"), netip.MustParseAddr("2001:db8:1::20")}}
	var installs []string
	for _, s := range v.api.scripts {
		if strings.HasPrefix(s, "# tentmaker: install network rules\n") {
			installs = append(installs, s)
		}
	}
	if len(installs) != 1 || installs[0] != FirewallScript(p) {
		t.Fatalf("install scripts %d:\n%s", len(installs), strings.Join(installs, "\n"))
	}
	if len(v.api.fw) != len(FirewallRules(p)) {
		t.Fatalf("rules after install: %v", v.api.fw)
	}
	if strings.Join(v.api.self, " ") != "2001:db8:1::20" {
		t.Fatalf("self rule after install: %v", v.api.self)
	}
	for _, s := range v.api.scripts {
		if strings.Contains(s, "Tm1!") || strings.Contains(s, string(v.api.password)) && len(v.api.password) > 0 {
			t.Fatal("a firewall script holds the runner password")
		}
	}
	// Rerun: the group is emptied first, so the same rules, no duplicates.
	before := len(v.api.fw)
	v.install()
	if len(v.api.fw) != before {
		t.Fatalf("rerun changed the rule count: %d -> %d", before, len(v.api.fw))
	}
	if ps := v.e.Preflight(context.Background()); len(ps) != 0 {
		t.Fatalf("Preflight after install: %+v", ps)
	}
}

func TestInstallFailsWithFirewallOff(t *testing.T) {
	v := newEnv(t)
	v.api.profiles["Public"] = "False"
	err := Install(context.Background(), v.o)
	if err == nil || !strings.Contains(err.Error(), "Windows Firewall is off for the Public profile") {
		t.Fatalf("Install with the firewall off: %v", err)
	}
}

func TestUninstallRemovesFirewallRules(t *testing.T) {
	v := newEnv(t)
	v.install()
	must(t, Uninstall(context.Background(), v.o, false))
	if len(v.api.fw) != 0 {
		t.Fatalf("rules left after uninstall: %v", v.api.fw)
	}
	if !v.api.has("PowerShell # tentmaker: remove network rules") {
		t.Fatal("uninstall ran no remove script")
	}
}

func TestNoFirewallRulesNoJob(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		breakIt func(v *tenv)
		want    string
	}{
		{"removed", func(v *tenv) { clear(v.api.fw) }, "network rules for CI jobs are missing"},
		{"one missing", func(v *tenv) { delete(v.api.fw, "Tentmaker-Block-Ranges") }, "1 of the network rules"},
		{"firewall off", func(v *tenv) { v.api.profiles["Domain"] = "False" }, "off for the Domain profile"},
		{"allow_lan rules on a strict host", func(v *tenv) {
			o := v.o
			o.AllowLAN = true
			must(t, Install(ctx, o))
		}, "rule Tentmaker-Block-Mesh-TCP is out of date"},
		{"another user's rules", func(v *tenv) { v.api.users[User] = "S-1-5-21-9-9-9-1001" }, "out of date"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t)
			v.install()
			c.breakIt(v)
			n := len(v.api.procs)
			err := v.e.Start(ctx, spec("job-nonet", core.KindCheck))
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "run tentmaker setup") {
				t.Fatalf("Start: %v", err)
			}
			if len(v.api.procs) != n {
				t.Fatal("a job started without the network rules")
			}
			found := false
			for _, p := range v.e.Preflight(ctx) {
				if p.Check == "windows-network" && strings.Contains(p.What, c.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("Preflight did not report %q: %+v", c.want, v.e.Preflight(ctx))
			}
		})
	}
}

// New-NetFirewallRule refuses an unspecified, multicast or loopback IPv6
// address in -RemoteAddress ("An unspecified, multicast, broadcast, or
// loopback IPv6 address was specified."), and setup fails with it.
func TestFirewallRulesAvoidRefusedIPv6(t *testing.T) {
	refused := func(a netip.Addr) bool {
		return a.Is6() && !a.Is4In6() && (a.IsLoopback() || a.IsUnspecified() || a.IsMulticast())
	}
	var rules []FirewallRule
	for _, rs := range [][]string{{"168.63.129.16"}, {"::1", "fec0:0:0:ffff::1", "127.0.0.53"}} {
		p := NetPolicy{SID: testSID}
		for _, r := range rs {
			p.Resolvers = append(p.Resolvers, netip.MustParseAddr(r))
		}
		rules = append(rules, FirewallRules(p)...)
	}
	for _, r := range rules {
		for _, s := range r.RemoteAddress {
			var ends []netip.Addr
			switch {
			case strings.Contains(s, "/"):
				pf := netip.MustParsePrefix(s)
				ends = append(ends, pf.Masked().Addr())
				if pf.Addr().Is6() && (pf.Contains(netip.IPv6Loopback()) || pf.Contains(netip.IPv6Unspecified())) {
					ends = append(ends, netip.IPv6Loopback())
				}
			case strings.Contains(s, "-"):
				a, b, _ := strings.Cut(s, "-")
				lo, hi := netip.MustParseAddr(a), netip.MustParseAddr(b)
				ends = append(ends, lo, hi)
				if lo.Is6() && lo.Compare(netip.IPv6Loopback()) <= 0 && hi.Compare(netip.IPv6Loopback()) >= 0 {
					ends = append(ends, netip.IPv6Loopback())
				}
			default:
				ends = append(ends, netip.MustParseAddr(s))
			}
			for _, a := range ends {
				if refused(a) {
					t.Errorf("rule %s has %s, which Windows Firewall refuses", r.Name, s)
				}
			}
		}
	}
}

func TestSelfAddresses(t *testing.T) {
	addrs := func(ss ...string) []netip.Addr {
		var out []netip.Addr
		for _, a := range ss {
			out = append(out, netip.MustParseAddr(a))
		}
		return out
	}
	p := NetPolicy{SID: testSID,
		Self: addrs("2001:db8:1::20", "::1", "127.0.0.1", "fe80::5", "192.168.1.20", "100.101.102.103", "fd7a:115c:a1e0::9",
			"203.0.113.7", "::ffff:203.0.113.7", "2001:db8:1::20", "ff02::1", "fd00::20", "198.51.100.53"),
		Resolvers: addrs("198.51.100.53")}
	// The fixed ranges already block loopback, link-local, ULA, the
	// tailnet and (without allow_lan) the LAN; a DNS server that is this
	// host gets the port-53 hole, not the self rule.
	if got := strings.Join(p.selfAddresses(), " "); got != "203.0.113.7 2001:db8:1::20" {
		t.Errorf("self = %q", got)
	}
	var host *FirewallRule
	for _, r := range FirewallRules(p) {
		if strings.Join(r.RemoteAddress, " ") == "198.51.100.53" && r.Protocol == "TCP" && r.Action == "Block" {
			host = &r
		}
	}
	if host == nil || strings.Join(host.RemotePort, ",") != "1-52,54-65535" {
		t.Errorf("no port-53 hole rule for a DNS server on this host: %+v", host)
	}
	p.AllowLAN = true
	if got := strings.Join(p.selfAddresses(), " "); got != "192.168.1.20 203.0.113.7 2001:db8:1::20" {
		t.Errorf("self with allow_lan = %q", got)
	}
	// Nothing to block: no rule, since a rule with no addresses blocks
	// every address.
	for _, r := range FirewallRules(NetPolicy{SID: testSID, Self: addrs("127.0.0.1", "::1", "10.1.0.4")}) {
		if r.Name == selfRule {
			t.Errorf("a self rule with nothing in it: %+v", r)
		}
	}
}

// The host's addresses change under rules that are in force (DHCP, a new
// IPv6 prefix, a VPN). Doctor says so; Start updates the self rule in
// place, never the whole group, and runs the job. A self rule that is
// gone comes back the same way.
func TestStartUpdatesSelfRule(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.install()
	v.api.addrs = append(v.api.addrs, "2001:db8:2::20", "198.51.100.9")
	found := false
	for _, p := range v.e.Preflight(ctx) {
		if p.Check == "windows-network" && strings.Contains(p.What, "own addresses is out of date") && strings.Contains(p.What, "2001:db8:2::20") {
			found = true
		}
	}
	if !found {
		t.Errorf("Preflight did not report the new address: %+v", v.e.Preflight(ctx))
	}
	n := len(v.api.procs)
	must(t, v.e.Start(ctx, spec("job-addr", core.KindCheck)))
	if len(v.api.procs) != n+1 {
		t.Fatal("the job did not start")
	}
	if got := strings.Join(v.api.self, " "); got != "198.51.100.9 2001:db8:1::20 2001:db8:2::20" {
		t.Errorf("self rule after Start: %s", got)
	}
	for _, s := range v.api.scripts {
		if strings.HasPrefix(s, "# tentmaker: host address rule\n") && !strings.Contains(s, "Set-NetFirewallRule") {
			t.Errorf("an existing self rule was not changed in place:\n%s", s)
		}
	}
	if !v.api.has("PowerShell # tentmaker: host address rule") {
		t.Error("Start ran no host address rule script")
	}

	delete(v.api.fw, selfRule)
	v.api.self = nil
	must(t, v.e.Start(ctx, spec("job-addr2", core.KindCheck)))
	if _, ok := v.api.fw[selfRule]; !ok || len(v.api.self) != 3 {
		t.Errorf("the self rule did not come back: %v %v", v.api.fw[selfRule], v.api.self)
	}
	if err := v.e.checkNet(ctx, testSID); err != nil {
		t.Errorf("after the update: %v", err)
	}

	// The host's global addresses go away: the rule goes, and a job runs.
	v.api.addrs = []string{"127.0.0.1", "192.168.1.20"}
	must(t, v.e.Start(ctx, spec("job-addr3", core.KindCheck)))
	if _, ok := v.api.fw[selfRule]; ok {
		t.Errorf("a self rule with no addresses left: %v", v.api.self)
	}
}
