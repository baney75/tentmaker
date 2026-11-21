package linux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// podnsArgs is the systemd-run argv for PodnsUnit. It has no
// NoNewPrivileges: podman needs the setuid newuidmap to map the subordinate
// ids. It runs only podman, never job code.
func (e *Executor) podnsArgs(uid, gid int) []string {
	args := []string{"--quiet", "--no-ask-password",
		"--unit=" + PodnsUnit, "--slice=" + Slice,
		"--uid=" + strconv.Itoa(uid), "--gid=" + strconv.Itoa(gid),
		"-p", "Type=exec"}
	for _, p := range e.jobMounts(uid, e.root) {
		args = append(args, "-p", p)
	}
	for _, kv := range []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "HOME=" + e.root,
		"XDG_RUNTIME_DIR=" + e.runtimeDir(uid), "XDG_CONFIG_HOME=" + filepath.Join(e.root, ".config")} {
		args = append(args, "--setenv="+kv)
	}
	return append(args, "--", "podman", "unshare", "sleep", "infinity")
}

// stopPodns stops PodnsUnit and with it the pause process. A unit that isn't
// loaded makes stop fail; that is fine.
func (e *Executor) stopPodns(ctx context.Context) {
	e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"stop", PodnsUnit}, Timeout: 30 * time.Second})
	e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"reset-failed", PodnsUnit}, Timeout: 30 * time.Second})
}

// startPodns starts PodnsUnit and waits until its pause process has left the
// host's user namespace. Until then, a podman command would make its own
// pause process outside the unit.
func (e *Executor) startPodns(ctx context.Context, uid, gid int) error {
	if err := e.writeJobResolvConf(); err != nil {
		return err
	}
	e.stopPodns(ctx)
	if _, err := e.h.Run(ctx, Cmd{Path: "systemd-run", Args: e.podnsArgs(uid, gid), Timeout: time.Minute}); err != nil {
		return fmt.Errorf("start %s: %v", PodnsUnit, err)
	}
	host, err := os.Readlink(filepath.Join(e.p.Proc, "self", "ns", "user"))
	if err != nil {
		return fmt.Errorf("read this host's user namespace: %v", err)
	}
	pidFile := filepath.Join(e.runtimeDir(uid), "libpod", "tmp", "pause.pid")
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

// podnsProblem runs a podman command and says what in podman's namespaces a
// job should not see, or "" when they match a job's.
func (e *Executor) podnsProblem(ctx context.Context, uid int) string {
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
