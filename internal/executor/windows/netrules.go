package windows

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Network isolation: Windows Firewall outbound rules in the "Tentmaker"
// group, scoped to the runner user, keep CI jobs off the tailnet, the LAN,
// link-local, loopback and the mesh port. They are the Windows half of the
// Linux nftables rules (internal/executor/linux/netrules.go); keep the
// ranges in step.
const (
	FirewallGroup = "Tentmaker"
	RelayPort     = 7434
	MeshPort      = 7433
	// netVersion changes when the rules change, so a host with older rules
	// refuses jobs until setup runs again.
	netVersion = "net-v2"
)

// Blocked ranges. Windows Firewall has no "this host" match, so the host's
// own addresses outside these ranges (its global IPv6 address, its LAN
// address under allow_lan) get a rule of their own, Tentmaker-Block-Self,
// which the check keeps in step with the host; the mesh port is blocked on
// every address. Unlike
// the Linux rules there is no ::1: Windows Firewall refuses an IPv6
// loopback, unspecified or multicast remote address, and the whole rule
// with it (see windowsRefuses).
var (
	TailnetV4 = "100.64.0.0/10"
	TailnetV6 = "fd7a:115c:a1e0::/48"
	LANv4     = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	HostRange = []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "fc00::/7", "fe80::/10"}
	Loopback  = netip.MustParseAddr("127.0.0.1")
)

// NetPolicy is what the rules allow and block for one runner user.
type NetPolicy struct {
	SID       string
	Resolvers []netip.Addr
	AllowLAN  bool
	// Self are this host's own unicast addresses, on every interface.
	Self []netip.Addr
}

// selfRule is the name of the rule for the host's own addresses.
const selfRule = "Tentmaker-Block-Self"

// errStaleNet means the rules are in force but Tentmaker-Block-Self names
// other addresses than the host has now. Start rewrites that one rule;
// every other checkNet error refuses the job.
type errStaleNet struct{ what string }

func (e *errStaleNet) Error() string {
	return "the network rule for this host's own addresses is out of date: " + e.what
}

// FirewallRule is one outbound rule.
type FirewallRule struct {
	Name          string
	Display       string
	Action        string // Block or Allow
	Protocol      string // TCP, UDP or "" for any
	RemoteAddress []string
	RemotePort    []string
}

// hole is a destination a job may reach on some ports even though it sits
// in a blocked range: the relay on 127.0.0.1 and DNS resolvers.
type hole struct {
	addr     netip.Addr
	tcp, udp []uint16
}

// ranges are the fixed blocked ranges for p.
func (p NetPolicy) ranges() []netip.Prefix {
	var out []netip.Prefix
	add := func(s ...string) {
		for _, x := range s {
			out = append(out, netip.MustParsePrefix(x))
		}
	}
	add(TailnetV4, TailnetV6)
	add(HostRange...)
	if !p.AllowLAN {
		add(LANv4...)
	}
	return out
}

// self is the host's own addresses the fixed ranges miss and a rule can
// name: sorted, without repeats, zones or ::1.
func (p NetPolicy) self() []netip.Addr {
	var out []netip.Addr
	for _, a := range p.Self {
		a = a.Unmap()
		if a.Zone() != "" || windowsRefuses(a) || !a.IsValid() || a.IsMulticast() ||
			slices.ContainsFunc(p.ranges(), func(b netip.Prefix) bool { return b.Contains(a) }) {
			continue
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	return slices.Compact(out)
}

// blocked is every blocked destination: the fixed ranges and the host's
// own addresses.
func (p NetPolicy) blocked() []netip.Prefix {
	out := p.ranges()
	for _, a := range p.self() {
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out
}

// selfAddresses is what Tentmaker-Block-Self names: the host's own
// addresses, less those that are DNS servers (their own rules block every
// port but 53).
func (p NetPolicy) selfAddresses() []string {
	var holes []netip.Addr
	for _, h := range p.holes() {
		holes = append(holes, h.addr)
	}
	var out []string
	for _, a := range p.self() {
		if !slices.Contains(holes, a) {
			out = append(out, a.String())
		}
	}
	return out
}

// windowsRefuses is an address New-NetFirewallRule will not take as a
// remote address: "An unspecified, multicast, broadcast, or loopback IPv6
// address was specified."
func windowsRefuses(a netip.Addr) bool {
	return a.Is6() && (a.IsLoopback() || a.IsUnspecified() || a.IsMulticast())
}

// resolvers is p.Resolvers without the ones a rule can't name.
func (p NetPolicy) resolvers() []netip.Addr {
	var out []netip.Addr
	for _, r := range p.Resolvers {
		if !windowsRefuses(r) {
			out = append(out, r)
		}
	}
	return out
}

func (p NetPolicy) holes() []hole {
	hs := []hole{{addr: Loopback, tcp: []uint16{RelayPort}}}
	for _, r := range p.resolvers() {
		if !slices.ContainsFunc(p.blocked(), func(b netip.Prefix) bool { return b.Contains(r) }) {
			continue
		}
		if r == Loopback {
			hs[0].tcp = append(hs[0].tcp, 53)
			hs[0].udp = append(hs[0].udp, 53)
			continue
		}
		hs = append(hs, hole{addr: r, tcp: []uint16{53}, udp: []uint16{53}})
	}
	for i := range hs {
		slices.Sort(hs[i].tcp)
		slices.Sort(hs[i].udp)
	}
	return hs
}

// FirewallRules is every rule Install creates. Block rules win over allow
// rules in Windows Firewall, so the blocked ranges leave out the hole
// addresses, which get their own rules that block every other port.
func FirewallRules(p NetPolicy) []FirewallRule {
	var holes []netip.Addr
	hs := p.holes()
	for _, h := range hs {
		holes = append(holes, h.addr)
	}
	var ranges []string
	for _, b := range p.ranges() {
		ranges = append(ranges, carve(b, holes)...)
	}
	rules := []FirewallRule{
		{Name: "Tentmaker-Block-Mesh-TCP", Display: "Tentmaker: CI jobs never reach the mesh port (TCP)", Action: "Block", Protocol: "TCP", RemotePort: []string{fmt.Sprint(MeshPort)}},
		{Name: "Tentmaker-Block-Mesh-UDP", Display: "Tentmaker: CI jobs never reach the mesh port (UDP)", Action: "Block", Protocol: "UDP", RemotePort: []string{fmt.Sprint(MeshPort)}},
		{Name: "Tentmaker-Block-Ranges", Display: "Tentmaker: CI jobs never reach the tailnet, the LAN, link-local or loopback", Action: "Block", RemoteAddress: ranges},
	}
	// No addresses would mean every address, so no rule then.
	if self := p.selfAddresses(); len(self) > 0 {
		rules = append(rules, FirewallRule{Name: selfRule, Display: "Tentmaker: CI jobs never reach this host's own addresses",
			Action: "Block", RemoteAddress: self})
	}
	for i, h := range hs {
		for _, pr := range []struct {
			proto string
			open  []uint16
		}{{"TCP", h.tcp}, {"UDP", h.udp}} {
			r := FirewallRule{
				Name:          fmt.Sprintf("Tentmaker-Block-Host%d-%s", i, pr.proto),
				Display:       fmt.Sprintf("Tentmaker: CI jobs reach %s only on allowed ports (%s)", h.addr, pr.proto),
				Action:        "Block",
				Protocol:      pr.proto,
				RemoteAddress: []string{h.addr.String()},
				RemotePort:    otherPorts(pr.open),
			}
			rules = append(rules, r)
		}
	}
	rules = append(rules, FirewallRule{Name: "Tentmaker-Allow-Relay", Display: "Tentmaker: CI jobs reach the source archive relay",
		Action: "Allow", Protocol: "TCP", RemoteAddress: []string{Loopback.String()}, RemotePort: []string{fmt.Sprint(RelayPort)}})
	if len(p.resolvers()) > 0 {
		var rs []string
		for _, r := range p.resolvers() {
			rs = append(rs, r.String())
		}
		for _, proto := range []string{"UDP", "TCP"} {
			rules = append(rules, FirewallRule{Name: "Tentmaker-Allow-DNS-" + proto, Display: "Tentmaker: CI jobs reach the DNS servers (" + proto + ")",
				Action: "Allow", Protocol: proto, RemoteAddress: rs, RemotePort: []string{"53"}})
		}
	}
	return rules
}

// otherPorts is every port but open, as Windows Firewall port ranges.
// Nothing open means every port.
func otherPorts(open []uint16) []string {
	if len(open) == 0 {
		return nil
	}
	var out []string
	next := 1
	for _, o := range open {
		if int(o) > next {
			out = append(out, portRange(next, int(o)-1))
		}
		next = int(o) + 1
	}
	if next <= 65535 {
		out = append(out, portRange(next, 65535))
	}
	return out
}

func portRange(a, b int) string {
	if a == b {
		return fmt.Sprint(a)
	}
	return fmt.Sprintf("%d-%d", a, b)
}

// carve is prefix b without the addresses in holes: b itself when none
// fall in it, else address ranges around them.
func carve(b netip.Prefix, holes []netip.Addr) []string {
	var in []netip.Addr
	for _, h := range holes {
		if b.Contains(h) {
			in = append(in, h)
		}
	}
	if len(in) == 0 {
		return []string{b.String()}
	}
	slices.SortFunc(in, func(x, y netip.Addr) int { return x.Compare(y) })
	in = slices.Compact(in)
	first := b.Masked().Addr()
	last := lastAddr(b)
	var out []string
	lo := first
	for _, h := range in {
		if h.Compare(lo) > 0 {
			out = append(out, addrRange(lo, h.Prev()))
		}
		lo = h.Next()
	}
	if lo.IsValid() && lo.Compare(last) <= 0 {
		out = append(out, addrRange(lo, last))
	}
	return out
}

func addrRange(a, b netip.Addr) string {
	if a == b {
		return a.String()
	}
	return a.String() + "-" + b.String()
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().AsSlice()
	bits := p.Bits()
	for i := range a {
		for j := 0; j < 8; j++ {
			if i*8+j >= bits {
				a[i] |= 0x80 >> j
			}
		}
	}
	out, _ := netip.AddrFromSlice(a)
	return out
}

// netDescription is written on every rule. The check compares it, so rules
// for another user (a recreated account has a new SID), another allow_lan
// or an older Tentmaker refuse jobs until setup runs again.
func netDescription(p NetPolicy) string {
	return fmt.Sprintf("Written by tentmaker setup (%s, user %s, allow_lan=%t). CI jobs never reach the tailnet, %sthis host, loopback, link-local or the mesh port.",
		netVersion, p.SID, p.AllowLAN, map[bool]string{false: "the LAN, ", true: ""}[p.AllowLAN])
}

// psQuote is a PowerShell single-quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func psList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = psQuote(x)
	}
	return "@(" + strings.Join(q, ", ") + ")"
}

// FirewallScript replaces the group's rules with FirewallRules(p).
func FirewallScript(p NetPolicy) string {
	var b strings.Builder
	b.WriteString("# tentmaker: install network rules\n")
	b.WriteString("Get-NetFirewallRule -Group " + psQuote(FirewallGroup) + " -ErrorAction SilentlyContinue | Remove-NetFirewallRule\n")
	b.WriteString("$u = " + psQuote("D:(A;;CC;;;"+p.SID+")") + "\n")
	b.WriteString("$d = " + psQuote(netDescription(p)) + "\n")
	for _, r := range FirewallRules(p) {
		b.WriteString(newRule(r) + "\n")
	}
	return b.String()
}

// newRule is the New-NetFirewallRule line for r; $u and $d must be set.
func newRule(r FirewallRule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "New-NetFirewallRule -Name %s -DisplayName %s -Group %s -Description $d -Direction Outbound -Action %s -LocalUser $u -Profile Any -Enabled True",
		psQuote(r.Name), psQuote(r.Display), psQuote(FirewallGroup), r.Action)
	if r.Protocol != "" {
		b.WriteString(" -Protocol " + r.Protocol)
	}
	if len(r.RemoteAddress) > 0 {
		b.WriteString(" -RemoteAddress " + psList(r.RemoteAddress))
	}
	if len(r.RemotePort) > 0 {
		b.WriteString(" -RemotePort " + psList(r.RemotePort))
	}
	b.WriteString(" | Out-Null")
	return b.String()
}

// SelfRuleScript brings Tentmaker-Block-Self in step with p's addresses
// and leaves every other rule alone. An existing rule is changed in place,
// so the addresses it still names stay blocked throughout.
func SelfRuleScript(p NetPolicy) string {
	var b strings.Builder
	b.WriteString("# tentmaker: host address rule\n")
	b.WriteString("$r = Get-NetFirewallRule -Name " + psQuote(selfRule) + " -ErrorAction SilentlyContinue\n")
	self := p.selfAddresses()
	if len(self) == 0 {
		b.WriteString("if ($r) { $r | Remove-NetFirewallRule }\n")
		return b.String()
	}
	b.WriteString("if ($r) { Set-NetFirewallRule -Name " + psQuote(selfRule) + " -RemoteAddress " + psList(self) + " } else {\n")
	b.WriteString("$u = " + psQuote("D:(A;;CC;;;"+p.SID+")") + "\n")
	b.WriteString("$d = " + psQuote(netDescription(p)) + "\n")
	for _, r := range FirewallRules(p) {
		if r.Name == selfRule {
			b.WriteString(newRule(r) + "\n")
		}
	}
	b.WriteString("}\n")
	return b.String()
}

const removeFirewallScript = "# tentmaker: remove network rules\n" +
	"Get-NetFirewallRule -Group 'Tentmaker' -ErrorAction SilentlyContinue | Remove-NetFirewallRule\n"

// checkFirewallScript prints one line per profile and per rule in the group,
// as the firewall enforces them now, the addresses Tentmaker-Block-Self
// names, and the host's addresses and DNS servers it should name.
const checkFirewallScript = "# tentmaker: check network rules\n" +
	"Get-NetFirewallProfile -PolicyStore ActiveStore | ForEach-Object { 'profile {0} {1} {2}' -f $_.Name, $_.Enabled, $_.AllowLocalFirewallRules }\n" +
	"Get-NetFirewallRule -Group 'Tentmaker' -PolicyStore ActiveStore -ErrorAction SilentlyContinue | ForEach-Object { 'rule {0} {1} {2} {3} {4}' -f $_.Name, $_.Enabled, $_.Direction, $_.Action, $_.Description }\n" +
	"Get-NetFirewallRule -Name 'Tentmaker-Block-Self' -PolicyStore ActiveStore -ErrorAction SilentlyContinue | Get-NetFirewallAddressFilter | ForEach-Object { 'selfrule ' + ($_.RemoteAddress -join ' ') }\n" +
	"Get-NetIPAddress -ErrorAction SilentlyContinue | ForEach-Object { 'addr ' + $_.IPAddress }\n" +
	"Get-DnsClientServerAddress | ForEach-Object { $_.ServerAddresses } | Sort-Object -Unique | ForEach-Object { 'dns ' + $_ }\n"

const resolversScript = "# tentmaker: DNS servers\n" +
	"Get-DnsClientServerAddress | ForEach-Object { $_.ServerAddresses } | Sort-Object -Unique\n"

const addressesScript = "# tentmaker: host addresses\n" +
	"Get-NetIPAddress -ErrorAction SilentlyContinue | ForEach-Object { $_.IPAddress }\n"

// parseAddrs reads one address per line (after prefix, when given): no
// zoned (link-local) addresses, no repeats, in order.
func parseAddrs(out, prefix string) []netip.Addr {
	var as []netip.Addr
	for _, line := range strings.Split(out, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		a, err := netip.ParseAddr(strings.TrimSpace(v))
		if err != nil || a.Zone() != "" {
			continue
		}
		a = a.Unmap()
		if !slices.Contains(as, a) {
			as = append(as, a)
		}
	}
	return as
}

// policy reads what the rules depend on from the host: the DNS servers
// Windows is configured with and its own addresses.
func (e *Executor) policy(ctx context.Context, sid string) (NetPolicy, error) {
	out, err := e.api.PowerShell(ctx, resolversScript)
	if err != nil {
		return NetPolicy{}, fmt.Errorf("read the DNS servers: %w", err)
	}
	addrs, err := e.api.PowerShell(ctx, addressesScript)
	if err != nil {
		return NetPolicy{}, fmt.Errorf("read this host's addresses: %w", err)
	}
	return NetPolicy{SID: sid, Resolvers: parseAddrs(out, ""), AllowLAN: e.o.AllowLAN, Self: parseAddrs(addrs, "")}, nil
}

// installNet writes the rules for the runner user and checks them.
func (e *Executor) installNet(ctx context.Context, sid string) error {
	p, err := e.policy(ctx, sid)
	if err != nil {
		return err
	}
	if _, err := e.api.PowerShell(ctx, FirewallScript(p)); err != nil {
		return fmt.Errorf("add the Windows Firewall rules for CI jobs: %w", err)
	}
	return e.checkNet(ctx, sid)
}

// ensureNet is Start's gate: the rules must be in force. When only the
// host's own addresses changed (errStaleNet), it brings that rule in step
// and checks again; anything else refuses the job.
func (e *Executor) ensureNet(ctx context.Context, sid string) error {
	err := e.checkNet(ctx, sid)
	var stale *errStaleNet
	if !errors.As(err, &stale) {
		return err
	}
	e.log.Warn("updating the network rule for this host's own addresses", "executor", Name, "why", stale.what)
	p, perr := e.policy(ctx, sid)
	if perr != nil {
		return perr
	}
	if _, perr := e.api.PowerShell(ctx, SelfRuleScript(p)); perr != nil {
		return fmt.Errorf("update the Windows Firewall rule for this host's own addresses: %w", perr)
	}
	return e.checkNet(ctx, sid)
}

// checkNet fails unless Windows Firewall is on for every profile and every
// rule for this runner user and allow_lan is there, enabled and outbound.
// The rule set's addresses depend on the DNS servers, so only the names
// and descriptions are compared; the description carries the user and
// allow_lan.
func (e *Executor) checkNet(ctx context.Context, sid string) error {
	out, err := e.api.PowerShell(ctx, checkFirewallScript)
	if err != nil {
		return fmt.Errorf("read the Windows Firewall rules: %w", err)
	}
	profiles := map[string]bool{}
	rules := map[string]string{}
	var selfNow []string
	selfSeen := false
	var problems []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), " ", 6)
		switch {
		case len(f) >= 3 && f[0] == "profile":
			profiles[f[1]] = true
			if f[2] != "True" {
				problems = append(problems, "Windows Firewall is off for the "+f[1]+" profile")
			} else if len(f) >= 4 && f[3] == "False" {
				problems = append(problems, "policy makes Windows Firewall ignore local rules for the "+f[1]+" profile")
			}
		case len(f) >= 1 && f[0] == "selfrule":
			selfSeen = true
			selfNow = append(selfNow, strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "selfrule")))...)
		case len(f) == 6 && f[0] == "rule":
			if f[2] != "True" || f[3] != "Outbound" {
				problems = append(problems, fmt.Sprintf("rule %s is %s, %s", f[1], f[2], f[3]))
				continue
			}
			rules[f[1]] = f[4] + " " + f[5]
		}
	}
	for _, p := range []string{"Domain", "Private", "Public"} {
		if !profiles[p] {
			problems = append(problems, "Windows Firewall reports no "+p+" profile")
		}
	}
	// Rule names but the self rule don't depend on the host's addresses or
	// DNS servers (an address or resolver change must not refuse jobs).
	p := NetPolicy{SID: sid, AllowLAN: e.o.AllowLAN}
	desc := netDescription(p)
	missing := 0
	for _, r := range FirewallRules(p) {
		got, ok := rules[r.Name]
		switch {
		case !ok:
			missing++
		case got != r.Action+" "+desc:
			problems = append(problems, fmt.Sprintf("rule %s is out of date (%s)", r.Name, trunc(got, 200)))
		}
	}
	if missing > 0 {
		problems = append(problems, fmt.Sprintf("%d of the network rules for CI jobs are missing", missing))
	}
	if got, ok := rules[selfRule]; ok && got != "Block "+desc {
		problems = append(problems, fmt.Sprintf("rule %s is out of date (%s)", selfRule, trunc(got, 200)))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	host := NetPolicy{SID: sid, AllowLAN: e.o.AllowLAN, Resolvers: parseAddrs(out, "dns "), Self: parseAddrs(out, "addr ")}
	want := host.selfAddresses()
	var have []string
	for _, a := range selfNow {
		if x, err := netip.ParseAddr(a); err == nil {
			a = x.Unmap().String()
		}
		have = append(have, a)
	}
	if _, ok := rules[selfRule]; !ok || !selfSeen {
		have = nil
	}
	slices.Sort(have)
	if w := slices.Sorted(slices.Values(want)); !slices.Equal(have, w) {
		return &errStaleNet{fmt.Sprintf("it names %s; this host has %s", orNone(have), orNone(w))}
	}
	return nil
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, " ")
}

// uninstallNet removes every rule in the group.
func (e *Executor) uninstallNet(ctx context.Context) error {
	if _, err := e.api.PowerShell(ctx, removeFirewallScript); err != nil {
		return fmt.Errorf("remove the Windows Firewall rules: %w", err)
	}
	return nil
}
