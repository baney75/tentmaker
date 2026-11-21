package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Network isolation for the runner user. Every CI process sends through the
// output hook with its socket's uid: the runner's own uid, or one of its
// subordinate uids for a rootless container on the host network that runs
// as a non-root user (pasta and slirp4netns open their sockets as the
// runner). A filter on those uids confines all of them. The job may reach
// the internet, DNS and the source archive relay; never the tailnet (nor
// what it routes), the LAN, multicast, link-local (cloud metadata), this
// host's own addresses, or the mesh port anywhere.

// NetTable is the nftables table (family inet) that holds the rules.
const NetTable = "tentmaker"

// NetUnit loads the rules at boot, before tentmaker.service.
const NetUnit = "tentmaker-net.service"

// NetChain is the iptables chain the fallback uses when nft is missing.
const NetChain = "TENTMAKER"

// RelayPort is the source archive relay's loopback port, where check jobs
// fetch their source archive. MeshPort is the peer API's port on every device.
const (
	RelayPort = 7435
	MeshPort  = 7433
)

// Address ranges a job never reaches. LANv4 is dropped by guards.allow_lan;
// the rest always applies.
var (
	TailnetV4 = "100.64.0.0/10"
	TailnetV6 = "fd7a:115c:a1e0::/48"
	LANv4     = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	HostV4    = []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16"}
	HostV6    = []string{"::1/128", "fc00::/7", "fe80::/10"}
	// Multicast and broadcast reach every device on the LAN segment
	// (mDNS, SSDP); a CI job has no use for them.
	MulticastV4 = []string{"224.0.0.0/4", "255.255.255.255"}
	MulticastV6 = []string{"ff00::/8"}
	// TailscaleRoutedV4 and TailscaleRoutedV6 are never reached through the
	// Tailscale interface, whatever allow_lan says: there they are other
	// tailnet devices and the subnets they route. Public destinations stay
	// open there so an exit node still gives jobs the internet.
	TailscaleRoutedV4 = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}
	TailscaleRoutedV6 = []string{"fc00::/7"}
)

// TailscalePaths hold tailscaled's LocalAPI socket (mode 0666) and its
// state. Firewall rules don't see a Unix socket, and a job on it could read
// every tailnet peer and login, so job units and the CI user's manager
// (rootless containers) never see these paths.
var TailscalePaths = []string{"/run/tailscale", "/var/run/tailscale", "/var/lib/tailscale"}

// TailscaleIface is tailscaled's interface unless it was started with
// another --tun name.
const TailscaleIface = "tailscale0"

// StubResolver is systemd-resolved's stub, always allowed for DNS.
var StubResolver = netip.MustParseAddr("127.0.0.53")

// NetPolicy is what the rules say for one host.
type NetPolicy struct {
	UID       int
	SubUIDs   []IDRange    // the runner's subordinate uids (rootless containers)
	Resolvers []netip.Addr // DNS servers jobs may query on port 53
	AllowLAN  bool
	// TailscaleIfaces are the interfaces that lead into the tailnet.
	TailscaleIfaces []string
	// OnLinkV6 are the global IPv6 prefixes this host reaches without a
	// router: the LAN by its public addresses. Rejected unless AllowLAN.
	OnLinkV6 []netip.Prefix
}

// IDRange is an inclusive range of uids.
type IDRange struct{ First, Last int }

func (r IDRange) String() string { return strconv.Itoa(r.First) + "-" + strconv.Itoa(r.Last) }

// owners are the socket uids the rules confine: the runner's uid, then each
// subordinate range, as nft and iptables write them.
func (p NetPolicy) owners() []string {
	out := []string{strconv.Itoa(p.UID)}
	for _, r := range p.SubUIDs {
		out = append(out, r.String())
	}
	return out
}

// errStaleNet means the loaded rules are the right kind but were written
// for an older picture of this host (a subordinate uid range, the Tailscale
// interface or an on-link IPv6 prefix changed). Start reloads them; any
// other checkNet error refuses the job.
type errStaleNet struct{ what string }

func (e *errStaleNet) Error() string {
	return "the loaded network rules for CI jobs are out of date: " + e.what
}

// netBackend is how the rules are loaded: "nft", or "iptables" when nft
// is missing.
type netBackend string

const (
	backendNft      netBackend = "nft"
	backendIptables netBackend = "iptables"
)

func (e *Executor) nftRules() string      { return filepath.Join(e.p.Lib, "net.nft") }
func (e *Executor) iptablesRules() string { return filepath.Join(e.p.Lib, "net-iptables.sh") }
func (e *Executor) netUnitPath() string   { return filepath.Join(e.p.Systemd, NetUnit) }

// tool returns the absolute path of a firewall or namespace tool, or "" when it is not
// installed. Paths overrides the lookup (tests).
func (e *Executor) tool(name string) string {
	p := map[string]string{"nft": e.p.Nft, "iptables": e.p.Iptables, "ip6tables": e.p.Ip6tables, "pasta": e.p.Pasta, "unshare": e.p.Unshare}[name]
	if p == "" {
		p, _ = lookPath(name)
	}
	if p == "" {
		return ""
	}
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return ""
	}
	return p
}

// pickBackend prefers nft, then iptables with ip6tables.
func (e *Executor) pickBackend() (netBackend, error) {
	if e.tool("nft") != "" {
		return backendNft, nil
	}
	if e.tool("iptables") != "" && e.tool("ip6tables") != "" {
		return backendIptables, nil
	}
	return "", errors.New("neither nft nor iptables and ip6tables is installed; CI jobs can't be kept off the tailnet and LAN without one (install nftables)")
}

// installedBackend is the backend whose rules file Install wrote.
func (e *Executor) installedBackend() (netBackend, bool) {
	if _, err := os.Stat(e.nftRules()); err == nil {
		return backendNft, true
	}
	if _, err := os.Stat(e.iptablesRules()); err == nil {
		return backendIptables, true
	}
	return "", false
}

// readResolvers returns the IP nameservers in a resolv.conf, without zoned
// (link-local) addresses, in file order.
func readResolvers(path string) []netip.Addr {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []netip.Addr
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(fields[1])
		if err != nil || a.Zone() != "" {
			continue
		}
		out = append(out, a.Unmap())
	}
	return out
}

// policy is this host's NetPolicy: the runner's uid, the stub resolver,
// every nameserver in resolv.conf and the upstream ones jobs query from
// their own namespace.
func (e *Executor) policy(uid int) NetPolicy {
	rs := []netip.Addr{StubResolver}
	for _, a := range append(readResolvers(e.p.ResolvConf), e.jobResolvers()...) {
		if !slices.Contains(rs, a) {
			rs = append(rs, a)
		}
	}
	return NetPolicy{UID: uid, SubUIDs: subIDRanges(e.p.Subuid, User), Resolvers: rs, AllowLAN: e.o.AllowLAN,
		TailscaleIfaces: tailscaleIfaces(e.p.IfInet6), OnLinkV6: onLinkV6(e.p.IPv6Route, tailscaleIfaces(e.p.IfInet6))}
}

// subIDRanges reads name's ranges from a subuid file, in file order.
func subIDRanges(file, name string) []IDRange {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []IDRange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(p) != 3 || p[0] != name {
			continue
		}
		first, err1 := strconv.Atoi(p[1])
		n, err2 := strconv.Atoi(p[2])
		if err1 != nil || err2 != nil || first < 0 || n <= 0 {
			continue
		}
		out = append(out, IDRange{first, first + n - 1})
	}
	return out
}

// procAddr6 parses the 32 hex digits /proc/net uses for an IPv6 address.
func procAddr6(s string) (netip.Addr, bool) {
	if len(s) != 32 {
		return netip.Addr{}, false
	}
	var b [16]byte
	for i := range b {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return netip.Addr{}, false
		}
		b[i] = byte(v)
	}
	return netip.AddrFrom16(b), true
}

// tailscaleIfaces is tailscale0 plus any interface that holds a tailnet
// IPv6 address (tailscaled under another --tun name), from
// /proc/net/if_inet6.
func tailscaleIfaces(ifInet6 string) []string {
	out := []string{TailscaleIface}
	b, err := os.ReadFile(ifInet6)
	if err != nil {
		return out
	}
	tail := netip.MustParsePrefix(TailnetV6)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		if a, ok := procAddr6(f[0]); ok && tail.Contains(a) && !slices.Contains(out, f[5]) {
			out = append(out, f[5])
		}
	}
	slices.Sort(out[1:])
	return out
}

// onLinkV6 is every global unicast IPv6 prefix the main routing table
// reaches without a gateway, outside the Tailscale interfaces and
// loopback, from /proc/net/ipv6_route. Sorted, without repeats.
func onLinkV6(ipv6Route string, tsIfaces []string) []netip.Prefix {
	// RTF_CACHE entries are per-destination exceptions (path MTU), not
	// prefixes; counting them would make the set churn. RTF_LOCAL and
	// RTF_ANYCAST are the host's own addresses, which the kernel lists on
	// the interface that holds them, not on lo; HostV6 covers those.
	const (
		rtfUp, rtfGateway, rtfReject = 0x1, 0x2, 0x200
		rtfAnycast, rtfCache         = 0x00100000, 0x01000000
		rtfLocal                     = 0x80000000
	)
	b, err := os.ReadFile(ipv6Route)
	if err != nil {
		return nil
	}
	global := netip.MustParsePrefix("2000::/3")
	var out []netip.Prefix
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[9] == "lo" || slices.Contains(tsIfaces, f[9]) {
			continue
		}
		dst, ok := procAddr6(f[0])
		bits, err1 := strconv.ParseUint(f[1], 16, 8)
		next, ok2 := procAddr6(f[4])
		flags, err2 := strconv.ParseUint(f[8], 16, 32)
		if !ok || !ok2 || err1 != nil || err2 != nil || bits == 0 || bits > 128 {
			continue
		}
		if flags&rtfUp == 0 || flags&(rtfGateway|rtfReject|rtfCache|rtfAnycast|rtfLocal) != 0 || !next.IsUnspecified() || !global.Contains(dst) {
			continue
		}
		pr, err := dst.Prefix(int(bits))
		if err == nil && !slices.Contains(out, pr) {
			out = append(out, pr)
		}
	}
	// nft merges a prefix inside another into the larger one, so the
	// listing would never show it and checkNet would call the rules stale.
	all := slices.Clone(out)
	out = slices.DeleteFunc(out, func(p netip.Prefix) bool {
		return slices.ContainsFunc(all, func(q netip.Prefix) bool {
			return q.Bits() < p.Bits() && q.Contains(p.Addr())
		})
	})
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return out
}

func prefixStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

func quoted(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Quote(x)
	}
	return out
}

func (p NetPolicy) resolvers(v6 bool) []string {
	var out []string
	for _, a := range p.Resolvers {
		if a.Is6() == v6 {
			out = append(out, a.String())
		}
	}
	return out
}

func nftSet(xs []string) string { return "{ " + strings.Join(xs, ", ") + " }" }

// NftRules is the nft script for p. Loading it replaces the table in one
// transaction, so rerunning it never leaves a moment without rules.
func NftRules(p NetPolicy) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	w("# Written by tentmaker setup; loaded at boot by " + NetUnit + ".")
	w("# CI jobs run as " + User + " (uid " + strconv.Itoa(p.UID) + ", rootless containers as its subordinate")
	w("# uids). They reach the internet, DNS and the source archive relay; never the")
	w("# tailnet, the LAN, multicast, link-local, this host's own addresses or the")
	w("# mesh port.")
	w("table inet " + NetTable)
	w("delete table inet " + NetTable)
	w("table inet " + NetTable + " {")
	w("\tchain output {")
	w("\t\ttype filter hook output priority filter; policy accept;")
	w("\t\tmeta skuid " + nftSet(p.owners()) + " jump runner")
	w("\t}")
	w("\tchain runner {")
	w("\t\tip daddr 127.0.0.1 tcp dport " + strconv.Itoa(RelayPort) + ` accept comment "source archive relay"`)
	for _, v6 := range []bool{false, true} {
		rs := p.resolvers(v6)
		if len(rs) == 0 {
			continue
		}
		fam := "ip"
		if v6 {
			fam = "ip6"
		}
		for _, proto := range []string{"udp", "tcp"} {
			w("\t\t" + fam + " daddr " + nftSet(rs) + " " + proto + " dport 53 accept comment \"DNS\"")
		}
	}
	for _, proto := range []string{"tcp", "udp"} {
		w("\t\t" + proto + " dport " + strconv.Itoa(MeshPort) + ` reject comment "mesh port"`)
	}
	w(`		oifname "lo" reject comment "this host"`)
	w("\t\tip daddr " + TailnetV4 + ` reject comment "tailnet"`)
	w("\t\tip6 daddr " + TailnetV6 + ` reject comment "tailnet"`)
	ts := nftSet(quoted(p.tailscaleIfaces()))
	w("\t\toifname " + ts + " ip daddr " + nftSet(TailscaleRoutedV4) + ` reject comment "tailnet subnet routes"`)
	w("\t\toifname " + ts + " ip6 daddr " + nftSet(TailscaleRoutedV6) + ` reject comment "tailnet subnet routes"`)
	w("\t\tip daddr " + nftSet(HostV4) + ` reject comment "loopback, link-local"`)
	w("\t\tip6 daddr " + nftSet(HostV6) + ` reject comment "loopback, unique local, link-local"`)
	w("\t\tip daddr " + nftSet(MulticastV4) + ` reject comment "multicast, broadcast"`)
	w("\t\tip6 daddr " + nftSet(MulticastV6) + ` reject comment "multicast"`)
	if !p.AllowLAN {
		w("\t\tip daddr " + nftSet(LANv4) + ` reject comment "LAN"`)
		if len(p.OnLinkV6) > 0 {
			w("\t\tip6 daddr " + nftSet(prefixStrings(p.OnLinkV6)) + ` reject comment "LAN (on-link global IPv6)"`)
		}
	}
	w("\t}")
	w("}")
	return b.String()
}

func (p NetPolicy) tailscaleIfaces() []string {
	if len(p.TailscaleIfaces) == 0 {
		return []string{TailscaleIface}
	}
	return p.TailscaleIfaces
}

// IptablesRules is the fallback shell script for p (iptables and
// ip6tables with the owner match), for hosts without nft.
func IptablesRules(p NetPolicy, iptables, ip6tables string) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	v4, v6 := shellQuote(iptables), shellQuote(ip6tables)
	uid := strconv.Itoa(p.UID)
	w("#!/bin/sh")
	w("# Written by tentmaker setup; loaded at boot by " + NetUnit + ". nft is missing,")
	w("# so this uses iptables. CI jobs run as " + User + " (uid " + uid + ", rootless")
	w("# containers as its subordinate uids). They reach the internet, DNS and the")
	w("# source archive relay; never the tailnet, the LAN, multicast, link-local, this")
	w("# host's own addresses or the mesh port.")
	w("set -eu")
	w("for t in " + v4 + " " + v6 + "; do")
	w("\t\"$t\" -w 5 -N " + NetChain + " 2>/dev/null || true")
	w("\t\"$t\" -w 5 -F " + NetChain)
	w("done")
	rule := func(t string, args ...string) { w(t + " -w 5 -A " + NetChain + " " + strings.Join(args, " ")) }
	rule(v4, "-d", "127.0.0.1/32", "-p", "tcp", "--dport", strconv.Itoa(RelayPort), "-j", "ACCEPT")
	for _, fam := range []struct {
		t  string
		v6 bool
	}{{v4, false}, {v6, true}} {
		for _, r := range p.resolvers(fam.v6) {
			for _, proto := range []string{"udp", "tcp"} {
				rule(fam.t, "-d", r, "-p", proto, "--dport", "53", "-j", "ACCEPT")
			}
		}
	}
	for _, t := range []string{v4, v6} {
		for _, proto := range []string{"tcp", "udp"} {
			rule(t, "-p", proto, "--dport", strconv.Itoa(MeshPort), "-j", "REJECT")
		}
		rule(t, "-o", "lo", "-j", "REJECT")
	}
	for _, ifc := range p.tailscaleIfaces() {
		for _, r := range TailscaleRoutedV4 {
			rule(v4, "-o", shellQuote(ifc), "-d", r, "-j", "REJECT")
		}
		for _, r := range TailscaleRoutedV6 {
			rule(v6, "-o", shellQuote(ifc), "-d", r, "-j", "REJECT")
		}
	}
	v4Ranges := append(append([]string{TailnetV4}, HostV4...), MulticastV4...)
	if !p.AllowLAN {
		v4Ranges = append(v4Ranges, LANv4...)
	}
	for _, r := range v4Ranges {
		rule(v4, "-d", r, "-j", "REJECT")
	}
	v6Ranges := append(append([]string{TailnetV6}, HostV6...), MulticastV6...)
	if !p.AllowLAN {
		v6Ranges = append(v6Ranges, prefixStrings(p.OnLinkV6)...)
	}
	for _, r := range v6Ranges {
		rule(v6, "-d", r, "-j", "REJECT")
	}
	w("for t in " + v4 + " " + v6 + "; do")
	w("\tfor o in " + strings.Join(p.owners(), " ") + "; do")
	w("\t\t\"$t\" -w 5 -C OUTPUT -m owner --uid-owner \"$o\" -j " + NetChain + " 2>/dev/null ||")
	w("\t\t\t\"$t\" -w 5 -I OUTPUT 1 -m owner --uid-owner \"$o\" -j " + NetChain)
	w("\tdone")
	w("done")
	return b.String()
}

// NetUnitFile is the boot unit that loads the rules.
func NetUnitFile(execStart string) string {
	return `# Written by tentmaker setup. Loads the network rules that keep CI jobs (user
# ` + User + `) off the tailnet, the LAN and this host's own services, before
# the Tentmaker daemon starts. The daemon also checks them before every job.
[Unit]
Description=Tentmaker: network rules for CI jobs
DefaultDependencies=no
After=local-fs.target
Before=network-pre.target tentmaker.service
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + execStart + `

[Install]
WantedBy=multi-user.target
`
}

// installNet writes the rules for this host, loads them, installs and
// enables the boot unit, and checks the loaded rules.
func (e *Executor) installNet(ctx context.Context, uid int) error {
	be, err := e.pickBackend()
	if err != nil {
		return err
	}
	exec, err := e.loadNet(ctx, uid, be)
	if err != nil {
		return err
	}
	if err := writeRoot(e.h, e.netUnitPath(), []byte(NetUnitFile(exec)), 0o644); err != nil {
		return err
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"daemon-reload"}, Timeout: time.Minute}); err != nil {
		return err
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"enable", NetUnit}, Timeout: time.Minute}); err != nil {
		return fmt.Errorf("enable %s: %v", NetUnit, err)
	}
	if err := e.checkNet(ctx, uid); err != nil {
		return fmt.Errorf("the network rules did not load: %v", err)
	}
	e.log.Info("network rules loaded", "executor", Name, "backend", string(be), "allow_lan", e.o.AllowLAN)
	return nil
}

// loadNet writes the rules file for backend be from this host's current
// facts and loads it. It returns the boot unit's command for that file.
func (e *Executor) loadNet(ctx context.Context, uid int, be netBackend) (string, error) {
	p := e.policy(uid)
	var file, exec string
	var data []byte
	var mode os.FileMode
	var load Cmd
	switch be {
	case backendNft:
		file, data, mode = e.nftRules(), []byte(NftRules(p)), 0o644
		exec = e.tool("nft") + " -f " + file
		load = Cmd{Path: e.tool("nft"), Args: []string{"-f", file}, Timeout: 30 * time.Second}
		if err := rmIfExists(e.iptablesRules()); err != nil {
			return "", err
		}
	default:
		file, data, mode = e.iptablesRules(), []byte(IptablesRules(p, e.tool("iptables"), e.tool("ip6tables"))), 0o755
		exec = "/bin/sh " + file
		load = Cmd{Path: "/bin/sh", Args: []string{file}, Timeout: time.Minute}
		if err := rmIfExists(e.nftRules()); err != nil {
			return "", err
		}
	}
	if err := writeRoot(e.h, file, data, mode); err != nil {
		return "", err
	}
	if _, err := e.h.Run(ctx, load); err != nil {
		return "", fmt.Errorf("load the network rules for CI jobs (%s): %v", be, err)
	}
	return exec, nil
}

// ensureNet is Start's gate: the rules must be loaded. Rules that are
// loaded but out of date for this host (errStaleNet) are rewritten and
// reloaded once; rules that are missing refuse the job.
func (e *Executor) ensureNet(ctx context.Context, uid int) error {
	err := e.checkNet(ctx, uid)
	var stale *errStaleNet
	if !errors.As(err, &stale) {
		return err
	}
	be, _ := e.installedBackend()
	e.log.Warn("reloading the network rules for CI jobs", "executor", Name, "why", stale.what)
	if _, lerr := e.loadNet(ctx, uid, be); lerr != nil {
		return lerr
	}
	return e.checkNet(ctx, uid)
}

// checkNet fails unless the rules Install wrote are loaded and block at
// least what the config and this host ask for. A missing table, chain or
// jump for the runner's own uid is a plain error: nothing reloads it
// behind the admin's back, and the job is refused. A table that is there
// but lacks a piece (a new subordinate uid range, Tailscale interface or
// on-link prefix, or a rule an older version did not write) is an
// *errStaleNet.
func (e *Executor) checkNet(ctx context.Context, uid int) error {
	be, ok := e.installedBackend()
	if !ok {
		return errors.New("the network rules for CI jobs are not installed")
	}
	p := e.policy(uid)
	if be == backendNft {
		nft := e.tool("nft")
		if nft == "" {
			return errors.New("nft is gone, so the network rules for CI jobs can't be checked")
		}
		res, err := e.h.Run(ctx, Cmd{Path: nft, Args: []string{"list", "table", "inet", NetTable}, Timeout: 30 * time.Second})
		if err != nil {
			return errors.New("the network rules for CI jobs are not loaded (nft table inet " + NetTable + " is missing)")
		}
		return checkNft(res.Stdout, p)
	}
	v4, v6 := iptWants(p)
	for _, t := range []struct {
		tool  string
		wants []iptRule
	}{{"iptables", v4}, {"ip6tables", v6}} {
		bin := e.tool(t.tool)
		if bin == "" {
			return errors.New(t.tool + " is gone, so the network rules for CI jobs can't be checked")
		}
		out, err := e.h.Run(ctx, Cmd{Path: bin, Args: []string{"-w", "5", "-S", "OUTPUT"}, Timeout: 30 * time.Second})
		jumps := map[string]bool{}
		if err == nil {
			for _, r := range parseIpt(out.Stdout) {
				if r.j == NetChain && r.uid != "" {
					jumps[r.uid] = true
				}
			}
		}
		if !jumps[strconv.Itoa(uid)] {
			return errors.New("the network rules for CI jobs are not loaded (" + t.tool + " OUTPUT has no jump to " + NetChain + ")")
		}
		chain, err := e.h.Run(ctx, Cmd{Path: bin, Args: []string{"-w", "5", "-S", NetChain}, Timeout: 30 * time.Second})
		if err != nil {
			return errors.New("the network rules for CI jobs are not loaded (" + t.tool + " has no " + NetChain + " chain)")
		}
		var miss []string
		for _, o := range p.owners()[1:] {
			if !jumps[o] {
				miss = append(miss, "the jump for uids "+o)
			}
		}
		have := parseIpt(chain.Stdout)
		for _, w := range t.wants {
			if !slices.ContainsFunc(have, w.matchedBy) {
				miss = append(miss, w.String())
			}
		}
		var dns []string
		for _, r := range have {
			if r.dport == "53" && r.j == "ACCEPT" {
				dns = append(dns, normElem(r.d))
			}
		}
		var rs []netip.Addr
		for _, a := range p.Resolvers {
			if a.Is6() == (t.tool == "ip6tables") {
				rs = append(rs, a)
			}
		}
		if d := dnsDiff(dns, rs); d != "" {
			miss = append(miss, d)
		}
		if len(miss) > 0 {
			return &errStaleNet{t.tool + " lacks " + strings.Join(miss, ", ")}
		}
	}
	return nil
}

// dnsDiff describes how the DNS servers the loaded rules allow (have, as
// listed) differ from want, or is "" when they are the same set.
func dnsDiff(have []string, want []netip.Addr) string {
	h := map[string]bool{}
	for _, a := range have {
		h[normElem(a)] = true
	}
	w := map[string]bool{}
	for _, a := range want {
		w[a.String()] = true
	}
	if maps.Equal(h, w) {
		return ""
	}
	return fmt.Sprintf("the DNS servers %s (the rules allow %s)", orNone(slices.Sorted(maps.Keys(w))), orNone(slices.Sorted(maps.Keys(h))))
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, " ")
}

// normElem spells an address or range the way both the rules file and the
// firewall's listing can be compared: a single-address prefix as the bare
// address.
func normElem(s string) string {
	if pr, err := netip.ParsePrefix(s); err == nil && pr.IsSingleIP() {
		return pr.Addr().String()
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String()
	}
	return s
}

var nftComment = regexp.MustCompile(`comment "([^"]*)"`)

// checkNft checks an nft listing (or the rules file itself) against p.
// nft sorts set elements and spells a single address without its length,
// so each commented rule is compared as a set of elements.
func checkNft(out string, p NetPolicy) error {
	rules := map[string]map[string]bool{}
	var owners map[string]bool
	for _, line := range strings.Split(out, "\n") {
		key := ""
		if m := nftComment.FindStringSubmatch(line); m != nil {
			key = m[1]
			line = strings.Replace(line, m[0], "", 1)
		} else if strings.Contains(line, "skuid") && strings.Contains(line, "jump runner") {
			owners = map[string]bool{}
			for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '{' || r == '}' || r == ',' }) {
				owners[f] = true
			}
			continue
		} else {
			continue
		}
		if rules[key] == nil {
			rules[key] = map[string]bool{}
		}
		for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '{' || r == '}' || r == ',' }) {
			rules[key][normElem(f)] = true
		}
	}
	if !strings.Contains(out, "hook output") || owners == nil {
		return errors.New("the network rules for CI jobs are not loaded (nft table inet " + NetTable + " has no output hook for " + User + ")")
	}
	if !owners[strconv.Itoa(p.UID)] && !owners[strconv.Quote(User)] {
		return fmt.Errorf("the loaded network rules don't match uid %d of %s", p.UID, User)
	}
	var miss []string
	for _, o := range p.owners()[1:] {
		if !owners[o] {
			miss = append(miss, "uids "+o)
		}
	}
	want := func(rule string, elems ...string) {
		for _, el := range elems {
			if !rules[rule][normElem(el)] {
				miss = append(miss, el+" in the "+strconv.Quote(rule)+" rule")
			}
		}
	}
	want("source archive relay", strconv.Itoa(RelayPort), "accept")
	want("mesh port", strconv.Itoa(MeshPort), "tcp", "udp", "reject")
	want("this host", `"lo"`, "reject")
	want("tailnet", TailnetV4, TailnetV6, "reject")
	want("tailnet subnet routes", append(append(quoted(p.tailscaleIfaces()), TailscaleRoutedV4...), TailscaleRoutedV6...)...)
	want("loopback, link-local", HostV4...)
	want("loopback, unique local, link-local", HostV6...)
	want("multicast, broadcast", MulticastV4...)
	want("multicast", MulticastV6...)
	// The DNS servers must be exactly the host's now: a laptop on a new
	// network has a new DHCP resolver, which the LAN rule would reject.
	var dns []string
	for el := range rules["DNS"] {
		if a, err := netip.ParseAddr(el); err == nil {
			dns = append(dns, a.String())
		}
	}
	if d := dnsDiff(dns, p.Resolvers); d != "" {
		miss = append(miss, d)
	}
	if !p.AllowLAN {
		want("LAN", LANv4...)
		if len(p.OnLinkV6) > 0 {
			want("LAN (on-link global IPv6)", prefixStrings(p.OnLinkV6)...)
		}
	}
	if len(miss) > 0 {
		return &errStaleNet{"nft lacks " + strings.Join(miss, ", ")}
	}
	return nil
}

// iptRule is the part of an iptables -S line that checkNet compares; an
// empty field matches anything.
type iptRule struct{ d, o, dport, j, uid string }

func (w iptRule) matchedBy(r iptRule) bool {
	eq := func(want, got string) bool { return want == "" || want == got }
	return eq(w.d, r.d) && eq(w.o, r.o) && eq(w.dport, r.dport) && eq(w.j, r.j) && eq(w.uid, r.uid)
}

func (w iptRule) String() string {
	var f []string
	for _, kv := range [][2]string{{"-d", w.d}, {"-o", w.o}, {"--dport", w.dport}, {"-j", w.j}} {
		if kv[1] != "" {
			f = append(f, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(f, " ")
}

// parseIpt reads iptables -S output (flags in any order, values possibly
// shell quoted as in the rules file).
func parseIpt(out string) []iptRule {
	var rs []iptRule
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		var r iptRule
		for i := 0; i+1 < len(f); i++ {
			v := strings.Trim(f[i+1], `'"`)
			switch f[i] {
			case "-d":
				r.d = normElem(v)
			case "-o":
				r.o = v
			case "--dport":
				r.dport = v
			case "-j":
				r.j = v
			case "--uid-owner":
				r.uid = v
			}
		}
		if r != (iptRule{}) {
			rs = append(rs, r)
		}
	}
	return rs
}

// iptWants are the rules the TENTMAKER chains must hold for p.
func iptWants(p NetPolicy) (v4, v6 []iptRule) {
	mesh := strconv.Itoa(MeshPort)
	for _, set := range []*[]iptRule{&v4, &v6} {
		*set = append(*set, iptRule{dport: mesh, j: "REJECT"}, iptRule{o: "lo", j: "REJECT"})
	}
	for _, ifc := range p.tailscaleIfaces() {
		for _, r := range TailscaleRoutedV4 {
			v4 = append(v4, iptRule{d: normElem(r), o: ifc, j: "REJECT"})
		}
		for _, r := range TailscaleRoutedV6 {
			v6 = append(v6, iptRule{d: normElem(r), o: ifc, j: "REJECT"})
		}
	}
	r4 := append(append([]string{TailnetV4}, HostV4...), MulticastV4...)
	r6 := append(append([]string{TailnetV6}, HostV6...), MulticastV6...)
	if !p.AllowLAN {
		r4 = append(r4, LANv4...)
		r6 = append(r6, prefixStrings(p.OnLinkV6)...)
	}
	for _, r := range r4 {
		v4 = append(v4, iptRule{d: normElem(r), j: "REJECT"})
	}
	for _, r := range r6 {
		v6 = append(v6, iptRule{d: normElem(r), j: "REJECT"})
	}
	return v4, v6
}

// netProblems is Preflight's view: the rules file, the boot unit, and the
// loaded rules.
func (e *Executor) netProblems(ctx context.Context, uid int) []string {
	var out []string
	if _, ok := e.installedBackend(); !ok {
		return []string{"the network rules for CI jobs are not installed, so a job could reach the tailnet and LAN"}
	}
	if _, err := os.Stat(e.netUnitPath()); err != nil {
		out = append(out, e.netUnitPath()+" is missing, so the network rules won't load after a reboot")
	} else if res, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"is-enabled", NetUnit}, Timeout: 30 * time.Second}); err != nil || strings.TrimSpace(res.Stdout) != "enabled" {
		out = append(out, NetUnit+" is not enabled, so the network rules won't load after a reboot")
	}
	if err := e.checkNet(ctx, uid); err != nil {
		out = append(out, err.Error())
	}
	return out
}

// uninstallNet unloads the rules and removes the boot unit. Safe to rerun.
func (e *Executor) uninstallNet(ctx context.Context) []error {
	var errs []error
	if _, err := os.Stat(e.netUnitPath()); err == nil {
		e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"disable", NetUnit}, Timeout: time.Minute})
		if err := rmIfExists(e.netUnitPath()); err != nil {
			errs = append(errs, err)
		}
	}
	if nft := e.tool("nft"); nft != "" {
		res, err := e.h.Run(ctx, Cmd{Path: nft, Args: []string{"list", "tables"}, Timeout: 30 * time.Second})
		if err == nil && hasLine(res.Stdout, "table inet "+NetTable) {
			if _, err := e.h.Run(ctx, Cmd{Path: nft, Args: []string{"delete", "table", "inet", NetTable}, Timeout: 30 * time.Second}); err != nil {
				errs = append(errs, fmt.Errorf("remove the nft table: %v", err))
			}
		}
	}
	for _, t := range []string{"iptables", "ip6tables"} {
		bin := e.tool(t)
		if bin == "" {
			continue
		}
		res, err := e.h.Run(ctx, Cmd{Path: bin, Args: []string{"-w", "5", "-S", "OUTPUT"}, Timeout: 30 * time.Second})
		if err != nil {
			continue
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			f := strings.Fields(line)
			if len(f) > 2 && f[0] == "-A" && slices.Contains(f, "-j") && f[len(f)-1] == NetChain {
				args := append([]string{"-w", "5", "-D"}, f[1:]...)
				if _, err := e.h.Run(ctx, Cmd{Path: bin, Args: args, Timeout: 30 * time.Second}); err != nil {
					errs = append(errs, fmt.Errorf("remove the %s jump: %v", t, err))
				}
			}
		}
		if _, err := e.h.Run(ctx, Cmd{Path: bin, Args: []string{"-w", "5", "-S", NetChain}, Timeout: 30 * time.Second}); err == nil {
			for _, op := range []string{"-F", "-X"} {
				if _, err := e.h.Run(ctx, Cmd{Path: bin, Args: []string{"-w", "5", op, NetChain}, Timeout: 30 * time.Second}); err != nil {
					errs = append(errs, fmt.Errorf("remove the %s chain: %v", t, err))
				}
			}
		}
	}
	for _, f := range []string{e.nftRules(), e.iptablesRules()} {
		if err := rmIfExists(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func hasLine(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}
