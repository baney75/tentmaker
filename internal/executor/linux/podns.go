package linux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PodnsUnit holds rootless podman's pause process. Rootless podman runs every
// command, a job's included, in the user and mount namespaces of that
// process, not in the caller's. Made anywhere else (Reset's own podman
// calls, run by runuser), those namespaces would show a job what its unit
// hides: the user bus, through which it could start a process outside its
// unit and network namespace, the manager's socket and tailscaled's; and the
// host's resolv.conf, whose 127.0.0.53 stub a job's namespace cannot reach,
// so no image would pull. Reset starts this unit before podman runs at all,
// with the mounts a job's unit gets, so the pause process is born there.
const PodnsUnit = "tentmaker-podns.service"

// Podman's namespaces get a network namespace of their own, held and linked
// out the way a job's is (netns.go): PodnetUnit holds it, PodnetUplinkUnit is
// its pasta. The runner's user manager runs in it, and so podman.service and
// every container it starts: a container on the host network (docker run
// --network=host through DOCKER_HOST) gets this namespace, not the host's.
// Rootless podman joins only the pause process's user and mount namespaces,
// never its network namespace, so PodnsUnit alone would not move
// podman.service; the manager needs NetworkNamespacePath= too. Its
// containers can then neither reach the abstract Unix sockets of host
// services nor listen on the host's addresses, and everything they send
// still leaves through pasta as the runner, where the per-uid rules match
// it. The namespace is one per device: every job's containers that the
// service starts share it.
const (
	PodnetUnit       = "tentmaker-podnet.service"
	PodnetUplinkUnit = "tentmaker-podnet-uplink.service"
)

// managerDropIn is the runtime drop-in that puts the runner's user manager
// in podman's network namespace. It lives under /run, so it never outlives
// the holder's pid across a reboot.
func (e *Executor) managerDropIn(uid int) string {
	return filepath.Join(e.p.SystemdRunning, "user@"+strconv.Itoa(uid)+".service.d", "60-tentmaker-netns.conf")
}

// startPodman stops the runner's user manager and every podman process,
// makes podman's network namespace afresh, and starts the user manager and
// then PodnsUnit in it. Install and Reset run it before any podman command.
func (e *Executor) startPodman(ctx context.Context, uid, gid int) error {
	if err := e.netnsTools(); err != nil {
		return err
	}
	if err := e.writeJobResolvConf(); err != nil {
		return err
	}
	e.stopPodns(ctx)
	u := strconv.Itoa(uid)
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"stop", "user@" + u + ".service", "user-runtime-dir@" + u + ".service"}, Timeout: 2 * time.Minute}); err != nil {
		return err
	}
	e.stopPodnet(ctx)
	ns, err := e.startNetUnits(ctx, PodnetUnit, PodnetUplinkUnit, uid, gid)
	if err != nil {
		e.stopPodnet(context.WithoutCancel(ctx))
		return fmt.Errorf("%v; podman runs only in a network namespace of its own", err)
	}
	drop := e.managerDropIn(uid)
	if err := os.MkdirAll(filepath.Dir(drop), 0o755); err != nil {
		return err
	}
	text := "# Written by tentmaker. The CI user's manager, podman.service and its\n" +
		"# containers run in podman's network namespace, not the host's.\n" +
		"[Service]\nNetworkNamespacePath=" + ns + "\n"
	if err := writeRoot(e.h, drop, []byte(text), 0o644); err != nil {
		return err
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"daemon-reload"}, Timeout: time.Minute}); err != nil {
		return err
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"start", "user@" + u + ".service"}, Timeout: 2 * time.Minute}); err != nil {
		return err
	}
	return e.startPodns(ctx, uid, gid, ns)
}

// stopPodnet stops podman's network namespace and its uplink.
func (e *Executor) stopPodnet(ctx context.Context) {
	e.stopUnits(ctx, PodnetUplinkUnit, PodnetUnit)
}

// podmanConf is the runner's containers.conf. Setup writes it, owned by
// root. The runner's podman (Reset, PodnsUnit, podman.service) reads it from
// XDG_CONFIG_HOME, a job's through CONTAINERS_CONF_OVERRIDE.
func (e *Executor) podmanConf() string {
	return filepath.Join(e.root, ".config", "containers", "containers.conf")
}

// podmanConfText makes containers use the cgroupfs manager. The systemd one
// needs the user bus, which no podman command sees (PodnsUnit hides it), and
// crun would ask the system bus instead, which refuses. With cgroupfs a
// job's containers stay in the job's cgroup and count against its limits.
const podmanConfText = `# Written by tentmaker setup.
[engine]
cgroup_manager = "cgroupfs"
`

// jobMounts are the mount properties a job's unit and PodnsUnit share: the
// jobs' resolv.conf, and the paths a job must not reach. home is the
// directory whose homes root stays visible.
func (e *Executor) jobMounts(uid int, home string) []string {
	rt := e.runtimeDir(uid)
	props := []string{
		"BindReadOnlyPaths=" + e.jobResolvConf() + ":/etc/resolv.conf",
		// The user bus and manager sockets sit at fixed paths this uid owns; a
		// job on them could point podman.service at a config it wrote.
		"InaccessiblePaths=-" + filepath.Join(rt, "bus"),
		"InaccessiblePaths=-" + filepath.Join(rt, "systemd"),
	}
	for _, p := range TailscalePaths {
		props = append(props, "InaccessiblePaths=-"+p)
	}
	// Other users' homes are closed by setup already; hiding them is a
	// second wall. Not ProtectHome=yes: that also hides /run/user, the
	// job's XDG_RUNTIME_DIR and podman socket.
	for _, p := range []string{e.p.Homes, "/root"} {
		if !strings.HasPrefix(home+"/", strings.TrimSuffix(p, "/")+"/") {
			props = append(props, "InaccessiblePaths=-"+p)
		}
	}
	return props
}

// podnsArgs is the systemd-run argv for PodnsUnit, in the network namespace
// at netns. It has no NoNewPrivileges: podman needs the setuid newuidmap to
// map the subordinate ids. It runs only podman, never job code.
func (e *Executor) podnsArgs(uid, gid int, netns string) []string {
	args := []string{"--quiet", "--no-ask-password",
		"--unit=" + PodnsUnit, "--slice=" + Slice,
		"--uid=" + strconv.Itoa(uid), "--gid=" + strconv.Itoa(gid),
		"-p", "Type=exec",
		"-p", "NetworkNamespacePath=" + netns}
	for _, p := range e.jobMounts(uid, e.root) {
		args = append(args, "-p", p)
	}
	for _, kv := range []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "HOME=" + e.root,
		"XDG_RUNTIME_DIR=" + e.runtimeDir(uid), "XDG_CONFIG_HOME=" + filepath.Join(e.root, ".config")} {
		args = append(args, "--setenv="+kv)
	}
	return append(args, "--", "podman", "unshare", "sleep", "infinity")
}

// stopPodns stops PodnsUnit and with it the pause process.
func (e *Executor) stopPodns(ctx context.Context) { e.stopUnits(ctx, PodnsUnit) }

// startPodns starts PodnsUnit in the network namespace at netns and waits
// until its pause process has left the host's user namespace. Until then, a
// podman command would make its own pause process outside the unit.
func (e *Executor) startPodns(ctx context.Context, uid, gid int, netns string) error {
	e.stopPodns(ctx)
	if _, err := e.h.Run(ctx, Cmd{Path: "systemd-run", Args: e.podnsArgs(uid, gid, netns), Timeout: time.Minute}); err != nil {
		return fmt.Errorf("start %s: %v", PodnsUnit, err)
	}
	host, err := os.Readlink(filepath.Join(e.p.Proc, "self", "ns", "user"))
	if err != nil {
		return fmt.Errorf("read this host's user namespace: %v", err)
	}
	pidFile := e.pausePIDFile(uid)
	for i := 0; i < 300; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
				ns, err := os.Readlink(filepath.Join(e.p.Proc, strconv.Itoa(pid), "ns", "user"))
				if err == nil && ns != host {
					return nil
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.o.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s made no podman pause process in 30s%s", PodnsUnit, e.unitLog(ctx, PodnsUnit))
}

func (e *Executor) pausePIDFile(uid int) string {
	return filepath.Join(e.runtimeDir(uid), "libpod", "tmp", "pause.pid")
}

// podnetProblem says why podman's processes are in the host's network
// namespace, or "" when they are not. Strict (Reset), a missing pause
// process or user manager is a problem too; Preflight leaves those to its
// podman socket check.
func (e *Executor) podnetProblem(ctx context.Context, uid int, strict bool) string {
	host, err := os.Readlink(filepath.Join(e.p.Proc, "self", "ns", "net"))
	if err != nil {
		return "read this host's network namespace: " + err.Error()
	}
	inHost := func(pid int) (bool, error) {
		ns, err := os.Readlink(filepath.Join(e.p.Proc, strconv.Itoa(pid), "ns", "net"))
		return ns == host, err
	}
	b, err := os.ReadFile(e.pausePIDFile(uid))
	pause, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	switch same, nsErr := inHost(pause); {
	case err != nil || pause <= 0 || nsErr != nil:
		if strict {
			return "podman has no pause process"
		}
	case same:
		return "podman's pause process is in the host's network namespace, so a host-network container would be too"
	}
	unit := "user@" + strconv.Itoa(uid) + ".service"
	mgr, err := e.mainPID(ctx, unit)
	if err != nil {
		return err.Error()
	}
	switch same, nsErr := inHost(mgr); {
	case mgr <= 0 || nsErr != nil:
		if strict {
			return "the runner's user manager is not running"
		}
	case same:
		return "the runner's user manager is in the host's network namespace, so podman.service and its host-network containers would be too"
	}
	return ""
}

// uidMapProblem says which uids podman's pause process maps that the
// network rules don't name, or "" when it maps only the runner's uid and
// subordinate uids. newuidmap may read ranges from elsewhere than
// /etc/subuid (an nsswitch subid source); then the fence would miss them.
func (e *Executor) uidMapProblem(uid int) string {
	p, err := e.policy(uid)
	if err != nil {
		return err.Error()
	}
	b, _ := os.ReadFile(e.pausePIDFile(uid))
	m, err := os.ReadFile(filepath.Join(e.p.Proc, strings.TrimSpace(string(b)), "uid_map"))
	if err != nil {
		return "read podman's uid map: " + err.Error()
	}
	covered := func(first, last int) bool {
		if first == uid && last == uid {
			return true
		}
		return slices.ContainsFunc(p.SubUIDs, func(r IDRange) bool { return first >= r.First && last <= r.Last })
	}
	for _, line := range strings.Split(string(m), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		out, err1 := strconv.Atoi(f[1])
		n, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || n <= 0 || !covered(out, out+n-1) {
			return "podman maps uids " + f[1] + " (" + f[2] + " of them), which the network rules for CI jobs don't name"
		}
	}
	return ""
}

// podnsProblem says what in podman's namespaces a job should not see, or ""
// when they match a job's: its network namespace, then, through a podman
// command, its mounts.
func (e *Executor) podnsProblem(ctx context.Context, uid int) string {
	if p := e.podnetProblem(ctx, uid, true); p != "" {
		return p
	}
	if p := e.uidMapProblem(uid); p != "" {
		return p
	}
	want, err := os.ReadFile(e.jobResolvConf())
	if err != nil {
		return "read the jobs' resolv.conf: " + err.Error()
	}
	const script = `test -w "$1/bus" && echo "the user bus is open."
test -w "$1/systemd/private" && echo "the user manager's socket is open."
[ "$(cat /etc/resolv.conf 2>/dev/null)" = "$2" ] || echo "resolv.conf is not the jobs'."
exit 0`
	res, err := e.h.Run(ctx, e.asRunner(uid, time.Minute, "podman", "unshare", "sh", "-c", script, "sh",
		e.runtimeDir(uid), strings.TrimRight(string(want), "\n")))
	if err != nil {
		return "podman unshare: " + err.Error()
	}
	if out := strings.Join(strings.Fields(res.Stdout), " "); out != "" {
		return "podman's namespace is not a job's: " + trunc(out, 200)
	}
	return ""
}
