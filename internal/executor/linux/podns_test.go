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
	args := v.e.podnsArgs(testUID, testGID)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := resetEnv(t, tc.probe)
			pidFile := filepath.Join(v.e.runtimeDir(testUID), "libpod", "tmp", "pause.pid")
			switch tc.pause {
			case "none":
				must(t, os.Remove(pidFile))
			case "self":
				// A pause process left in the host's user namespace.
				must(t, os.MkdirAll(filepath.Join(v.o.Paths.Proc, "4444", "ns"), 0o755))
				must(t, os.Symlink("user:[4026531837]", filepath.Join(v.o.Paths.Proc, "4444", "ns", "user")))
				must(t, os.WriteFile(pidFile, []byte(strconv.Itoa(4444)), 0o600))
			}
			err := v.e.Reset(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Reset: %v, want %q", err, tc.want)
			}
		})
	}
}
