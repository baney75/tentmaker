package linux

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
)

// Each job runs in a network namespace of its own, so it has its own
// loopback: a server a job starts on 127.0.0.1 or ::1 answers that job, and
// nothing on the host's loopback does, but for the source relay.
//
// Two units per job set it up, both as the runner user in tentmaker.slice:
//
//   - tentmaker-netns-NAME holds the namespace: unshare makes a user
//     namespace the runner owns and a network namespace in it. The runner
//     must own it, or pasta (which runs as the runner) could not join it.
//   - tentmaker-uplink-NAME is pasta, the namespace's only way out. pasta
//     joins the holder's namespaces and opens every outside socket as the
//     runner, so the per-uid rules (netrules.go) still decide what a job
//     reaches. It forwards one port into the host: the job's own
//     127.0.0.1:RelayPort goes to the host's relay. It maps no other host
//     loopback port (--no-map-gw, -T only the relay) and publishes no job
//     port on the host (-t none, -u none).
//
// The job unit enters the namespace with NetworkNamespacePath= and gets a
// resolv.conf of the host's real resolvers, because the 127.0.0.53 stub is
// on the host's loopback.

// NetnsPrefix and UplinkPrefix start the two units of a job's namespace.
const (
	NetnsPrefix  = "tentmaker-netns-"
	UplinkPrefix = "tentmaker-uplink-"
)

func netnsUnit(name string) string  { return NetnsPrefix + name + ".service" }
func uplinkUnit(name string) string { return UplinkPrefix + name + ".service" }

// jobResolvConf is the resolv.conf every job sees.
func (e *Executor) jobResolvConf() string { return filepath.Join(e.priv, "resolv.conf") }

// PastaArgs is pasta's argv (after the binary) for the namespace held by
// process pid.
func PastaArgs(pid int) []string {
	return []string{"--config-net", "--quiet",
		// No host loopback but the relay: the gateway address stays the
		// gateway, and only RelayPort is forwarded to the host.
		"--no-map-gw", "-T", strconv.Itoa(RelayPort), "-U", "none",
		// No job port is published on the host.
		"-t", "none", "-u", "none",
		strconv.Itoa(pid)}
}

// jobResolvers are the DNS servers a job can reach from its namespace:
// resolv.conf's, or, when that names only a loopback stub, the upstream
// servers systemd-resolved lists.
func (e *Executor) jobResolvers() []netip.Addr {
	pick := func(path string) []netip.Addr {
		var out []netip.Addr
		for _, a := range readResolvers(path) {
			if !a.IsLoopback() && !a.IsUnspecified() {
				out = append(out, a)
			}
		}
		return out
	}
	if rs := pick(e.p.ResolvConf); len(rs) > 0 {
		return rs
	}
	return pick(e.p.ResolvedConf)
}

// writeJobResolvConf writes the resolv.conf jobs see. It fails when there is
// no DNS server a job could reach.
func (e *Executor) writeJobResolvConf() error {
	rs := e.jobResolvers()
	if len(rs) == 0 {
		return fmt.Errorf("no DNS server for CI jobs: %s names none but loopback and %s is missing or empty", e.p.ResolvConf, e.p.ResolvedConf)
	}
	var b strings.Builder
	b.WriteString("# Written by tentmaker for CI jobs.\n")
	for _, a := range rs {
		b.WriteString("nameserver " + a.String() + "\n")
	}
	if err := os.MkdirAll(e.priv, 0o700); err != nil {
		return err
	}
	return config.WriteAtomic(e.jobResolvConf(), []byte(b.String()), 0o644)
}

// netnsTools checks the tools a job's namespace needs.
func (e *Executor) netnsTools() error {
	if e.tool("pasta") == "" {
		return errors.New("pasta is not installed; CI jobs need it for a network namespace of their own (install the passt package)")
	}
	if e.tool("unshare") == "" {
		return errors.New("unshare is not installed; CI jobs need it for a network namespace of their own (install util-linux)")
	}
	return nil
}

// helperArgs is the systemd-run argv for one of a job's namespace units,
// with props as extra unit properties.
func helperArgs(unit, typ string, uid, gid int, props []string, cmd ...string) []string {
	args := []string{"--quiet", "--no-ask-password",
		"--unit=" + unit, "--slice=" + Slice,
		"--uid=" + strconv.Itoa(uid), "--gid=" + strconv.Itoa(gid),
		"-p", "Type=" + typ,
		"-p", "NoNewPrivileges=yes",
		"-p", "TimeoutStartSec=30",
		"-p", "TimeoutStopSec=10"}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	return append(append(args, "--"), cmd...)
}

// holderProps lets the holder make its user namespace where AppArmor
// restricts unprivileged ones (kernel.apparmor_restrict_unprivileged_userns,
// on by default since Ubuntu 24.04): there unshare cannot write its uid_map
// ("Operation not permitted") unless it holds CAP_SYS_ADMIN. The holder
// keeps the runner's uid, so the runner still owns the namespace and pasta
// can join it, and once unshare is inside, its capabilities count only
// there. CAP_SYS_ADMIN is the one capability it gets.
var holderProps = []string{"AmbientCapabilities=CAP_SYS_ADMIN", "CapabilityBoundingSet=CAP_SYS_ADMIN"}

// startNetns starts a job's namespace and its uplink and returns the path the
// job unit enters. On error nothing is left running.
func (e *Executor) startNetns(ctx context.Context, name string, uid, gid int) (_ string, err error) {
	if err := e.netnsTools(); err != nil {
		return "", err
	}
	if err := e.writeJobResolvConf(); err != nil {
		return "", err
	}
	e.stopNetns(ctx, name) // left over from a crash
	defer func() {
		if err != nil {
			e.stopNetns(context.WithoutCancel(ctx), name)
		}
	}()
	return e.startNetUnits(ctx, netnsUnit(name), uplinkUnit(name), uid, gid)
}

// startNetUnits starts hold, a namespace holder, and up, its pasta uplink,
// both as the runner, and returns the path of the holder's network
// namespace. On error the caller stops both.
func (e *Executor) startNetUnits(ctx context.Context, hold, up string, uid, gid int) (string, error) {
	args := helperArgs(hold, "exec", uid, gid, holderProps,
		e.tool("unshare"), "--user", "--map-root-user", "--net", "--", "sleep", "infinity")
	if _, err := e.h.Run(ctx, Cmd{Path: "systemd-run", Args: args, Timeout: time.Minute}); err != nil {
		return "", fmt.Errorf("start %s: %v", hold, err)
	}
	pid, err := e.netnsPID(ctx, hold)
	if err != nil {
		return "", err
	}
	args = helperArgs(up, "forking", uid, gid, nil, append([]string{e.tool("pasta")}, PastaArgs(pid)...)...)
	if _, err := e.h.Run(ctx, Cmd{Path: "systemd-run", Args: args, Timeout: time.Minute}); err != nil {
		return "", fmt.Errorf("start %s: %v", up, err)
	}
	// The holder must still be the process pasta joined.
	if again, err := e.netnsPID(ctx, hold); err != nil || again != pid {
		return "", fmt.Errorf("%s ended while its uplink started", hold)
	}
	return filepath.Join(e.p.Proc, strconv.Itoa(pid), "ns", "net"), nil
}

// netnsPID waits until the holder unit's process has left the host's network
// namespace and returns its pid. Until then a unit entering it would share
// the host's.
func (e *Executor) netnsPID(ctx context.Context, unit string) (int, error) {
	host, err := os.Readlink(filepath.Join(e.p.Proc, "self", "ns", "net"))
	if err != nil {
		return 0, fmt.Errorf("read this host's network namespace: %v", err)
	}
	for i := 0; i < 100; i++ {
		pid, err := e.mainPID(ctx, unit)
		if err != nil {
			return 0, err
		}
		if pid > 0 {
			ns, err := os.Readlink(filepath.Join(e.p.Proc, strconv.Itoa(pid), "ns", "net"))
			if err == nil && ns != host {
				return pid, nil
			}
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		e.o.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("%s did not make a network namespace in 10s%s", unit, e.unitLog(ctx, unit))
}

// mainPID is a unit's main process, or 0 when it has none.
func (e *Executor) mainPID(ctx context.Context, unit string) (int, error) {
	res, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"show", "--property=MainPID", "--value", unit}, Timeout: 30 * time.Second})
	if err != nil {
		return 0, fmt.Errorf("read the pid of %s: %v", unit, err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(res.Stdout))
	return pid, nil
}

// unitLog is the tail of a unit's journal, for an error that says why the
// unit failed, or "" when there is none.
func (e *Executor) unitLog(ctx context.Context, unit string) string {
	res, err := e.h.Run(ctx, Cmd{Path: "journalctl", Args: []string{"--no-pager", "--output=cat", "--lines=5", "--unit=" + unit}, Timeout: 10 * time.Second})
	out := strings.Join(strings.Fields(res.Stdout), " ")
	if err != nil || out == "" || strings.HasPrefix(out, "-- No entries --") {
		return ""
	}
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return " (its log: " + out + ")"
}

// stopNetns stops a job's uplink and namespace.
func (e *Executor) stopNetns(ctx context.Context, name string) {
	e.stopUnits(ctx, uplinkUnit(name), netnsUnit(name))
}

// stopUnits stops units in order and forgets their failures. Units that
// aren't loaded make stop fail; that is fine.
func (e *Executor) stopUnits(ctx context.Context, units ...string) {
	for _, u := range units {
		e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"stop", u}, Timeout: 30 * time.Second})
		e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"reset-failed", u}, Timeout: 30 * time.Second})
	}
}

// netnsUnits lists every loaded namespace or uplink unit.
func (e *Executor) netnsUnits(ctx context.Context) ([]string, error) {
	res, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"list-units", "--all", "--plain", "--no-legend", "--type=service", NetnsPrefix + "*", UplinkPrefix + "*"}, Timeout: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("list job namespace units: %v", err)
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if f := strings.Fields(line); len(f) > 0 && (strings.HasPrefix(f[0], NetnsPrefix) || strings.HasPrefix(f[0], UplinkPrefix)) {
			out = append(out, f[0])
		}
	}
	return out, nil
}
