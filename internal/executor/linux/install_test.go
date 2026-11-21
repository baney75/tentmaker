//go:build !windows

package linux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostFixture makes the temp dir look like a systemd host with podman.
func (v *env) hostFixture(t *testing.T) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(v.dir, "run-systemd"), 0o755))
	must(t, os.WriteFile(filepath.Join(v.dir, "cgroup.controllers"), []byte("cpu memory io pids\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(v.dir, "subuid"), []byte("alice:100000:65536\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(v.dir, "subgid"), []byte("alice:100000:65536\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(v.dir, "home", "alice"), 0o700))
	must(t, os.Chmod(filepath.Join(v.dir, "home", "alice"), 0o700))
	must(t, os.WriteFile(filepath.Join(v.dir, "docker"), []byte("#!/bin/sh\n"), 0o755))
	tarball := []byte("pretend this is actions-runner.tar.gz")
	sum := sha256.Sum256(tarball)
	v.o.Pin = &Pin{Version: "9.9.9", Arch: "x64", SHA256: hex.EncodeToString(sum[:])}
	v.e = New(v.o)
	v.f.noUser = true
	v.f.fetch = func(url, dst string) error {
		must(t, os.MkdirAll(filepath.Dir(dst), 0o755))
		return os.WriteFile(dst, tarball, 0o644)
	}
	groups := "tentmaker-runner docker"
	v.f.respond = func(c Cmd) (Result, error) {
		a := strings.Join(c.Args, " ")
		switch {
		case c.Path == "useradd":
			v.f.mu.Lock()
			v.f.noUser = false
			v.f.mu.Unlock()
		case c.Path == "id":
			return Result{Stdout: groups + "\n"}, nil
		case c.Path == "gpasswd":
			groups = "tentmaker-runner"
		case c.Path == "usermod":
			file := v.o.Paths.Subuid
			if c.Args[0] == "--add-subgids" {
				file = v.o.Paths.Subgid
			}
			r := strings.SplitN(c.Args[1], "-", 2)
			f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
			f.WriteString(User + ":" + r[0] + ":65536\n")
			f.Close()
		case c.Path == "tar":
			dir := c.Args[len(c.Args)-2]
			must(t, os.MkdirAll(filepath.Join(dir, "externals"), 0o755))
			must(t, os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/bash\n"), 0o775))
		case strings.Contains(a, "systemctl --user list-units"):
			return Result{Stdout: "dbus.socket loaded active running D-Bus\ntentmaker-job-x.service loaded active running j\npodman.socket loaded active listening p\n"}, nil
		}
		return Result{}, nil
	}
}

func TestInstall(t *testing.T) {
	v := newEnv(t)
	v.hostFixture(t)
	ctx := context.Background()
	must(t, Install(ctx, v.o))
	cmds := strings.Join(v.f.commands(), "\n")
	root := filepath.Join(v.dir, "state", "linux")
	for _, s := range []string{
		"podman --version",
		"useradd --home-dir " + root + " --no-create-home --shell /usr/sbin/nologin --user-group tentmaker-runner",
		"gpasswd -d tentmaker-runner docker",
		"usermod --add-subuids 165536-231071 tentmaker-runner",
		"usermod --add-subgids 165536-231071 tentmaker-runner",
		"tar -xzf " + filepath.Join(v.dir, "cache", "dist", "actions-runner-linux-x64-9.9.9.tar.gz") + " -C " + filepath.Join(v.dir, "opt", "actions-runner-x64-9.9.9.part") + " --no-same-owner",
		"systemctl daemon-reload",
		"loginctl enable-linger tentmaker-runner",
		"systemctl restart user@1234.service",
		"systemctl --user restart podman.socket",
		"podman info",
	} {
		if !strings.Contains(cmds, s) {
			t.Errorf("Install did not run %q", s)
		}
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		must(t, err)
		return string(b)
	}
	lib := filepath.Join(v.dir, "lib")
	if read(filepath.Join(lib, "job-runner")) != jobRunnerScript {
		t.Error("job script")
	}
	if !strings.Contains(read(filepath.Join(lib, "bin", "docker")), "real='"+filepath.Join(v.dir, "docker")+"'") {
		t.Error("docker shim does not point at the real CLI")
	}
	if read(filepath.Join(lib, "guest", "tentmaker-linux-x64")) != "#!guest binary" {
		t.Error("guest binary")
	}
	for _, p := range []string{filepath.Join(lib, "job-runner"), filepath.Join(lib, "guest", "tentmaker-linux-x64")} {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 || v.f.chowns[p] != [2]int{0, 0} {
			t.Errorf("%s: mode %o owner %v", p, st.Mode().Perm(), v.f.chowns[p])
		}
	}
	if read(filepath.Join(v.dir, "systemd", "tentmaker.slice")) != sliceUnit {
		t.Error("slice unit")
	}
	if read(filepath.Join(v.dir, "systemd", "user@1234.service.d", "50-tentmaker.conf")) != userManagerDropIn {
		t.Error("manager drop-in")
	}
	if !strings.Contains(read(filepath.Join(root, ".config", "environment.d", "10-tentmaker.conf")), "XDG_CONFIG_HOME="+root+"/.config") {
		t.Error("environment.d")
	}
	if l, _ := os.Readlink(filepath.Join(root, ".config", "systemd", "user", "sockets.target.wants", "podman.socket")); l != "/usr/lib/systemd/user/podman.socket" {
		t.Errorf("podman.socket link %q", l)
	}
	base := read(filepath.Join(v.dir, "state", "linux-private", "units.baseline"))
	if base != "dbus.socket\npodman.service\npodman.socket\n" {
		t.Errorf("baseline %q", base)
	}
	if l, _ := os.Readlink(filepath.Join(v.dir, "opt", "runner")); l != filepath.Join(v.dir, "opt", "actions-runner-x64-9.9.9") {
		t.Errorf("template link %q", l)
	}
	// Ownership: the home, jobs dir and config are root's; podman storage is the runner's.
	want := map[string][2]int{
		root:                           {0, testGID},
		filepath.Join(root, "jobs"):    {0, testGID},
		filepath.Join(root, ".config"): {0, 0},
		filepath.Join(root, ".local", "share", "containers"):                   {testUID, testGID},
		filepath.Join(v.dir, "opt", "actions-runner-x64-9.9.9.part", "run.sh"): {0, 0},
	}
	for p, w := range want {
		if v.f.chowns[p] != w {
			t.Errorf("chown %s = %v, want %v", p, v.f.chowns[p], w)
		}
	}
	if st, _ := os.Stat(filepath.Join(v.dir, "opt", "actions-runner-x64-9.9.9", "run.sh")); st.Mode().Perm() != 0o755 {
		t.Errorf("template run.sh mode %o: group write must be gone", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(v.dir, "state", "linux-private")); st.Mode().Perm() != 0o700 {
		t.Error("private dir is not 0700")
	}

	// The host is now ready: Preflight finds nothing.
	must(t, os.MkdirAll(filepath.Join(v.dir, "run-user", "1234", "podman"), 0o700))
	must(t, os.WriteFile(filepath.Join(v.dir, "run-user", "1234", "podman", "podman.sock"), nil, 0o600))
	e := New(v.o)
	if ps := e.Preflight(ctx); len(ps) != 0 {
		t.Fatalf("Preflight after Install: %+v", ps)
	}

	// Rerun: no new user, ids, download or extraction.
	n := len(v.f.cmds)
	nf := len(v.f.fetches)
	must(t, Install(ctx, v.o))
	again := strings.Join(v.f.commands()[n:], "\n")
	for _, s := range []string{"useradd", "usermod", "gpasswd", "tar "} {
		if strings.Contains(again, s) {
			t.Errorf("rerun ran %q again", s)
		}
	}
	if len(v.f.fetches) != nf {
		t.Error("rerun downloaded again")
	}
}

func TestInstallRefuses(t *testing.T) {
	ctx := context.Background()

	v := newEnv(t)
	v.hostFixture(t)
	must(t, os.Chmod(filepath.Join(v.dir, "home", "alice"), 0o755))
	if err := Install(ctx, v.o); err == nil || !strings.Contains(err.Error(), "chmod o-rwx") {
		t.Fatalf("open home: %v", err)
	}

	v = newEnv(t)
	v.hostFixture(t)
	must(t, os.MkdirAll(filepath.Join(v.dir, "proc"), 0o755))
	must(t, os.WriteFile(filepath.Join(v.dir, "proc", "WSLInterop"), []byte("enabled\ninterpreter /init\n"), 0o644))
	if err := Install(ctx, v.o); err == nil || !strings.Contains(err.Error(), "wsl.conf") {
		t.Fatalf("WSL interop: %v", err)
	}
	if len(v.f.cmds) != 0 {
		t.Fatalf("ran commands before refusing: %v", v.f.commands())
	}

	v = newEnv(t)
	v.hostFixture(t)
	sum := v.o.Pin.SHA256
	v.o.Pin.SHA256 = strings.Repeat("0", 64)
	if err := Install(ctx, v.o); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(v.dir, "cache", "dist", "actions-runner-linux-x64-9.9.9.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a tarball with the wrong checksum was kept")
	}
	_ = sum
}

func TestPreflightFailsClosed(t *testing.T) {
	v := newEnv(t)
	v.f.noUser = true
	v.f.respond = func(c Cmd) (Result, error) { return Result{Code: -1}, errors.New("not here") }
	must(t, os.MkdirAll(filepath.Join(v.dir, "home", "bob"), 0o755))
	must(t, os.Chmod(filepath.Join(v.dir, "home", "bob"), 0o755))
	must(t, os.MkdirAll(filepath.Join(v.dir, "proc"), 0o755))
	must(t, os.WriteFile(filepath.Join(v.dir, "proc", "WSLInterop"), []byte("enabled\n"), 0o644))
	checks := map[string]bool{}
	for _, p := range v.e.Preflight(context.Background()) {
		checks[p.Check] = true
		if p.Fix == "" {
			t.Errorf("problem without a fix: %+v", p)
		}
	}
	for _, c := range []string{"systemd", "cgroup v2", "podman", "docker CLI", "WSL interop", "homes closed", "user", "installed"} {
		if !checks[c] {
			t.Errorf("Preflight missed %q (got %v)", c, checks)
		}
	}
}

func TestUninstall(t *testing.T) {
	v := newEnv(t)
	v.hostFixture(t)
	ctx := context.Background()
	must(t, Install(ctx, v.o))
	must(t, os.Mkdir(filepath.Join(v.dir, "state", "linux", "jobs", "job-left"), 0o700))

	must(t, Uninstall(ctx, v.o, false))
	for _, p := range []string{"lib", "systemd/tentmaker.slice", "systemd/user@1234.service.d", "state/linux/jobs/job-left"} {
		if _, err := os.Lstat(filepath.Join(v.dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived uninstall", p)
		}
	}
	if _, err := os.Stat(filepath.Join(v.dir, "opt", "runner")); err != nil {
		t.Error("the template went without purge")
	}
	if strings.Contains(strings.Join(v.f.commands(), "\n"), "userdel") {
		t.Error("user deleted without purge")
	}

	must(t, Uninstall(ctx, v.o, true))
	cmds := strings.Join(v.f.commands(), "\n")
	for _, s := range []string{"systemctl stop tentmaker-job-*.service", "loginctl disable-linger tentmaker-runner", "userdel tentmaker-runner"} {
		if !strings.Contains(cmds, s) {
			t.Errorf("purge did not run %q", s)
		}
	}
	for _, p := range []string{"opt", "state/linux", "state/linux-private", "cache/dist/actions-runner-linux-x64-9.9.9.tar.gz"} {
		if _, err := os.Lstat(filepath.Join(v.dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived purge", p)
		}
	}
	v.f.noUser = true
	must(t, Uninstall(ctx, v.o, true))
}
