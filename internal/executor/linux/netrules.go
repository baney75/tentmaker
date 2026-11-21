package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Network isolation for the runner user. Every CI process, rootless
// containers included (pasta and slirp4netns open their sockets as the
// runner's uid), sends through the output hook with that socket uid, so a
// filter on it confines all of them. The job may reach the internet, DNS and
// the source archive relay; never the tailnet, the LAN, link-local
// (cloud metadata), this host's own addresses, or the mesh port anywhere.

// NetTable is the nftables table (family inet) that holds the rules.
const NetTable = "tentmaker"

// NetUnit loads the rules at boot, before tentmaker.service.
const NetUnit = "tentmaker-net.service"

// NetChain is the iptables chain the fallback uses when nft is missing.
const NetChain = "TENTMAKER"

// RelayPort is the control API's loopback port, where check jobs fetch their
// source archive. MeshPort is the peer API's port on every device.
const (
	RelayPort = 7434
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
)

// StubResolver is systemd-resolved's stub, always allowed for DNS.
var StubResolver = netip.MustParseAddr("127.0.0.53")

// NetPolicy is what the rules say for one host.
type NetPolicy struct {
	UID       int
	Resolvers []netip.Addr // DNS servers jobs may query on port 53
	AllowLAN  bool
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

// tool returns the absolute path of a firewall tool, or "" when it is not
// installed. Paths overrides the lookup (tests).
func (e *Executor) tool(name string) string {
	p := map[string]string{"nft": e.p.Nft, "iptables": e.p.Iptables, "ip6tables": e.p.Ip6tables}[name]
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

// policy is this host's NetPolicy: the runner's uid, the stub resolver and
// every nameserver in resolv.conf.
func (e *Executor) policy(uid int) NetPolicy {
	rs := []netip.Addr{StubResolver}
	for _, a := range readResolvers(e.p.ResolvConf) {
		if !slices.Contains(rs, a) {
			rs = append(rs, a)
		}
	}
	return NetPolicy{UID: uid, Resolvers: rs, AllowLAN: e.o.AllowLAN}
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
	w("# CI jobs run as " + User + " (uid " + strconv.Itoa(p.UID) + "). They reach the internet, DNS and")
	w("# the source archive relay; never the tailnet, the LAN, link-local, this host's")
	w("# own addresses or the mesh port.")
	w("table inet " + NetTable)
	w("delete table inet " + NetTable)
	w("table inet " + NetTable + " {")
	w("\tchain output {")
	w("\t\ttype filter hook output priority filter; policy accept;")
	w("\t\tmeta skuid " + strconv.Itoa(p.UID) + " jump runner")
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
	w("\t\tip daddr " + nftSet(HostV4) + ` reject comment "loopback, link-local"`)
	w("\t\tip6 daddr " + nftSet(HostV6) + ` reject comment "loopback, unique local, link-local"`)
	if !p.AllowLAN {
		w("\t\tip daddr " + nftSet(LANv4) + ` reject comment "LAN"`)
	}
	w("\t}")
	w("}")
	return b.String()
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
	w("# so this uses iptables. CI jobs run as " + User + " (uid " + uid + "). They reach the")
	w("# internet, DNS and the source archive relay; never the tailnet, the LAN,")
	w("# link-local, this host's own addresses or the mesh port.")
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
	v4Ranges := append([]string{TailnetV4}, HostV4...)
	if !p.AllowLAN {
		v4Ranges = append(v4Ranges, LANv4...)
	}
	for _, r := range v4Ranges {
		rule(v4, "-d", r, "-j", "REJECT")
	}
	for _, r := range append([]string{TailnetV6}, HostV6...) {
		rule(v6, "-d", r, "-j", "REJECT")
	}
	w("for t in " + v4 + " " + v6 + "; do")
	w("\t\"$t\" -w 5 -C OUTPUT -m owner --uid-owner " + uid + " -j " + NetChain + " 2>/dev/null ||")
	w("\t\t\"$t\" -w 5 -I OUTPUT 1 -m owner --uid-owner " + uid + " -j " + NetChain)
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
			return err
		}
	default:
		file, data, mode = e.iptablesRules(), []byte(IptablesRules(p, e.tool("iptables"), e.tool("ip6tables"))), 0o755
		exec = "/bin/sh " + file
		load = Cmd{Path: "/bin/sh", Args: []string{file}, Timeout: time.Minute}
		if err := rmIfExists(e.nftRules()); err != nil {
			return err
		}
	}
	if err := writeRoot(e.h, file, data, mode); err != nil {
		return err
	}
	if err := writeRoot(e.h, e.netUnitPath(), []byte(NetUnitFile(exec)), 0o644); err != nil {
		return err
	}
	if _, err := e.h.Run(ctx, load); err != nil {
		return fmt.Errorf("load the network rules for CI jobs (%s): %v", be, err)
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
		return err
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"enable", NetUnit}}); err != nil {
		return fmt.Errorf("enable %s: %v", NetUnit, err)
	}
	if err := e.checkNet(ctx, uid); err != nil {
		return fmt.Errorf("the network rules did not load: %v", err)
	}
	e.log.Info("network rules loaded", "executor", Name, "backend", string(be), "allow_lan", p.AllowLAN)
	return nil
}

// checkNet fails unless the rules Install wrote are loaded and block at
// least what the config asks for. A job starts only after it passes.
func (e *Executor) checkNet(ctx context.Context, uid int) error {
	be, ok := e.installedBackend()
	if !ok {
		return errors.New("the network rules for CI jobs are not installed")
	}
	p := e.policy(uid)
	need := func(out string, ws ...string) error {
		var miss []string
		for _, w := range ws {
			if !strings.Contains(out, w) {
				miss = append(miss, w)
			}
		}
		if len(miss) > 0 {
			return fmt.Errorf("the loaded network rules for CI jobs lack %s", strings.Join(miss, ", "))
		}
		return nil
	}
	if be == backendNft {
		nft := e.tool("nft")
		if nft == "" {
			return errors.New("nft is gone, so the network rules for CI jobs can't be checked")
		}
		res, err := e.h.Run(ctx, Cmd{Path: nft, Args: []string{"list", "table", "inet", NetTable}, Timeout: 30 * time.Second})
		if err != nil {
			return errors.New("the network rules for CI jobs are not loaded (nft table inet " + NetTable + " is missing)")
		}
		out := res.Stdout
		if !strings.Contains(out, "skuid "+strconv.Itoa(uid)+" ") && !strings.Contains(out, `skuid "`+User+`"`) {
			return fmt.Errorf("the loaded network rules don't match uid %d of %s", uid, User)
		}
		ws := []string{"hook output", "jump runner", "dport " + strconv.Itoa(MeshPort) + " reject", `oifname "lo" reject`,
			TailnetV4, TailnetV6}
		ws = append(ws, HostV4[1:]...)
		ws = append(ws, "fc00::/7", "fe80::/10")
		if !p.AllowLAN {
			ws = append(ws, LANv4...)
		}
		return need(out, ws...)
	}
	for _, t := range []struct {
		tool   string
		ranges []string
	}{
		{"iptables", append(append([]string{TailnetV4}, HostV4[1:]...), lanIf(!p.AllowLAN)...)},
		{"ip6tables", []string{TailnetV6, "fc00::/7", "fe80::/10"}},
	} {
		bin := e.tool(t.tool)
		if bin == "" {
			return errors.New(t.tool + " is gone, so the network rules for CI jobs can't be checked")
		}
		out, err := e.h.Run(ctx, Cmd{Path: bin, Args: []string{"-w", "5", "-S", "OUTPUT"}, Timeout: 30 * time.Second})
		if err != nil || !strings.Contains(out.Stdout, "--uid-owner "+strconv.Itoa(uid)+" -j "+NetChain) {
			return errors.New("the network rules for CI jobs are not loaded (" + t.tool + " OUTPUT has no jump to " + NetChain + ")")
		}
		chain, err := e.h.Run(ctx, Cmd{Path: bin, Args: []string{"-w", "5", "-S", NetChain}, Timeout: 30 * time.Second})
		if err != nil {
			return errors.New("the network rules for CI jobs are not loaded (" + t.tool + " has no " + NetChain + " chain)")
		}
		ws := []string{"--dport " + strconv.Itoa(MeshPort) + " -j REJECT", "-o lo -j REJECT"}
		for _, r := range t.ranges {
			ws = append(ws, "-d "+r+" -j REJECT")
		}
		if err := need(chain.Stdout, ws...); err != nil {
			return err
		}
	}
	return nil
}

func lanIf(on bool) []string {
	if on {
		return LANv4
	}
	return nil
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
