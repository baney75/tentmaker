package windows

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

func TestFirewallScript(t *testing.T) {
	p := NetPolicy{SID: testSID, Resolvers: []netip.Addr{
		netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("100.100.100.100"),
	}}
	want := `# tentmaker: install network rules
$u = 'D:(A;;CC;;;S-1-5-21-1-2-3-1001)'
$d = 'Written by tentmaker setup (net-v3, user S-1-5-21-1-2-3-1001, allow_lan=false). CI jobs never reach the tailnet, the LAN, link-local or the mesh port on other hosts. Windows Firewall does not filter traffic to this host; Tentmaker''s WFP filters do.'
$want = @('Tentmaker-Block-Mesh-TCP-ac4259065d', 'Tentmaker-Block-Mesh-UDP-81f949504c', 'Tentmaker-Block-Ranges-eb44627341', 'Tentmaker-Block-Host0-TCP-1ce39e2b73', 'Tentmaker-Block-Host0-UDP-e7d30f5442', 'Tentmaker-Block-Host1-TCP-c531c59087', 'Tentmaker-Block-Host1-UDP-8c21aec547', 'Tentmaker-Block-Host2-TCP-04dd39c9a8', 'Tentmaker-Block-Host2-UDP-77ea22554e', 'Tentmaker-Allow-Relay-edcdfd3892', 'Tentmaker-Allow-DNS-UDP-4b5c18e5a6', 'Tentmaker-Allow-DNS-TCP-e4795d7c8f')
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Mesh-TCP-ac4259065d' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Mesh-TCP-ac4259065d' -DisplayName 'Tentmaker: CI jobs never reach the mesh port (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemotePort @('7433') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Mesh-UDP-81f949504c' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Mesh-UDP-81f949504c' -DisplayName 'Tentmaker: CI jobs never reach the mesh port (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemotePort @('7433') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Ranges-eb44627341' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Ranges-eb44627341' -DisplayName 'Tentmaker: CI jobs never reach the tailnet, the LAN or link-local on other hosts' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -RemoteAddress @('100.64.0.0-100.100.100.99', '100.100.100.101-100.127.255.255', 'fd7a:115c:a1e0::/48', '0.0.0.0/8', '127.0.0.0', '127.0.0.2-127.255.255.255', '169.254.0.0/16', 'fc00::/7', 'fe80::/10', '10.0.0.0/8', '172.16.0.0/12', '192.168.0.0-192.168.1.0', '192.168.1.2-192.168.255.255') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Host0-TCP-1ce39e2b73' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Host0-TCP-1ce39e2b73' -DisplayName 'Tentmaker: CI jobs reach 127.0.0.1 only on allowed ports (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('127.0.0.1') -RemotePort @('1-7434', '7436-65535') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Host0-UDP-e7d30f5442' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Host0-UDP-e7d30f5442' -DisplayName 'Tentmaker: CI jobs reach 127.0.0.1 only on allowed ports (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('127.0.0.1') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Host1-TCP-c531c59087' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Host1-TCP-c531c59087' -DisplayName 'Tentmaker: CI jobs reach 192.168.1.1 only on allowed ports (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('192.168.1.1') -RemotePort @('1-52', '54-65535') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Host1-UDP-8c21aec547' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Host1-UDP-8c21aec547' -DisplayName 'Tentmaker: CI jobs reach 192.168.1.1 only on allowed ports (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('192.168.1.1') -RemotePort @('1-52', '54-65535') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Host2-TCP-04dd39c9a8' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Host2-TCP-04dd39c9a8' -DisplayName 'Tentmaker: CI jobs reach 100.100.100.100 only on allowed ports (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('100.100.100.100') -RemotePort @('1-52', '54-65535') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Block-Host2-UDP-77ea22554e' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Block-Host2-UDP-77ea22554e' -DisplayName 'Tentmaker: CI jobs reach 100.100.100.100 only on allowed ports (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Block -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('100.100.100.100') -RemotePort @('1-52', '54-65535') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Allow-Relay-edcdfd3892' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Allow-Relay-edcdfd3892' -DisplayName 'Tentmaker: CI jobs reach the source archive relay' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Allow -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('127.0.0.1') -RemotePort @('7435') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Allow-DNS-UDP-4b5c18e5a6' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Allow-DNS-UDP-4b5c18e5a6' -DisplayName 'Tentmaker: CI jobs reach the DNS servers (UDP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Allow -LocalUser $u -Profile Any -Enabled True -Protocol UDP -RemoteAddress @('168.63.129.16', '192.168.1.1', '100.100.100.100') -RemotePort @('53') | Out-Null }
if (-not (Get-NetFirewallRule -Name 'Tentmaker-Allow-DNS-TCP-e4795d7c8f' -ErrorAction SilentlyContinue)) { New-NetFirewallRule -Name 'Tentmaker-Allow-DNS-TCP-e4795d7c8f' -DisplayName 'Tentmaker: CI jobs reach the DNS servers (TCP)' -Group 'Tentmaker' -Description $d -Direction Outbound -Action Allow -LocalUser $u -Profile Any -Enabled True -Protocol TCP -RemoteAddress @('168.63.129.16', '192.168.1.1', '100.100.100.100') -RemotePort @('53') | Out-Null }
Get-NetFirewallRule -Group 'Tentmaker' -ErrorAction SilentlyContinue | Where-Object { $want -notcontains $_.Name } | Remove-NetFirewallRule
`
	if got := FirewallScript(p); got != want {
		t.Errorf("FirewallScript:\n%s\nwant:\n%s", got, want)
	}

	// allow_lan: the RFC 1918 ranges go, nothing else does, and the LAN
	// resolver needs no hole.
	p.AllowLAN = true
	got := FirewallScript(p)
	for _, r := range []string{"'10.0.0.0/8'", "'172.16.0.0/12'", "192.168.0.0", "reach 192.168.1.1 only", "the LAN or"} {
		if strings.Contains(got, r) {
			t.Errorf("allow_lan still has %s", r)
		}
	}
	for _, r := range []string{"'100.64.0.0-100.100.100.99'", "'fd7a:115c:a1e0::/48'", "'169.254.0.0/16'", "'fc00::/7'", "-RemotePort @('7433')", "allow_lan=true"} {
		if !strings.Contains(got, r) {
			t.Errorf("allow_lan dropped %s", r)
		}
	}
	// No rule or description claims the firewall keeps jobs off this host
	// or loopback: Windows Firewall does not filter that traffic.
	for _, r := range FirewallRules(p) {
		if d := strings.ToLower(r.Display); strings.Contains(d, "loopback") || strings.Contains(d, "this host") || strings.Contains(d, "own address") {
			t.Errorf("rule %s claims %q", r.Name, r.Display)
		}
	}
	if d := netDescription(p); !strings.Contains(d, "on other hosts") || !strings.Contains(d, "WFP filters") {
		t.Errorf("description: %s", d)
	}
}

// A rerun adds every new rule before it removes any old one, so a job
// running during setup or a DNS change is never unblocked (the old script
// removed the whole group first).
func TestFirewallScriptAddsBeforeItRemoves(t *testing.T) {
	p := NetPolicy{SID: testSID, Resolvers: []netip.Addr{netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("192.168.1.1")}}
	s := FirewallScript(p)
	lastNew := strings.LastIndex(s, "New-NetFirewallRule")
	firstRemove := strings.Index(s, "Remove-NetFirewallRule")
	if firstRemove < lastNew || strings.Count(s, "Remove-NetFirewallRule") != 1 ||
		!strings.Contains(s, "Where-Object { $want -notcontains $_.Name } | Remove-NetFirewallRule") {
		t.Fatalf("the script removes rules before it adds them, or removes wanted ones:\n%s", s)
	}
	// A rule changes name when it changes, so "already there by name" means
	// "already right".
	q := p
	q.Resolvers = []netip.Addr{netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("192.168.1.9")}
	a, b := FirewallRules(p), FirewallRules(q)
	for i := range a {
		same := slices.Equal(a[i].RemoteAddress, b[i].RemoteAddress) && slices.Equal(a[i].RemotePort, b[i].RemotePort)
		if same != (a[i].Name == b[i].Name) {
			t.Errorf("rule %s / %s: same content %v but names equal %v", a[i].Name, b[i].Name, same, a[i].Name == b[i].Name)
		}
	}
	q = p
	q.SID = "S-1-5-21-9-9-9-1001"
	if FirewallRules(q)[0].Name == FirewallRules(p)[0].Name {
		t.Error("a rule for another user has the same name")
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

func TestInstallFailsWithFirewallOff(t *testing.T) {
	v := newEnv(t)
	v.api.profiles["Public"] = "False"
	err := Install(context.Background(), v.o)
	if err == nil || !strings.Contains(err.Error(), "Windows Firewall is off for the Public profile") {
		t.Fatalf("Install with the firewall off: %v", err)
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
	for _, rs := range [][]string{{"168.63.129.16"}, {"::1", "fec0:0:0:ffff::1", "127.0.0.53", "fd7a:115c:a1e0::53"}} {
		for _, lan := range []bool{false, true} {
			p := NetPolicy{SID: testSID, AllowLAN: lan}
			for _, r := range rs {
				p.Resolvers = append(p.Resolvers, netip.MustParseAddr(r))
			}
			rules = append(rules, FirewallRules(p)...)
		}
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

func TestWFPFilters(t *testing.T) {
	rs := []netip.Addr{netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("::1"), netip.MustParseAddr("fe80::1%4"), netip.MustParseAddr("::ffff:192.0.2.53")}
	fs := WFPFilters(testSID, rs)
	keys := map[GUID]bool{}
	var blocks, permits int
	for _, f := range fs {
		if keys[f.Key] || !slices.Contains(WFPKeys(), f.Key) {
			t.Errorf("filter %s: key %s repeated or not Tentmaker's", f.Name, f.Key)
		}
		keys[f.Key] = true
		if f.SID != testSID {
			t.Errorf("filter %s is not for the runner user", f.Description())
		}
		for _, a := range f.DstAddrs {
			if a.Is6() != f.V6 || a.Zone() != "" {
				t.Errorf("filter %s names %s", f.Description(), a)
			}
		}
		// Nothing at the connect layers touches loopback traffic: the
		// job's connections to its own listeners must pass there, since the
		// connect side can't tell who listens (review r2 partS
		// second-S2-F6). Only multicast and broadcast are cut at connect.
		if !f.Accept && !f.AnyHost {
			t.Errorf("a loopback filter at the connect layer: %s", f.Description())
		}
		if f.ToRunner && (!f.Accept || !f.Permit) {
			t.Errorf("a listener condition on anything but an accept permit: %s", f.Description())
		}
		if f.Permit {
			permits++
			if !f.Accept || f.Weight <= 1 || f.AnyHost {
				t.Errorf("permit not at accept, or weighed at or below the blocks: %s", f.Description())
			}
			switch {
			case f.ToRunner:
				// The job's own listeners: any port, protocol and address.
				if f.Protocol != 0 || f.DstPort != 0 || len(f.DstAddrs) != 0 || len(f.DstPrefixes) != 0 {
					t.Errorf("own-listener permit narrower than all the job's listeners: %s", f.Description())
				}
			case f.DstPort == 53:
				if f.Protocol == 0 || len(f.DstAddrs) == 0 {
					t.Errorf("DNS permit too wide: %s", f.Description())
				}
			case f.DstPort != RelayPort || f.Protocol != 6 || f.V6 || !slices.Equal(f.DstAddrs, []netip.Addr{Loopback}):
				t.Errorf("permit for something but the job's listeners, DNS and the relay: %s", f.Description())
			}
			continue
		}
		blocks++
		if f.AnyHost {
			// Multicast and broadcast, to any host, each family.
			want := MulticastV4
			if f.V6 {
				want = MulticastV6
			}
			if f.Accept || f.Protocol != 0 || f.DstPort != 0 || len(f.DstAddrs) != 0 || !slices.Equal(f.DstPrefixes, want) {
				t.Errorf("multicast block: %s", f.Description())
			}
			continue
		}
		// The host blocks take all of this host's traffic from the user,
		// both families, no matter the address or port.
		if f.Protocol != 0 || f.DstPort != 0 || len(f.DstAddrs) != 0 || len(f.DstPrefixes) != 0 {
			t.Errorf("block narrower than all loopback: %s", f.Description())
		}
	}
	for _, want := range []string{
		"tentmaker wfp-v1 user " + testSID + ": permit accept v4 loopback weight 15 listener the same user",
		"tentmaker wfp-v1 user " + testSID + ": permit accept v6 loopback weight 15 listener the same user",
		"tentmaker wfp-v1 user " + testSID + ": permit accept v4 loopback weight 14 proto 6 port 7435 to 127.0.0.1",
		"tentmaker wfp-v1 user " + testSID + ": permit accept v4 loopback weight 13 proto 17 port 53 to 168.63.129.16 192.0.2.53",
		"tentmaker wfp-v1 user " + testSID + ": permit accept v6 loopback weight 13 proto 6 port 53 to ::1",
		"tentmaker wfp-v1 user " + testSID + ": block connect v4 any host weight 1 to 224.0.0.0/4 255.255.255.255/32",
		"tentmaker wfp-v1 user " + testSID + ": block connect v6 any host weight 1 to ff00::/8",
		"tentmaker wfp-v1 user " + testSID + ": block accept v4 loopback weight 1",
		"tentmaker wfp-v1 user " + testSID + ": block accept v6 loopback weight 1",
	} {
		if !slices.ContainsFunc(fs, func(f WFPFilter) bool { return f.Description() == want }) {
			t.Errorf("no %q", want)
		}
	}
	if blocks != 4 || permits != 7 {
		t.Errorf("%d blocks and %d permits:\n%v", blocks, permits, fs)
	}
	// No DNS servers: the job's listeners, the relay and the blocks only.
	if fs := WFPFilters(testSID, nil); len(fs) != 7 {
		t.Errorf("filters without DNS servers: %v", fs)
	}
}

// wfpConn is one connection (or first datagram) as WFP classifies it.
type wfpConn struct {
	accept   bool // the listening side's classification; else the connecting side's
	v6       bool
	loopback bool
	user     string // connect: the socket's user; accept: the remote (connecting) user
	listener string // accept: the listening socket's user
	proto    uint8
	port     uint16 // the listening end's port
	addr     netip.Addr
}

// wfpDecide plays WFP's arbitration in Tentmaker's sublayer: the matching
// filter with the highest weight decides. It returns "permit", "block" or
// "" (no filter matched: the sublayer has no say).
func wfpDecide(fs []WFPFilter, c wfpConn) string {
	best, act := -1, ""
	for _, f := range fs {
		ok := f.Accept == c.accept && f.V6 == c.v6 && f.SID == c.user && (f.AnyHost || c.loopback) &&
			(!f.ToRunner || c.listener == f.SID) && (f.Protocol == 0 || f.Protocol == c.proto) &&
			(f.DstPort == 0 || f.DstPort == c.port)
		if ok && (len(f.DstAddrs) > 0 || len(f.DstPrefixes) > 0) {
			ok = slices.Contains(f.DstAddrs, c.addr) || slices.ContainsFunc(f.DstPrefixes, func(p netip.Prefix) bool { return p.Contains(c.addr) })
		}
		if ok && int(f.Weight) > best {
			best, act = int(f.Weight), "block"
			if f.Permit {
				act = "permit"
			}
		}
	}
	return act
}

// A job reaches its own listeners on 127.0.0.1, ::1 and the host's own
// addresses (httptest, a test database, a Gradle daemon), the relay and
// the DNS servers, and nothing else on this host (review r2 partS
// second-S2-F6).
func TestWFPFiltersKeepJobsOffHostServices(t *testing.T) {
	const admin = "S-1-5-18"
	dns := netip.MustParseAddr("192.0.2.53")
	fs := WFPFilters(testSID, []netip.Addr{dns, netip.MustParseAddr("::1")})
	v4, v6, own := netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("192.0.2.77")
	for _, c := range []struct {
		name string
		c    wfpConn
		want string
	}{
		{"job connects on loopback", wfpConn{v6: false, loopback: true, user: testSID, proto: 6, port: 8766, addr: v4}, ""},
		{"job connects on ::1", wfpConn{v6: true, loopback: true, user: testSID, proto: 6, port: 8766, addr: v6}, ""},
		{"job to its own 127.0.0.1 listener", wfpConn{accept: true, loopback: true, user: testSID, listener: testSID, proto: 6, port: 41234, addr: v4}, "permit"},
		{"job to its own ::1 listener", wfpConn{accept: true, v6: true, loopback: true, user: testSID, listener: testSID, proto: 6, port: 41234, addr: v6}, "permit"},
		{"job to its own UDP socket", wfpConn{accept: true, loopback: true, user: testSID, listener: testSID, proto: 17, port: 41235, addr: v4}, "permit"},
		{"job to its own listener on a host address", wfpConn{accept: true, loopback: true, user: testSID, listener: testSID, proto: 6, port: 8080, addr: own}, "permit"},
		{"job to a host service on 127.0.0.1", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 6, port: 8766, addr: v4}, "block"},
		{"job to a host service on ::1", wfpConn{accept: true, v6: true, loopback: true, user: testSID, listener: admin, proto: 6, port: 8766, addr: v6}, "block"},
		{"job to the mesh port", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 6, port: 7433, addr: v4}, "block"},
		{"job to the control API", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 6, port: 7434, addr: v4}, "block"},
		{"job to a host UDP service", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 17, port: 8768, addr: v4}, "block"},
		{"job to a host service on a host address", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 6, port: 8767, addr: own}, "block"},
		{"job to the relay", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 6, port: RelayPort, addr: v4}, "permit"},
		{"job to the relay port over UDP", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 17, port: RelayPort, addr: v4}, "block"},
		{"job to the relay port on ::1", wfpConn{accept: true, v6: true, loopback: true, user: testSID, listener: admin, proto: 6, port: RelayPort, addr: v6}, "block"},
		{"job to the DNS server", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 17, port: 53, addr: dns}, "permit"},
		{"job to DNS on ::1", wfpConn{accept: true, v6: true, loopback: true, user: testSID, listener: admin, proto: 6, port: 53, addr: v6}, "permit"},
		{"job to port 53 elsewhere on this host", wfpConn{accept: true, loopback: true, user: testSID, listener: admin, proto: 17, port: 53, addr: v4}, "block"},
		{"host process to a host service", wfpConn{accept: true, loopback: true, user: admin, listener: admin, proto: 6, port: 8766, addr: v4}, ""},
		{"job to multicast", wfpConn{user: testSID, proto: 17, port: 7777, addr: netip.MustParseAddr("239.77.77.77")}, "block"},
		{"job to broadcast", wfpConn{user: testSID, proto: 17, port: 67, addr: netip.MustParseAddr("255.255.255.255")}, "block"},
		{"job to IPv6 multicast", wfpConn{v6: true, user: testSID, proto: 17, port: 5353, addr: netip.MustParseAddr("ff02::fb")}, "block"},
		{"job to the internet", wfpConn{user: testSID, proto: 6, port: 443, addr: netip.MustParseAddr("140.82.112.3")}, ""},
	} {
		if got := wfpDecide(fs, c.c); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCheckWFP(t *testing.T) {
	want := WFPFilters(testSID, []netip.Addr{netip.MustParseAddr("192.0.2.53")})
	good := func() []WFPState {
		var out []WFPState
		for _, f := range want {
			out = append(out, WFPState{Key: f.Key, Description: f.Description(), Accept: f.Accept, V6: f.V6, Permit: f.Permit, Block: !f.Permit, Persistent: true, Hard: !f.Permit, Sublayer: true})
		}
		return out
	}
	if err := checkWFP(want, good()); err != nil {
		t.Fatalf("the right filters: %v", err)
	}
	isStale := func(err error) bool { var se *errStaleNet; return errors.As(err, &se) }
	for _, c := range []struct {
		name  string
		edit  func(hs []WFPState) []WFPState
		stale bool
		msg   string
	}{
		{"none", func([]WFPState) []WFPState { return nil }, false, "WFP filters that keep CI jobs off this host are missing"},
		{"one gone", func(hs []WFPState) []WFPState { return hs[1:] }, true, "missing"},
		{"soft block", func(hs []WFPState) []WFPState { hs[len(hs)-1].Hard = false; return hs }, true, "is not"},
		{"not persistent", func(hs []WFPState) []WFPState { hs[0].Persistent = false; return hs }, true, "is not"},
		{"permit for block", func(hs []WFPState) []WFPState { hs[len(hs)-1].Block, hs[len(hs)-1].Permit = false, true; return hs }, true, "is not"},
		{"other sublayer", func(hs []WFPState) []WFPState { hs[0].Sublayer = false; return hs }, true, "is not"},
		{"connect layer for accept", func(hs []WFPState) []WFPState { hs[0].Accept = false; return hs }, true, "is not"},
		{"new DNS server", func(hs []WFPState) []WFPState {
			i := slices.IndexFunc(hs, func(h WFPState) bool { return strings.Contains(h.Description, "192.0.2.53") })
			hs[i].Description = strings.Replace(hs[i].Description, "192.0.2.53", "192.0.2.54", 1)
			return hs
		}, true, "is not"},
		{"extra", func(hs []WFPState) []WFPState {
			return append(hs, WFPState{Key: wfpFilterKey(wfpSlots - 1), Description: hs[0].Description})
		}, true, "extra"},
		{"another user", func(hs []WFPState) []WFPState {
			hs[0].Description = strings.Replace(hs[0].Description, testSID, "S-1-5-21-9-9-9-1001", 1)
			return hs
		}, false, "for another user or version"},
	} {
		err := checkWFP(want, c.edit(good()))
		if err == nil || isStale(err) != c.stale || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v (stale %v), want stale %v and %q", c.name, err, isStale(err), c.stale, c.msg)
		}
	}
}

func TestInstallAddsFirewallRules(t *testing.T) {
	v := newEnv(t)
	v.install()
	p := NetPolicy{SID: testSID, Resolvers: []netip.Addr{netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("192.168.1.1")}}
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
	if len(v.api.wfp) != len(WFPFilters(testSID, p.Resolvers)) {
		t.Fatalf("WFP filters after install: %v", v.api.wfp)
	}
	for _, s := range v.api.scripts {
		if strings.Contains(s, "Tm1!") || strings.Contains(s, string(v.api.password)) && len(v.api.password) > 0 {
			t.Fatal("a firewall script holds the runner password")
		}
	}
	// Rerun: the same rules, nothing removed and nothing added.
	before := len(v.api.fw)
	v.install()
	if len(v.api.fw) != before {
		t.Fatalf("rerun changed the rule count: %d -> %d", before, len(v.api.fw))
	}
	if steps := v.api.installSteps[len(v.api.installSteps)-1]; len(steps) != 0 {
		t.Errorf("rerun with nothing new changed rules: %v", steps)
	}
	if ps := v.e.Preflight(context.Background()); len(ps) != 0 {
		t.Fatalf("Preflight after install: %+v", ps)
	}
}

// WFP refuses the filters: setup fails, and no job runs.
func TestInstallFailsWithoutWFP(t *testing.T) {
	v := newEnv(t)
	v.api.wfpErr = errors.New("FwpmFilterAdd0: access denied")
	err := Install(context.Background(), v.o)
	if err == nil || !strings.Contains(err.Error(), "add the WFP filters that keep CI jobs off this host") {
		t.Fatalf("Install without WFP: %v", err)
	}
}

func TestUninstallRemovesFirewallRules(t *testing.T) {
	v := newEnv(t)
	v.install()
	must(t, Uninstall(context.Background(), v.o, false))
	if len(v.api.fw) != 0 || len(v.api.wfp) != 0 {
		t.Fatalf("rules left after uninstall: %v %v", v.api.fw, v.api.wfp)
	}
	if !v.api.has("PowerShell # tentmaker: remove network rules") || !v.api.has("RemoveWFP") {
		t.Fatal("uninstall did not remove the rules and the WFP filters")
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
		{"firewall off", func(v *tenv) { v.api.profiles["Domain"] = "False" }, "off for the Domain profile"},
		{"allow_lan rules on a strict host", func(v *tenv) {
			o := v.o
			o.AllowLAN = true
			must(t, Install(ctx, o))
		}, "allow_lan=true"},
		{"another user's rules", func(v *tenv) { v.api.users[User] = "S-1-5-21-9-9-9-1001" }, "out of date"},
		{"WFP filters removed", func(v *tenv) { clear(v.api.wfp) }, "WFP filters that keep CI jobs off this host are missing"},
		// Stale filters, and WFP refuses the new ones: fail closed.
		{"WFP refuses", func(v *tenv) {
			delete(v.api.wfp, wfpFilterKey(9))
			v.api.wfpErr = errors.New("FwpmTransactionCommit0: timeout")
		}, "add the WFP filters"},
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
			if c.name == "WFP refuses" {
				return // doctor reports the stale filter, which Start would fix
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

// The host's DNS servers change under rules in force (DHCP, a VPN). The
// check compares against the policy install used, so doctor says so and
// Start rewrites the rules and filters, new rules first, then runs the
// job. A rule or filter someone deleted comes back the same way.
func TestStartRewritesStaleNetRules(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.install()
	v.api.dns = []string{"168.63.129.16", "10.9.9.9"}
	found := false
	for _, p := range v.e.Preflight(ctx) {
		if p.Check == "windows-network" && strings.Contains(p.What, "out of date") {
			found = true
		}
	}
	if !found {
		t.Errorf("Preflight did not report the new DNS server: %+v", v.e.Preflight(ctx))
	}
	n := len(v.api.procs)
	must(t, v.e.Start(ctx, spec("job-dns", core.KindCheck)))
	if len(v.api.procs) != n+1 {
		t.Fatal("the job did not start")
	}
	p := NetPolicy{SID: testSID, Resolvers: []netip.Addr{netip.MustParseAddr("168.63.129.16"), netip.MustParseAddr("10.9.9.9")}}
	var want []string
	for _, r := range FirewallRules(p) {
		want = append(want, r.Name)
	}
	var got []string
	for k := range v.api.fw {
		got = append(got, k)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("rules after Start:\n%v\nwant\n%v", got, want)
	}
	steps := v.api.installSteps[len(v.api.installSteps)-1]
	seenRemove := false
	for _, s := range steps {
		seenRemove = seenRemove || s[0] == '-'
		if s[0] == '+' && seenRemove {
			t.Errorf("a rule was added after one was removed: %v", steps)
		}
	}
	if !slices.ContainsFunc(steps, func(s string) bool { return strings.HasPrefix(s, "+Tentmaker-Block-Host1") }) {
		t.Errorf("no hole rule for the new LAN DNS server: %v", steps)
	}
	if st, ok := v.api.wfp[wfpFilterKey(3)]; !ok || !strings.Contains(st.Description, "10.9.9.9") || strings.Contains(st.Description, "192.168.1.1") {
		t.Errorf("WFP DNS filter after Start: %+v", st)
	}
	must(t, v.e.checkNet(ctx, testSID))

	for i, breakIt := range []func(){
		func() { delete(v.api.fw, want[0]) },
		func() { delete(v.api.wfp, wfpFilterKey(10)) },
	} {
		breakIt()
		must(t, v.e.Start(ctx, spec(fmt.Sprintf("job-back%d", i), core.KindCheck)))
		must(t, v.e.checkNet(ctx, testSID))
	}
}
