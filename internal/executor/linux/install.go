package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// subIDCount is the size of the runner user's subordinate uid/gid range.
const subIDCount = 65536

// Preflight implements core.Executor. Every check that can't run counts as a
// problem (fail closed).
func (e *Executor) Preflight(ctx context.Context) []core.Problem {
	var ps []core.Problem
	add := func(check, what, fix string) { ps = append(ps, core.Problem{Check: check, What: what, Fix: fix}) }
	setup := "run sudo tentmaker setup"

	if _, err := os.Stat(e.p.SystemdRunning); err != nil {
		add("systemd", "systemd is not running as init", "use a Linux host booted with systemd (on WSL, set [boot] systemd=true in /etc/wsl.conf)")
	}
	if _, err := os.Stat(e.p.CgroupControllers); err != nil {
		add("cgroup v2", "the unified cgroup v2 hierarchy is not mounted at /sys/fs/cgroup", "boot with systemd.unified_cgroup_hierarchy=1")
	}
	if _, ok := e.pin(); !ok {
		add("arch", "no pinned actions/runner for "+e.arch, "use an x64 or arm64 host")
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "podman", Args: []string{"--version"}, Timeout: 30 * time.Second}); err != nil {
		add("podman", "podman is not installed or does not run", "install podman (rootless) from your distribution")
	}
	if err := e.netnsTools(); err != nil {
		add("network namespace", err.Error(), "install passt (pasta) and util-linux from your distribution")
	}
	if len(e.jobResolvers()) == 0 {
		add("network namespace", "no DNS server CI jobs can reach: "+e.p.ResolvConf+" names only loopback and "+e.p.ResolvedConf+" is missing", "point resolv.conf at a real DNS server, or run systemd-resolved")
	}
	if st, err := os.Stat(e.p.DockerCLI); err != nil || st.Mode()&0o111 == 0 {
		add("docker CLI", e.p.DockerCLI+" is missing", "install the docker CLI only (docker-ce-cli or your distribution's docker client); the daemon is never used")
	}
	if p := e.wslProblem(); p != "" {
		add("WSL interop", p, "in /etc/wsl.conf set [interop] enabled=false and appendWindowsPath=false, then run wsl --shutdown from Windows")
	}
	for _, h := range e.openHomes() {
		add("homes closed", h+" is readable by other users", "chmod o-rwx "+h)
	}

	uid, _, err := e.ids()
	if err != nil {
		add("user", err.Error(), setup)
	} else {
		for _, f := range []string{e.p.Subuid, e.p.Subgid} {
			if n := subIDs(f, User); n < subIDCount {
				add("subordinate ids", fmt.Sprintf("%s gives %s %d ids; rootless podman needs %d", f, User, n, subIDCount), setup)
			}
		}
		if res, err := e.h.Run(ctx, Cmd{Path: "id", Args: []string{"-nG", User}, Timeout: 30 * time.Second}); err != nil {
			add("groups", "can't read the groups of "+User, setup)
		} else {
			for _, g := range strings.Fields(res.Stdout) {
				if privilegedGroup(g) {
					add("groups", User+" is in group "+g, "gpasswd -d "+User+" "+g)
				}
			}
		}
		if _, err := os.Stat(filepath.Join(e.runtimeDir(uid), "podman", "podman.sock")); err != nil {
			add("podman socket", "the rootless podman socket of "+User+" is not up", setup)
		}
		for _, p := range e.netProblems(ctx, uid) {
			add("network", p, setup)
		}
	}
	for _, f := range []string{filepath.Join(e.template(), "run.sh"), e.jobScript(), filepath.Join(e.shimDir(), "docker"), e.guestBinary(), e.baselinePath(), filepath.Join(e.p.Systemd, Slice)} {
		if _, err := os.Stat(f); err != nil {
			add("installed", f+" is missing", setup)
		}
	}
	for _, d := range []string{e.root, e.jobsDir(), filepath.Join(e.root, ".config")} {
		if st, err := os.Stat(d); err == nil && st.Mode().Perm()&0o022 != 0 {
			add("installed", d+" is group or world writable", setup)
		}
	}
	return ps
}

func privilegedGroup(g string) bool {
	switch g {
	case "wheel", "sudo", "admin", "docker", "root", "adm", "lxd", "libvirt", "disk", "systemd-journal":
		return true
	}
	return false
}

// wslProblem is non-empty when this is WSL with interop on.
func (e *Executor) wslProblem() string {
	b, err := os.ReadFile(e.p.WSLInterop)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "can't read " + e.p.WSLInterop
	}
	if strings.Contains(string(b), "disabled") {
		return ""
	}
	return "WSL interop is on: any Linux user, the CI user included, could run Windows programs as the Windows user"
}

func (e *Executor) onWSL() bool {
	if _, err := os.Stat(e.p.WSLInterop); err == nil {
		return true
	}
	b, _ := os.ReadFile(e.p.OSRelease)
	return strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// openHomes lists /home/* directories other users can read or enter.
func (e *Executor) openHomes() []string {
	entries, err := os.ReadDir(e.p.Homes)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		p := filepath.Join(e.p.Homes, ent.Name())
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			continue
		}
		if st.Mode().Perm()&0o007 != 0 {
			out = append(out, p)
		}
	}
	return out
}

// subIDs returns how many subordinate ids file gives name.
func subIDs(file, name string) int {
	f, err := os.Open(file)
	if err != nil {
		return 0
	}
	defer f.Close()
	total := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(p) == 3 && p[0] == name {
			if n, err := strconv.Atoi(p[2]); err == nil {
				total += n
			}
		}
	}
	return total
}

// nextSubID is the first id after every range in file (at least 100000).
func nextSubID(file string) (int, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return 100000, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	next := 100000
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(p) != 3 {
			continue
		}
		a, err1 := strconv.Atoi(p[1])
		n, err2 := strconv.Atoi(p[2])
		if err1 == nil && err2 == nil && a+n > next {
			next = a + n
		}
	}
	return next, sc.Err()
}

// Install sets up the host: the CI user, its subordinate ids, a root-owned
// home, the pinned runner template, the job script and docker shim, the guest
// binary for checks, rootless podman, tentmaker.slice and the units baseline.
// It runs as root and is safe to rerun. It refuses a host where WSL interop
// is on or a home directory is open to other users.
func Install(ctx context.Context, o Options) error {
	e := New(o)
	h := e.h
	run := func(c Cmd) error { _, err := h.Run(ctx, c); return err }

	if _, err := os.Stat(e.p.SystemdRunning); err != nil {
		return errors.New("systemd is not running as init")
	}
	if _, err := os.Stat(e.p.CgroupControllers); err != nil {
		return errors.New("cgroup v2 is not mounted at /sys/fs/cgroup")
	}
	pin, ok := e.pin()
	if !ok {
		return fmt.Errorf("no pinned actions/runner for %s", e.arch)
	}
	// WSL re-registers interop despite wsl.conf; switch it off, then refuse if that failed.
	if _, err := os.Stat(e.p.WSLInterop); err == nil {
		os.WriteFile(e.p.WSLInterop, []byte("0"), 0o644)
	}
	if p := e.wslProblem(); p != "" {
		return fmt.Errorf("refusing: %s. In /etc/wsl.conf set [interop] enabled=false and appendWindowsPath=false, then run wsl --shutdown from Windows", p)
	}
	if open := e.openHomes(); len(open) > 0 {
		return fmt.Errorf("refusing: home directories are open to other users: %s (chmod o-rwx each)", strings.Join(open, " "))
	}
	if err := run(Cmd{Path: "podman", Args: []string{"--version"}, Timeout: 30 * time.Second}); err != nil {
		return fmt.Errorf("podman is required: %v", err)
	}

	// The user: no sudo, no docker group, a root-owned home.
	if _, _, err := h.LookupUser(User); err != nil {
		if err := run(Cmd{Path: "useradd", Args: []string{"--home-dir", e.root, "--no-create-home", "--shell", "/usr/sbin/nologin", "--user-group", User}}); err != nil {
			return fmt.Errorf("create user %s: %v", User, err)
		}
	}
	uid, gid, err := e.ids()
	if err != nil {
		return err
	}
	if res, err := h.Run(ctx, Cmd{Path: "id", Args: []string{"-nG", User}, Timeout: 30 * time.Second}); err == nil {
		for _, g := range strings.Fields(res.Stdout) {
			if privilegedGroup(g) && g != User {
				if err := run(Cmd{Path: "gpasswd", Args: []string{"-d", User, g}}); err != nil {
					return fmt.Errorf("remove %s from %s: %v", User, g, err)
				}
			}
		}
	} else {
		return fmt.Errorf("read the groups of %s: %v", User, err)
	}
	for _, s := range []struct{ file, flag string }{{e.p.Subuid, "--add-subuids"}, {e.p.Subgid, "--add-subgids"}} {
		if subIDs(s.file, User) >= subIDCount {
			continue
		}
		next, err := nextSubID(s.file)
		if err != nil {
			return err
		}
		if err := run(Cmd{Path: "usermod", Args: []string{s.flag, fmt.Sprintf("%d-%d", next, next+subIDCount-1), User}}); err != nil {
			return fmt.Errorf("subordinate ids for %s: %v", User, err)
		}
	}

	// Directories. Only jobs' own dirs, podman's storage and systemd's state
	// are the runner's; everything on the paths systemd and podman read
	// config from is root's.
	type dir struct {
		path     string
		mode     os.FileMode
		uid, gid int
	}
	dirs := []dir{
		{e.o.StateDir, 0o755, -1, -1},
		{e.root, 0o750, 0, gid},
		{e.jobsDir(), 0o750, 0, gid},
		{filepath.Join(e.root, ".config"), 0o755, 0, 0},
		{filepath.Join(e.root, ".config", "containers"), 0o755, 0, 0},
		{filepath.Join(e.root, ".config", "environment.d"), 0o755, 0, 0},
		{filepath.Join(e.root, ".config", "systemd"), 0o755, 0, 0},
		{filepath.Join(e.root, ".config", "systemd", "user"), 0o755, 0, 0},
		{filepath.Join(e.root, ".config", "systemd", "user", "sockets.target.wants"), 0o755, 0, 0},
		{filepath.Join(e.root, ".local"), 0o755, 0, 0},
		{filepath.Join(e.root, ".local", "share"), 0o755, 0, 0},
		{filepath.Join(e.root, ".local", "share", "containers"), 0o700, uid, gid},
		{filepath.Join(e.root, ".local", "state"), 0o700, uid, gid},
		{e.priv, 0o700, 0, 0},
		{e.secretDir(), 0o700, 0, 0},
		{e.p.Lib, 0o755, 0, 0},
		{e.shimDir(), 0o755, 0, 0},
		{filepath.Join(e.p.Lib, "guest"), 0o755, 0, 0},
		{e.p.Opt, 0o755, 0, 0},
		{filepath.Join(e.o.CacheDir, "dist"), 0o755, 0, 0},
		{e.toolsDir(), 0o755, 0, 0},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if d.uid >= 0 {
			if err := h.Lchown(d.path, d.uid, d.gid); err != nil {
				return fmt.Errorf("chown %s: %w", d.path, err)
			}
			if err := os.Chmod(d.path, d.mode); err != nil {
				return err
			}
		}
	}
	// The runner user must be able to enter the state dir to reach its home.
	if st, err := os.Stat(e.o.StateDir); err == nil && st.Mode().Perm()&0o001 == 0 {
		if err := os.Chmod(e.o.StateDir, st.Mode().Perm()|0o001); err != nil {
			return err
		}
	}
	// Nothing but jobs/, .config, .local and podman's files lives in the home.
	if entries, err := os.ReadDir(e.root); err == nil {
		for _, ent := range entries {
			switch ent.Name() {
			case "jobs", ".config", ".local":
			default:
				os.RemoveAll(filepath.Join(e.root, ent.Name()))
			}
		}
	}

	if err := e.installRunner(ctx, pin); err != nil {
		return err
	}
	if err := writeRoot(h, e.jobScript(), []byte(jobRunnerScript), 0o755); err != nil {
		return err
	}
	if err := writeRoot(h, filepath.Join(e.shimDir(), "docker"), []byte(dockerShim(e.p.DockerCLI)), 0o755); err != nil {
		return err
	}
	if e.o.Binary == nil {
		return errors.New("no tentmaker binary for linux checks (Options.Binary is nil)")
	}
	src, err := e.o.Binary(core.Platform{OS: "linux", Arch: e.arch})
	if err != nil {
		return fmt.Errorf("tentmaker binary for linux/%s: %w", e.arch, err)
	}
	bin, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := writeRoot(h, e.guestBinary(), bin, 0o755); err != nil {
		return err
	}

	if e.onWSL() {
		unit := strings.ReplaceAll(rsharedUnit, "@UID@", strconv.Itoa(uid))
		if err := writeRoot(h, filepath.Join(e.p.Systemd, "tentmaker-rshared.service"), []byte(unit), 0o644); err != nil {
			return err
		}
	}
	if err := writeRoot(h, filepath.Join(e.p.Systemd, Slice), []byte(sliceUnit), 0o644); err != nil {
		return err
	}
	dropDir := filepath.Join(e.p.Systemd, "user@"+strconv.Itoa(uid)+".service.d")
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		return err
	}
	if err := writeRoot(h, filepath.Join(dropDir, "50-tentmaker.conf"), []byte(userManagerDropIn), 0o644); err != nil {
		return err
	}
	// Rootless podman refuses a ~/.config it doesn't own unless
	// XDG_CONFIG_HOME names it; this sets it for the runner's manager.
	envd := "# Written by tentmaker setup.\nXDG_CONFIG_HOME=" + filepath.Join(e.root, ".config") + "\n"
	if err := writeRoot(h, filepath.Join(e.root, ".config", "environment.d", "10-tentmaker.conf"), []byte(envd), 0o644); err != nil {
		return err
	}
	link := filepath.Join(e.root, ".config", "systemd", "user", "sockets.target.wants", "podman.socket")
	if err := replaceSymlink(e.p.PodmanSocketUnit, link); err != nil {
		return err
	}

	if err := run(Cmd{Path: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
		return err
	}
	if err := e.netnsTools(); err != nil {
		return err
	}
	if err := e.installNet(ctx, uid); err != nil {
		return err
	}
	if e.onWSL() {
		if err := run(Cmd{Path: "systemctl", Args: []string{"enable", "--now", "tentmaker-rshared.service"}}); err != nil {
			return err
		}
	}
	if err := run(Cmd{Path: "loginctl", Args: []string{"enable-linger", User}}); err != nil {
		return err
	}
	// Restart the manager so the slice drop-in and environment.d apply.
	u := strconv.Itoa(uid)
	if err := run(Cmd{Path: "systemctl", Args: []string{"restart", "user@" + u + ".service"}}); err != nil {
		return err
	}
	bus := filepath.Join(e.runtimeDir(uid), "bus")
	for i := 0; i < 40; i++ {
		if _, err := os.Stat(bus); err == nil {
			break
		}
		e.o.Sleep(250 * time.Millisecond)
	}
	for _, args := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "set-environment", "XDG_CONFIG_HOME=" + filepath.Join(e.root, ".config")},
		{"systemctl", "--user", "restart", "podman.socket"},
		{"podman", "info"},
	} {
		if err := run(e.asRunner(uid, 2*time.Minute, args...)); err != nil {
			return fmt.Errorf("rootless podman for %s: %v", User, err)
		}
	}
	units, err := e.userUnits(ctx, uid)
	if err != nil {
		return err
	}
	units = append(units, "podman.service", "podman.socket")
	sort.Strings(units)
	var base strings.Builder
	last := ""
	for _, n := range units {
		if n != last && !strings.HasPrefix(n, UnitPrefix) {
			base.WriteString(n + "\n")
		}
		last = n
	}
	if err := config.WriteAtomic(e.baselinePath(), []byte(base.String()), 0o600); err != nil {
		return err
	}
	e.log.Info("installed", "executor", Name, "runner", pin.Version, "arch", pin.Arch)
	return nil
}

// installRunner puts the pinned, checksummed runner under Opt, root-owned
// and read-only to everyone else, and points Opt/runner at it.
func (e *Executor) installRunner(ctx context.Context, pin Pin) error {
	release := filepath.Join(e.p.Opt, "actions-runner-"+pin.Arch+"-"+pin.Version)
	if _, err := os.Stat(filepath.Join(release, "run.sh")); err != nil {
		tarball := filepath.Join(e.o.CacheDir, "dist", "actions-runner-linux-"+pin.Arch+"-"+pin.Version+".tar.gz")
		if err := fetchVerified(ctx, e.h, pin.URL(), tarball, pin.SHA256); err != nil {
			return fmt.Errorf("actions/runner %s: %w", pin.Version, err)
		}
		part := release + ".part"
		os.RemoveAll(part)
		if err := os.MkdirAll(part, 0o755); err != nil {
			return err
		}
		if _, err := e.h.Run(ctx, Cmd{Path: "tar", Args: []string{"-xzf", tarball, "-C", part, "--no-same-owner"}, Timeout: 10 * time.Minute}); err != nil {
			return fmt.Errorf("extract actions/runner: %v", err)
		}
		if _, err := os.Stat(filepath.Join(part, "run.sh")); err != nil {
			return errors.New("the actions/runner archive has no run.sh")
		}
		if err := rootOwnReadOnly(e.h, part); err != nil {
			return err
		}
		if err := os.Rename(part, release); err != nil {
			return err
		}
	}
	return replaceSymlink(release, e.template())
}

// rootOwnReadOnly makes a tree root-owned and not group or world writable.
func rootOwnReadOnly(h Host, top string) error {
	return filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := h.Lchown(p, 0, 0); err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(p, info.Mode().Perm()&^0o022)
	})
}

// writeRoot writes a root-owned file atomically.
func writeRoot(h Host, path string, data []byte, mode os.FileMode) error {
	if err := config.WriteAtomic(path, data, mode); err != nil {
		return err
	}
	return h.Lchown(path, 0, 0)
}

// replaceSymlink points link at target atomically.
func replaceSymlink(target, link string) error {
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// Uninstall stops every job and removes what Install added. With purge it
// also deletes the CI user, its home and podman storage, the runner template
// and downloads. Safe to rerun.
func Uninstall(ctx context.Context, o Options, purge bool) error {
	e := New(o)
	h := e.h
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	// A pattern only matches loaded units, so this is a no-op when none run.
	h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"stop", UnitPrefix + "*.service"}, Timeout: 2 * time.Minute})
	if _, err := os.Stat(filepath.Join(e.p.Systemd, "tentmaker-rshared.service")); err == nil {
		h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"disable", "--now", "tentmaker-rshared.service"}})
		note(rmIfExists(filepath.Join(e.p.Systemd, "tentmaker-rshared.service")))
	}
	uid, _, uerr := h.LookupUser(User)
	if uerr == nil {
		dropDir := filepath.Join(e.p.Systemd, "user@"+strconv.Itoa(uid)+".service.d")
		note(rmIfExists(filepath.Join(dropDir, "50-tentmaker.conf")))
		os.Remove(dropDir) // only if empty
	}
	note(rmIfExists(filepath.Join(e.p.Systemd, Slice)))
	for _, err := range e.uninstallNet(ctx) {
		note(err)
	}
	note(os.RemoveAll(e.p.Lib))
	if entries, err := os.ReadDir(e.jobsDir()); err == nil {
		for _, ent := range entries {
			note(e.removeJobDir(ctx, filepath.Join(e.jobsDir(), ent.Name())))
		}
	}
	note(os.RemoveAll(e.secretDir()))
	if purge {
		if uerr == nil {
			h.Run(ctx, Cmd{Path: "loginctl", Args: []string{"disable-linger", User}})
			h.Run(ctx, Cmd{Path: "loginctl", Args: []string{"terminate-user", User}})
			e.o.Sleep(2 * time.Second)
			if _, err := h.Run(ctx, Cmd{Path: "userdel", Args: []string{User}}); err != nil {
				note(fmt.Errorf("delete user %s: %v", User, err))
			}
		}
		note(os.RemoveAll(e.root))
		note(os.RemoveAll(e.priv))
		note(os.RemoveAll(e.p.Opt))
		if entries, err := os.ReadDir(filepath.Join(e.o.CacheDir, "dist")); err == nil {
			for _, ent := range entries {
				if strings.HasPrefix(ent.Name(), "actions-runner-linux-") {
					note(os.Remove(filepath.Join(e.o.CacheDir, "dist", ent.Name())))
				}
			}
		}
	}
	if _, err := h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"daemon-reload"}}); err != nil {
		note(err)
	}
	if len(errs) > 0 {
		return errors.New("uninstall: " + strings.Join(errs, "; "))
	}
	return nil
}

func rmIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
