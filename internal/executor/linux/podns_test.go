//go:build !windows

package linux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// resetEnv is an installed env whose user units match the baseline, so
// Reset passes unless podman's namespace is wrong.
func resetEnv(t *testing.T, probe string) *env {
	t.Helper()
	v := newEnv(t)
	v.installed(t)
	must(t, os.WriteFile(v.e.baselinePath(), []byte("podman.socket\n"), 0o600))
	v.f.respond = func(c Cmd) (Result, error) {
		a := strings.Join(c.Args, " ")
		switch {
		case strings.Contains(a, "systemctl --user list-units"):
			return Result{Stdout: "podman.socket loaded active listening y\n"}, nil
		case strings.Contains(a, "podman unshare sh -c"):
			return Result{Stdout: probe}, nil
		}
		return Result{}, nil
	}
	return v
}

func TestResetStartsPodmanNamespaceFirst(t *testing.T) {
	v := resetEnv(t, "")
	must(t, v.e.Reset(context.Background()))
	cmds := v.f.commands()
	at := func(s string) int {
		for i, c := range cmds {
			if strings.Contains(c, s) {
				return i
			}
		}
		t.Fatalf("Reset did not run %q", s)
		return -1
	}
	user, podns, podman := at("systemctl start user@1234.service"), at("--unit="+PodnsUnit), at("podman pod rm -af")
	if !(at("systemctl stop "+PodnsUnit) < user && user < podns && podns < podman) {
		t.Fatalf("order: stop podns, start user@ %d, start podns %d, first podman %d:\n%s", user, podns, podman, strings.Join(cmds, "\n"))
	}
	for _, c := range cmds[:podns] {
		if strings.Contains(c, "podman") && !strings.Contains(c, PodnsUnit) {
			t.Fatalf("podman ran before %s: %s", PodnsUnit, c)
		}
	}

	// The unit gets a job's mounts, and no NoNewPrivileges (newuidmap).
	args := v.e.podnsArgs(testUID, testGID, filepath.Join(v.o.Paths.Proc, strconv.Itoa(testPodnetPID), "ns", "net"), "")
	if !strings.Contains(cmds[podns], strings.Join(args, " ")) {
		t.Fatalf("podns unit:\n%s\nwant args %q", cmds[podns], args)
	}
	rt := v.e.runtimeDir(testUID)
	for _, p := range []string{
		"BindReadOnlyPaths=" + v.e.jobResolvConf() + ":/etc/resolv.conf",
		"InaccessiblePaths=-" + filepath.Join(rt, "bus"),
		"InaccessiblePaths=-" + filepath.Join(rt, "systemd"),
		"InaccessiblePaths=-/run/tailscale",
	} {
		if !slices.Contains(args, p) {
			t.Errorf("podns unit lacks %s", p)
		}
	}
	if strings.Contains(strings.Join(args, " "), "NoNewPrivileges") {
		t.Error("podns unit sets NoNewPrivileges; newuidmap could not map the subordinate ids")
	}
	if args[len(args)-4] != "podman" || args[len(args)-1] != "infinity" {
		t.Errorf("podns unit runs %q", args[len(args)-4:])
	}

	// A job's unit has every one of those mounts too.
	job := strings.Join(v.e.unitArgs(spec("job-x", "runner"), "runner", testUID, testGID, "/in", "/proc/1/ns/net"), " ")
	for _, p := range v.e.jobMounts(testUID, v.e.jobDir("job-x")) {
		if !strings.Contains(job, "-p "+p) {
			t.Errorf("job unit lacks %s", p)
		}
	}

	// The probe compares against the jobs' resolv.conf.
	want, err := os.ReadFile(v.e.jobResolvConf())
	must(t, err)
	if !strings.Contains(string(want), "nameserver 192.0.2.53") {
		t.Fatalf("jobs' resolv.conf: %q", want)
	}
	var probe *Cmd
	for i := range v.f.cmds {
		if slices.Contains(v.f.cmds[i].Args, "unshare") && slices.Contains(v.f.cmds[i].Args, "-c") {
			probe = &v.f.cmds[i]
		}
	}
	if probe == nil || probe.Args[len(probe.Args)-1] != strings.TrimRight(string(want), "\n") || probe.Args[len(probe.Args)-2] != rt {
		t.Fatalf("probe: %+v", probe)
	}
}

func TestResetRefusesPodmanNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, probe, want string
		pause             string // pause.pid's content; "" keeps the fake pause process
	}{
		{name: "bus-open", probe: "the user bus is open.\n", want: "the user bus is open"},
		{name: "host-resolv", probe: "resolv.conf is not the jobs'.\n", want: "resolv.conf is not the jobs'"},
		{name: "no-pause", pause: "none", want: "made no podman pause process"},
		{name: "pause-in-host-userns", pause: "self", want: "made no podman pause process"},
		// A host-network container through the podman service would share
		// the host's network namespace.
		{name: "pause-in-host-netns", pause: "host-netns", want: "pause process is in the host's network namespace"},
		{name: "manager-in-host-netns", pause: "manager-host-netns", want: "user manager is in the host's network namespace"},
		// newuidmap took a range from somewhere the fence did not read.
		{name: "unfenced-uid-map", pause: "uid-map", want: "podman maps uids 300000 (65536 of them)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := resetEnv(t, tc.probe)
			pidFile := filepath.Join(v.e.runtimeDir(testUID), "libpod", "tmp", "pause.pid")
			switch tc.pause {
			case "none":
				must(t, os.Remove(pidFile))
				v.f.pause = "none"
			case "self":
				// A pause process left in the host's user namespace.
				must(t, os.MkdirAll(filepath.Join(v.o.Paths.Proc, "4444", "ns"), 0o755))
				must(t, os.Symlink("user:[4026531837]", filepath.Join(v.o.Paths.Proc, "4444", "ns", "user")))
				must(t, os.WriteFile(pidFile, []byte(strconv.Itoa(4444)), 0o600))
				v.f.pause = "4444"
			case "host-netns":
				setNetns(t, v, testPausePID, "net:[4026531840]")
			case "manager-host-netns":
				setNetns(t, v, testManagerPID, "net:[4026531840]")
			case "uid-map":
				must(t, os.WriteFile(filepath.Join(v.o.Paths.Proc, strconv.Itoa(testPausePID), "uid_map"), []byte("0 1234 1\n1 300000 65536\n"), 0o644))
			}
			err := v.e.Reset(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Reset: %v, want %q", err, tc.want)
			}
		})
	}
}

// setNetns points the fake /proc/<pid>/ns/net at ns.
func setNetns(t *testing.T, v *env, pid int, ns string) {
	t.Helper()
	link := filepath.Join(v.o.Paths.Proc, strconv.Itoa(pid), "ns", "net")
	must(t, os.Remove(link))
	must(t, os.Symlink(ns, link))
}

// A host-network container started through the podman service (DOCKER_HOST)
// runs in podman.service's network namespace, which it gets from the
// runner's user manager; rootless podman joins only the pause process's user
// and mount namespaces. So Reset makes podman a network namespace of its
// own, the way Start makes a job's, and starts both the user manager and
// tentmaker-podns.service in it.
func TestResetGivesPodmanItsOwnNetworkNamespace(t *testing.T) {
	v := resetEnv(t, "")
	must(t, v.e.Reset(context.Background()))
	cmds := v.f.commands()
	last := func(s string, before int) int {
		at := -1
		for i, c := range cmds[:before] {
			if strings.Contains(c, s) {
				at = i
			}
		}
		return at
	}
	const holdUnit, upUnit = "tentmaker-podnet.service", "tentmaker-podnet-uplink.service"
	hold, up, podns := unitIndex(v.f, holdUnit), unitIndex(v.f, upUnit), unitIndex(v.f, PodnsUnit)
	user := last("systemctl start user@1234.service", len(cmds))
	reload := last("systemctl daemon-reload", user)
	stopUser := last("systemctl stop user@1234.service", hold+1)
	if !(stopUser >= 0 && stopUser < hold && hold < up && up < reload && reload < user && user < podns) {
		t.Fatalf("order: stop user@ %d, holder %d, uplink %d, daemon-reload %d, start user@ %d, podns %d:\n%s",
			stopUser, hold, up, reload, user, podns, strings.Join(cmds, "\n"))
	}

	// The holder and its pasta are a job's: the same unshare and pasta
	// argv, so the same per-uid rules match everything it sends out.
	holdArgs := strings.Join(findUnit(t, v.f, holdUnit).Args, " ")
	for _, s := range []string{"--uid=1234", "--slice=" + Slice, "AmbientCapabilities=CAP_SYS_ADMIN",
		v.o.Paths.Unshare + " --user --map-root-user --net -- sleep infinity"} {
		if !strings.Contains(holdArgs, s) {
			t.Errorf("podman's namespace holder lacks %q: %s", s, holdArgs)
		}
	}
	upArgs := strings.Join(findUnit(t, v.f, upUnit).Args, " ")
	if !strings.HasSuffix(upArgs, v.o.Paths.Pasta+" "+strings.Join(PastaArgs(testPodnetPID), " ")) || !strings.Contains(upArgs, "--uid=1234") {
		t.Errorf("podman's uplink: %s", upArgs)
	}

	// The user manager (podman.service) and the pause process enter it.
	ns := filepath.Join(v.o.Paths.Proc, strconv.Itoa(testPodnetPID), "ns", "net")
	drop, err := os.ReadFile(filepath.Join(v.o.Paths.SystemdRunning, "user@1234.service.d", "60-tentmaker-netns.conf"))
	if err != nil || !strings.Contains(string(drop), "[Service]\nNetworkNamespacePath="+ns+"\n") {
		t.Errorf("the user manager's runtime drop-in: %q, %v", drop, err)
	}
	if args := strings.Join(findUnit(t, v.f, PodnsUnit).Args, " "); !strings.Contains(args, "-p NetworkNamespacePath="+ns) {
		t.Errorf("%s does not enter podman's namespace: %s", PodnsUnit, args)
	}
}

// Doctor says so when the user manager or the pause process sits in the
// host's network namespace (after a reboot, before the first reset).
func TestPreflightReportsPodmanInHostNetns(t *testing.T) {
	for _, pid := range []int{testManagerPID, testPausePID} {
		v := newEnv(t)
		v.installed(t)
		setNetns(t, v, pid, "net:[4026531840]")
		found := false
		for _, p := range v.e.Preflight(context.Background()) {
			if p.Check == "podman network" && strings.Contains(p.What, "in the host's network namespace") {
				found = true
			}
		}
		if !found {
			t.Errorf("pid %d in the host's namespace: Preflight said %+v", pid, v.e.Preflight(context.Background()))
		}
	}
}
