//go:build !windows

package linux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPastaArgs(t *testing.T) {
	got := strings.Join(PastaArgs(77), " ")
	want := "--config-net --quiet --no-map-gw -T " + strconv.Itoa(RelayPort) + " -U none -t none -u none 77"
	if got != want {
		t.Fatalf("pasta args\n got %s\nwant %s", got, want)
	}
	// Nothing else reaches the host's loopback or publishes a job port.
	for _, bad := range []string{"--map-host-loopback", "auto", "--runas", "all"} {
		if strings.Contains(got, bad) {
			t.Errorf("pasta args hold %q", bad)
		}
	}
}

// unitIndex is the position of the systemd-run that started unit, or -1.
func unitIndex(f *fakeHost, unit string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.cmds {
		if c.Path == "systemd-run" && slices.Contains(c.Args, "--unit="+unit) {
			return i
		}
	}
	return -1
}

func TestStartJobNetns(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	must(t, v.e.Start(context.Background(), spec("job-ns", "")))

	common := func(unit, typ string, props ...string) []string {
		args := []string{"--quiet", "--no-ask-password", "--unit=" + unit, "--slice=tentmaker.slice",
			"--uid=1234", "--gid=1235", "-p", "Type=" + typ, "-p", "NoNewPrivileges=yes",
			"-p", "TimeoutStartSec=30", "-p", "TimeoutStopSec=10"}
		for _, p := range props {
			args = append(args, "-p", p)
		}
		return append(args, "--")
	}
	hold := findUnit(t, v.f, netnsUnit("job-ns"))
	// Only CAP_SYS_ADMIN, which AppArmor's userns restriction asks of
	// unshare; pasta gets no capability.
	want := append(common("tentmaker-netns-job-ns.service", "exec", "AmbientCapabilities=CAP_SYS_ADMIN", "CapabilityBoundingSet=CAP_SYS_ADMIN"), v.o.Paths.Unshare, "--user", "--map-root-user", "--net", "--", "sleep", "infinity")
	if strings.Join(hold.Args, " ") != strings.Join(want, " ") {
		t.Errorf("holder\n got %q\nwant %q", hold.Args, want)
	}
	up := findUnit(t, v.f, uplinkUnit("job-ns"))
	want = append(append(common("tentmaker-uplink-job-ns.service", "forking"), v.o.Paths.Pasta), PastaArgs(testNetnsPID)...)
	if strings.Join(up.Args, " ") != strings.Join(want, " ") {
		t.Errorf("uplink\n got %q\nwant %q", up.Args, want)
	}
	// Holder, then pasta, then the job, which enters the holder's namespace.
	h, u, j := unitIndex(v.f, netnsUnit("job-ns")), unitIndex(v.f, uplinkUnit("job-ns")), unitIndex(v.f, unitName("job-ns"))
	if !(h >= 0 && h < u && u < j) {
		t.Errorf("start order holder %d uplink %d job %d", h, u, j)
	}
	job := strings.Join(findUnit(t, v.f, unitName("job-ns")).Args, " ")
	if !strings.Contains(job, "NetworkNamespacePath="+filepath.Join(v.o.Paths.Proc, "4242", "ns", "net")) {
		t.Errorf("job unit does not enter the namespace: %s", job)
	}
	// Its resolv.conf names the upstream server, not the host's stub or a
	// loopback one, and only root writes it.
	rc := v.e.jobResolvConf()
	if !strings.Contains(job, "BindReadOnlyPaths="+rc+":/etc/resolv.conf") {
		t.Errorf("job unit has no resolv.conf: %s", job)
	}
	b, err := os.ReadFile(rc)
	must(t, err)
	if !strings.Contains(string(b), "nameserver 192.0.2.53\n") || strings.Contains(string(b), "127.0.0") {
		t.Errorf("job resolv.conf:\n%s", b)
	}
	if st, _ := os.Stat(rc); st.Mode().Perm() != 0o644 {
		t.Errorf("job resolv.conf mode %o", st.Mode().Perm())
	}

	// Stop and Clean take the namespace down with the job.
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "show" && slices.Contains(c.Args, "--property=ActiveState") {
			return Result{Stdout: "ActiveState=inactive\nLoadState=loaded\n"}, nil
		}
		return Result{}, nil
	}
	n := len(v.f.cmds)
	must(t, v.e.Stop(context.Background(), "job-ns"))
	must(t, v.e.Clean(context.Background(), "job-ns"))
	cmds := strings.Join(v.f.commands()[n:], "\n")
	for _, s := range []string{"systemctl stop " + uplinkUnit("job-ns"), "systemctl stop " + netnsUnit("job-ns")} {
		if !strings.Contains(cmds, s) {
			t.Errorf("Stop and Clean did not run %q:\n%s", s, cmds)
		}
	}
}

func TestStartRefusesWithoutNetns(t *testing.T) {
	jobStarted := func(v *env, name string) bool { return unitIndex(v.f, unitName(name)) >= 0 }
	for _, c := range []struct {
		name, want string
		break_     func(v *env)
	}{
		{"no-pasta", "pasta is not installed", func(v *env) { must(t, os.Remove(v.o.Paths.Pasta)) }},
		{"no-unshare", "unshare is not installed", func(v *env) { must(t, os.Remove(v.o.Paths.Unshare)) }},
		// The holder never left the host's namespace: a job there would
		// share the host's loopback.
		{"same-ns", "did not make a network namespace", func(v *env) {
			v.f.netnsPID = 99
			must(t, os.MkdirAll(filepath.Join(v.o.Paths.Proc, "99", "ns"), 0o755))
			must(t, os.Symlink("net:[4026531840]", filepath.Join(v.o.Paths.Proc, "99", "ns", "net")))
		}},
		// The holder died at once: the error says why, from its journal.
		{"holder-dies", "Operation not permitted", func(v *env) {
			v.f.netnsPID = 99
			must(t, os.MkdirAll(filepath.Join(v.o.Paths.Proc, "99", "ns"), 0o755))
			must(t, os.Symlink("net:[4026531840]", filepath.Join(v.o.Paths.Proc, "99", "ns", "net")))
			v.f.respond = func(c Cmd) (Result, error) {
				if c.Path == "journalctl" && slices.Contains(c.Args, "--unit="+netnsUnit("job-no-holder-dies")) {
					return Result{Stdout: "unshare: write failed /proc/self/uid_map: Operation not permitted\n"}, nil
				}
				return Result{}, nil
			}
		}},
		{"no-dns", "no DNS server for CI jobs", func(v *env) {
			must(t, os.WriteFile(v.o.Paths.ResolvedConf, []byte("nameserver 127.0.0.1\n"), 0o644))
		}},
		{"uplink-fails", "start " + uplinkUnit("job-no-uplink-fails"), func(v *env) {
			v.f.respond = func(c Cmd) (Result, error) {
				if c.Path == "systemd-run" && slices.Contains(c.Args, "--unit="+uplinkUnit("job-no-uplink-fails")) {
					return Result{Code: 1}, errors.New("pasta failed")
				}
				return Result{}, nil
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t)
			v.installed(t)
			c.break_(v)
			name := "job-no-" + c.name
			err := v.e.Start(context.Background(), spec(name, ""))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Start: %v, want %q", err, c.want)
			}
			if jobStarted(v, name) {
				t.Fatal("a job started without a namespace of its own")
			}
			if _, err := os.Stat(v.e.jobDir(name)); !errors.Is(err, os.ErrNotExist) {
				t.Error("job dir left after a refused start")
			}
			if unitIndex(v.f, netnsUnit(name)) >= 0 {
				cmds := strings.Join(v.f.commands(), "\n")
				last := strings.LastIndex(cmds, "systemctl stop "+netnsUnit(name))
				if last < strings.LastIndex(cmds, "--unit="+netnsUnit(name)) {
					t.Errorf("the holder was left running:\n%s", cmds)
				}
			}
		})
	}
}

func TestPreflightAndInstallNeedPasta(t *testing.T) {
	v := newEnv(t)
	v.hostFixture(t)
	must(t, os.Remove(v.o.Paths.Pasta))
	err := Install(context.Background(), v.o)
	if err == nil || !strings.Contains(err.Error(), "pasta is not installed") {
		t.Fatalf("Install without pasta: %v", err)
	}
	found := false
	for _, p := range v.e.Preflight(context.Background()) {
		found = found || (p.Check == "network namespace" && strings.Contains(p.What, "pasta"))
	}
	if !found {
		t.Errorf("Preflight did not report missing pasta: %+v", v.e.Preflight(context.Background()))
	}
}

func TestResetStopsJobNetns(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "list-units" && slices.Contains(c.Args, NetnsPrefix+"*") {
			return Result{Stdout: "tentmaker-netns-job-old.service loaded active running x\ntentmaker-uplink-job-old.service loaded active running x\n"}, nil
		}
		return Result{}, nil
	}
	v.e.Reset(context.Background()) // the fake podman leaves nothing to check
	cmds := strings.Join(v.f.commands(), "\n")
	for _, u := range []string{"tentmaker-netns-job-old.service", "tentmaker-uplink-job-old.service"} {
		if !strings.Contains(cmds, "systemctl stop "+u) {
			t.Errorf("Reset left %s running", u)
		}
	}
}
