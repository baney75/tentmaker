// Package linux is the linux-podman executor: each job runs as the
// unprivileged user tentmaker-runner in a transient systemd unit inside
// tentmaker.slice, with hard memory and CPU limits, its own fresh runner copy,
// HOME and tool cache, and rootless podman for container steps.
//
// The daemon runs as root. Jobs never do: they get no sudo, no docker group,
// a root-owned home, and NoNewPrivileges. The single-use secret (a JIT config
// or a check bundle) goes to the unit on stdin through a root-only 0600 file
// systemd opens before it drops privileges; it never reaches argv, the
// environment or a log.
package linux

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/executor/guestbin"
)

// Name is the executor's name.
const Name = "linux-podman"

// User is the unprivileged account every job runs as.
const User = "tentmaker-runner"

// Slice is the systemd slice that holds every job (and the runner user's
// manager, so rootless containers count against the pool limit too).
const Slice = "tentmaker.slice"

// UnitPrefix starts every job's transient unit name.
const UnitPrefix = "tentmaker-job-"

// Paths are the system locations the executor uses. Tests point them at a
// temp dir; the zero value means the defaults.
type Paths struct {
	Lib               string // /usr/local/lib/tentmaker: job script, docker shim, guest binary
	Opt               string // /opt/tentmaker: the root-owned runner template
	Systemd           string // /etc/systemd/system
	Homes             string // /home, which must be closed to other users
	Subuid            string // /etc/subuid
	Subgid            string // /etc/subgid
	WSLInterop        string // /proc/sys/fs/binfmt_misc/WSLInterop
	OSRelease         string // /proc/sys/kernel/osrelease
	CgroupControllers string // /sys/fs/cgroup/cgroup.controllers
	SystemdRunning    string // /run/systemd/system
	RunUser           string // /run/user
	PodmanSocketUnit  string // /usr/lib/systemd/user/podman.socket
	DockerCLI         string // /usr/bin/docker, the real docker CLI behind the shim
	ResolvConf        string // /etc/resolv.conf: the DNS servers jobs may query
	IfInet6           string // /proc/net/if_inet6: finds the Tailscale interfaces
	IPv6Route         string // /proc/net/ipv6_route: the on-link IPv6 prefixes (the LAN)
	ResolvedConf      string // /run/systemd/resolve/resolv.conf: upstream DNS behind the 127.0.0.53 stub
	Proc              string // /proc
	// Nft, Iptables and Ip6tables are the firewall tools, Pasta and Unshare
	// make a job's network namespace; empty means look them up in
	// SystemPath.
	Nft, Iptables, Ip6tables, Pasta, Unshare string
}

func (p Paths) withDefaults() Paths {
	set := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	set(&p.Lib, "/usr/local/lib/tentmaker")
	set(&p.Opt, "/opt/tentmaker")
	set(&p.Systemd, "/etc/systemd/system")
	set(&p.Homes, "/home")
	set(&p.Subuid, "/etc/subuid")
	set(&p.Subgid, "/etc/subgid")
	set(&p.WSLInterop, "/proc/sys/fs/binfmt_misc/WSLInterop")
	set(&p.OSRelease, "/proc/sys/kernel/osrelease")
	set(&p.CgroupControllers, "/sys/fs/cgroup/cgroup.controllers")
	set(&p.SystemdRunning, "/run/systemd/system")
	set(&p.RunUser, "/run/user")
	set(&p.PodmanSocketUnit, "/usr/lib/systemd/user/podman.socket")
	set(&p.DockerCLI, "/usr/bin/docker")
	set(&p.ResolvConf, "/etc/resolv.conf")
	set(&p.IfInet6, "/proc/net/if_inet6")
	set(&p.IPv6Route, "/proc/net/ipv6_route")
	set(&p.ResolvedConf, "/run/systemd/resolve/resolv.conf")
	set(&p.Proc, "/proc")
	return p
}

// Options configure the executor.
type Options struct {
	StateDir string // daemon state; the executor uses <StateDir>/linux and <StateDir>/linux-private
	CacheDir string // downloads (<CacheDir>/dist)
	Log      *slog.Logger
	// Binary returns the tentmaker binary for a guest platform (KindCheck).
	// Install copies it to a root-owned 0755 path the runner user can run.
	Binary func(p core.Platform) (string, error)
	// Arch is the host's CPU in GitHub's spelling ("x64", "arm64"); empty
	// means runtime.GOARCH.
	Arch string
	// KeepImages keeps podman images across Reset. The default wipes them,
	// since the executor can't tell whether an untrusted job retagged one.
	KeepImages bool
	// AllowLAN lets jobs reach the private IPv4 ranges (RFC 1918), from
	// guards.allow_lan. The tailnet, link-local, this host and the mesh port
	// stay blocked.
	AllowLAN bool
	Host     Host
	Paths    Paths
	// Pin overrides the pinned actions/runner (tests).
	Pin   *Pin
	Sleep func(time.Duration)
}

// Executor is the linux-podman executor.
type Executor struct {
	o     Options
	p     Paths
	root  string // <StateDir>/linux: the runner user's root-owned home
	priv  string // <StateDir>/linux-private: root only
	arch  string
	log   *slog.Logger
	h     Host
	mu    sync.Mutex
	uid   int
	gid   int
	known bool
	guest guestbin.Syncer
}

// New returns the executor. It touches nothing until a method is called.
func New(o Options) *Executor {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Host == nil {
		o.Host = OSHost{}
	}
	if o.StateDir == "" {
		o.StateDir = "/var/lib/tentmaker"
	}
	if o.CacheDir == "" {
		o.CacheDir = "/var/cache/tentmaker"
	}
	if o.Arch == "" {
		o.Arch = goArch(runtime.GOARCH)
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	e := &Executor{o: o, p: o.Paths.withDefaults(), log: o.Log, h: o.Host, arch: o.Arch}
	e.root = filepath.Join(o.StateDir, "linux")
	e.priv = filepath.Join(o.StateDir, "linux-private")
	return e
}

func goArch(a string) string {
	switch a {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	}
	return a
}

func (e *Executor) pin() (Pin, bool) {
	if e.o.Pin != nil {
		return *e.o.Pin, true
	}
	return PinFor(e.arch)
}

// Name implements core.Executor.
func (e *Executor) Name() string { return Name }

// Platforms implements core.Executor.
func (e *Executor) Platforms() []core.Platform {
	if _, ok := PinFor(e.arch); !ok && e.o.Pin == nil {
		return nil
	}
	return []core.Platform{{OS: "linux", Arch: e.arch}}
}

// MaxSlots implements core.Executor: no cap beyond the budget.
func (e *Executor) MaxSlots() int { return 0 }

// Layout.
func (e *Executor) jobsDir() string            { return filepath.Join(e.root, "jobs") }
func (e *Executor) jobDir(name string) string  { return filepath.Join(e.jobsDir(), name) }
func (e *Executor) secretDir() string          { return filepath.Join(e.priv, "secrets") }
func (e *Executor) secretPath(n string) string { return filepath.Join(e.secretDir(), n) }
func (e *Executor) baselinePath() string       { return filepath.Join(e.priv, "units.baseline") }
func (e *Executor) template() string           { return filepath.Join(e.p.Opt, "runner") }
func (e *Executor) jobScript() string          { return filepath.Join(e.p.Lib, "job-runner") }
func (e *Executor) shimDir() string            { return filepath.Join(e.p.Lib, "bin") }

// toolsDir is where check jobs' setup actions find toolchains
// (<state>/tools/<tool>-<version>/): root-owned, readable by the runner.
func (e *Executor) toolsDir() string { return filepath.Join(e.o.StateDir, "tools") }
func (e *Executor) guestBinary() string {
	return filepath.Join(e.p.Lib, "guest", "tentmaker-linux-"+e.arch)
}
func unitName(name string) string { return UnitPrefix + name + ".service" }

func checkName(name string) error {
	if !core.NamePattern.MatchString(name) {
		return fmt.Errorf("bad runner name %q", trunc(name, 80))
	}
	return nil
}

// ids returns the runner user's uid and gid, looked up once.
func (e *Executor) ids() (int, int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.known {
		return e.uid, e.gid, nil
	}
	uid, gid, err := e.h.LookupUser(User)
	if err != nil {
		return 0, 0, fmt.Errorf("user %s: %w (run tentmaker setup)", User, err)
	}
	if uid == 0 || gid == 0 {
		return 0, 0, fmt.Errorf("user %s has uid or gid 0; refusing", User)
	}
	e.uid, e.gid, e.known = uid, gid, true
	return uid, gid, nil
}

func (e *Executor) runtimeDir(uid int) string {
	return filepath.Join(e.p.RunUser, strconv.Itoa(uid))
}

// asRunner runs a command as the runner user with its podman environment.
func (e *Executor) asRunner(uid int, timeout time.Duration, args ...string) Cmd {
	argv := []string{"-u", User, "--", "env", "-i",
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8",
		"HOME=" + e.root,
		"XDG_RUNTIME_DIR=" + e.runtimeDir(uid),
		"XDG_CONFIG_HOME=" + filepath.Join(e.root, ".config"),
	}
	return Cmd{Path: "runuser", Args: append(argv, args...), Timeout: timeout}
}

// Start implements core.Executor.
func (e *Executor) Start(ctx context.Context, spec core.ExecSpec) (err error) {
	if err := checkName(spec.Name); err != nil {
		return err
	}
	kind := spec.Kind
	if kind == "" {
		kind = core.KindRunner
	}
	if kind != core.KindRunner && kind != core.KindCheck {
		return fmt.Errorf("unknown job kind %q", trunc(kind, 20))
	}
	if spec.Platform.OS != "linux" || spec.Platform.Arch != e.arch {
		return fmt.Errorf("%s runs linux/%s, not %s", Name, e.arch, spec.Platform)
	}
	if len(spec.Grant.Secret) == 0 {
		return errors.New("the grant has no secret")
	}
	if spec.Limits.MemoryBytes <= 0 || spec.Limits.CPUs <= 0 {
		return errors.New("a job needs a memory and CPU limit")
	}
	if kind == core.KindCheck {
		if err := e.syncGuestBinary(); err != nil {
			return err
		}
	}
	uid, gid, err := e.ids()
	if err != nil {
		return err
	}
	// Fail closed: no job runs unless its traffic is confined.
	if err := e.ensureNet(ctx, uid); err != nil {
		return fmt.Errorf("%v; run sudo tentmaker setup", err)
	}
	dir := e.jobDir(spec.Name)
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("job %s already exists", spec.Name)
	}
	if err := e.prepareJob(dir, kind, spec, uid, gid); err != nil {
		os.RemoveAll(dir)
		return err
	}
	netns, err := e.startNetns(ctx, spec.Name, uid, gid)
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("%v; no job runs without a network namespace of its own", err)
	}
	// The secret waits in a root-only 0600 file. systemd opens it as the
	// unit's stdin before it drops to the runner user; it is gone as soon as
	// systemd-run returns (Type=exec: the process has started by then).
	if err := os.MkdirAll(e.secretDir(), 0o700); err != nil {
		return err
	}
	sp := e.secretPath(spec.Name)
	if err := config.WriteAtomic(sp, spec.Grant.Secret, 0o600); err != nil {
		e.stopNetns(context.WithoutCancel(ctx), spec.Name)
		os.RemoveAll(dir)
		return fmt.Errorf("write the job's stdin file: %v", err)
	}
	defer os.Remove(sp)
	if _, err := e.h.Run(ctx, Cmd{Path: "systemd-run", Args: e.unitArgs(spec, kind, uid, gid, sp, netns), Timeout: time.Minute}); err != nil {
		e.h.Run(context.WithoutCancel(ctx), Cmd{Path: "systemctl", Args: []string{"stop", unitName(spec.Name)}, Timeout: time.Minute})
		e.stopNetns(context.WithoutCancel(ctx), spec.Name)
		os.RemoveAll(dir)
		return fmt.Errorf("start unit %s: %s", unitName(spec.Name), redact(err.Error(), spec.Grant.Secret))
	}
	e.log.Info("job started", "executor", Name, "name", spec.Name, "kind", kind,
		"memory", spec.Limits.MemoryBytes, "cpus", spec.Limits.CPUs)
	return nil
}

// syncGuestBinary refreshes the check jobs' tentmaker binary when it
// differs from the daemon's (setup copied it; an update made it stale).
func (e *Executor) syncGuestBinary() error {
	if e.o.Binary == nil {
		return errors.New("no tentmaker binary for linux checks (Options.Binary is nil)")
	}
	src, err := e.o.Binary(core.Platform{OS: "linux", Arch: e.arch})
	if err != nil {
		return fmt.Errorf("tentmaker binary for linux/%s: %w", e.arch, err)
	}
	copied, err := e.guest.Sync(src, e.guestBinary(), func(src, dst string) error {
		bin, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return writeRoot(e.h, dst, bin, 0o755)
	})
	if copied {
		e.log.Info("refreshed the tentmaker binary for check jobs", "executor", Name, "path", e.guestBinary())
	}
	return err
}

func cpuQuota(cpus float64) string {
	return strconv.Itoa(int(math.Ceil(cpus*100))) + "%"
}

func maxDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 3 * time.Hour
	}
	return d
}

func (e *Executor) jobEnv(spec core.ExecSpec, uid int) []string {
	dir := e.jobDir(spec.Name)
	home := filepath.Join(dir, "home")
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"TMPDIR=" + filepath.Join(dir, "tmp"),
		"RUNNER_TOOL_CACHE=" + filepath.Join(dir, "toolcache"),
		"AGENT_TOOLSDIRECTORY=" + filepath.Join(dir, "toolcache"),
		"DOCKER_HOST=unix://" + filepath.Join(e.runtimeDir(uid), "podman", "podman.sock"),
		// The job's XDG_CONFIG_HOME is its own; this adds the runner's.
		"CONTAINERS_CONF_OVERRIDE=" + e.podmanConf(),
		"PATH=" + e.shimDir() + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
	}
	if spec.TestWorkers > 0 {
		env = append(env, "TENTMAKER_JOBS="+strconv.Itoa(spec.TestWorkers))
	}
	if spec.Kind == core.KindCheck {
		env = append(env, "TENTMAKER_TOOLS="+e.toolsDir())
	}
	return env
}

// unitArgs is the systemd-run argv for one job. It holds no secret.
func (e *Executor) unitArgs(spec core.ExecSpec, kind string, uid, gid int, stdinFile, netns string) []string {
	dir := e.jobDir(spec.Name)
	args := []string{
		// No --collect: a failed unit stays loaded until Clean, so
		// CheckResult can read why it ended (Result=oom-kill).
		"--quiet", "--no-ask-password",
		"--unit=" + unitName(spec.Name),
		"--slice=" + Slice,
		"--uid=" + strconv.Itoa(uid),
		"--gid=" + strconv.Itoa(gid),
		"-p", "Type=exec",
		"-p", "MemoryMax=" + strconv.FormatInt(spec.Limits.MemoryBytes, 10),
		"-p", "MemorySwapMax=0",
		// The kernel's OOM kill ends the whole job, not one process of it.
		"-p", "OOMPolicy=stop",
		"-p", "CPUQuota=" + cpuQuota(spec.Limits.CPUs),
		"-p", "TasksMax=4096",
		"-p", "Nice=10",
		"-p", "IOSchedulingClass=idle",
		"-p", "RuntimeMaxSec=" + strconv.FormatInt(int64(maxDuration(spec.MaxDuration)/time.Second), 10),
		"-p", "TimeoutStopSec=30",
		"-p", "NoNewPrivileges=yes",
		"-p", "PrivateTmp=yes",
		// Its own network namespace and loopback (netns.go), and DNS servers
		// it can reach from there.
		"-p", "NetworkNamespacePath=" + netns,
	}
	// The same mounts as podman's namespaces (podns.go).
	for _, p := range e.jobMounts(uid, dir) {
		args = append(args, "-p", p)
	}
	args = append(args,
		"-p", "StandardInput=file:"+stdinFile,
		"-p", "StandardOutput=journal",
		"-p", "StandardError=journal",
	)
	for _, kv := range e.jobEnv(spec, uid) {
		args = append(args, "--setenv="+kv)
	}
	switch kind {
	case core.KindCheck:
		args = append(args, "--working-directory="+filepath.Join(dir, "work"), "--",
			e.guestBinary(), "check-run", "--out", filepath.Join(dir, "out"))
	default:
		args = append(args, "--working-directory="+filepath.Join(dir, "runner"), "--",
			e.jobScript(), e.jobsDir(), spec.Name)
	}
	return args
}

// prepareJob builds the job's directory as root under the root-owned jobs
// dir, then hands it to the runner user bottom-up, so no job can swap a path
// under root's feet while it is being built.
func (e *Executor) prepareJob(dir, kind string, spec core.ExecSpec, uid, gid int) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create the job dir: %w", err)
	}
	for _, d := range []string{"home", "tmp", "toolcache", "work"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			return err
		}
	}
	if kind == core.KindCheck {
		if err := os.Mkdir(filepath.Join(dir, "out"), 0o700); err != nil {
			return err
		}
	} else {
		tpl, err := filepath.EvalSymlinks(e.template())
		if err != nil {
			return fmt.Errorf("runner template: %w (run tentmaker setup)", err)
		}
		if err := copyRunner(tpl, filepath.Join(dir, "runner")); err != nil {
			return fmt.Errorf("copy the runner: %w", err)
		}
		// The runner passes .env to every step: the job's own HOME, caches and podman.
		var env strings.Builder
		for _, kv := range e.jobEnv(spec, uid) {
			env.WriteString(kv + "\n")
		}
		if err := os.WriteFile(filepath.Join(dir, "runner", ".env"), []byte(env.String()), 0o600); err != nil {
			return err
		}
	}
	return chownTree(e.h, dir, uid, gid)
}

// copyRunner copies the template, linking its large read-only externals.
func copyRunner(tpl, dst string) error {
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(tpl)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		src := filepath.Join(tpl, ent.Name())
		if ent.Name() == "externals" {
			if err := os.Symlink(src, filepath.Join(dst, "externals")); err != nil {
				return err
			}
			continue
		}
		if err := copyTree(src, filepath.Join(dst, ent.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies files, dirs and symlinks (as links), keeping permissions.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		case d.IsDir():
			return os.Mkdir(to, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(p, to, info.Mode().Perm())
		}
		return nil // sockets, devices: never in a runner release
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// chownTree hands a tree to uid:gid children first, so the top directory
// (0700, root's until the end) keeps everyone else out while it walks.
func chownTree(h Host, top string, uid, gid int) error {
	var paths []string
	// WalkDir never descends a symlink (the externals link stays a link).
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(paths) - 1; i >= 0; i-- {
		if err := h.Lchown(paths[i], uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", paths[i], err)
		}
	}
	return nil
}

// State implements core.Executor.
func (e *Executor) State(ctx context.Context, name string) (core.ExecState, error) {
	if err := checkName(name); err != nil {
		return core.ExecUnknown, err
	}
	res, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"show", "--property=ActiveState", "--property=LoadState", unitName(name)}, Timeout: 30 * time.Second})
	if err != nil {
		return core.ExecUnknown, fmt.Errorf("read the state of %s: %v", unitName(name), err)
	}
	props := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	active, load := props["ActiveState"], props["LoadState"]
	switch {
	case active == "active" || active == "activating" || active == "deactivating" || active == "reloading" || active == "refreshing":
		return core.ExecRunning, nil
	case active == "inactive" || active == "failed" || load == "not-found":
		if _, err := os.Lstat(e.jobDir(name)); err == nil {
			return core.ExecExited, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return core.ExecUnknown, err
		}
		return core.ExecMissing, nil
	}
	return core.ExecUnknown, fmt.Errorf("unit %s has state %q", unitName(name), trunc(active, 40))
}

// Stop implements core.Executor.
func (e *Executor) Stop(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	os.Remove(e.secretPath(name))
	// A unit that isn't loaded makes stop fail; the state check below decides.
	e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"stop", unitName(name)}, Timeout: 90 * time.Second})
	st, err := e.State(ctx, name)
	if err != nil {
		return err
	}
	if st == core.ExecRunning {
		return fmt.Errorf("unit %s still runs after stop", unitName(name))
	}
	e.stopNetns(ctx, name)
	e.log.Info("job stopped", "executor", Name, "name", name)
	return nil
}

// Clean implements core.Executor.
func (e *Executor) Clean(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	st, err := e.State(ctx, name)
	if err != nil {
		return err
	}
	if st == core.ExecRunning || st == core.ExecUnknown {
		return fmt.Errorf("job %s still runs; stop it first", name)
	}
	os.Remove(e.secretPath(name))
	// A failed unit stays loaded (no --collect) until this.
	e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"reset-failed", unitName(name)}, Timeout: 30 * time.Second})
	e.stopNetns(ctx, name)
	return e.removeJobDir(ctx, e.jobDir(name))
}

// removeJobDir deletes a job tree. Container root writes files as a
// subordinate uid, so the runner user deletes inside its user namespace
// first; root finishes the rest (os.RemoveAll never follows a symlink).
func (e *Executor) removeJobDir(ctx context.Context, dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if uid, _, err := e.ids(); err == nil {
		e.h.Run(ctx, e.asRunner(uid, 5*time.Minute, "podman", "unshare", "rm", "-rf", "--", dir))
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete %s: %w", dir, err)
	}
	return nil
}

// List implements core.Executor: job dirs and loaded job units.
func (e *Executor) List(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	entries, err := os.ReadDir(e.jobsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, ent := range entries {
		if core.NamePattern.MatchString(ent.Name()) {
			seen[ent.Name()] = true
		}
	}
	units, err := e.jobUnits(ctx)
	if err != nil {
		return nil, err
	}
	for n := range units {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// jobUnits returns name -> ActiveState for every loaded job unit.
func (e *Executor) jobUnits(ctx context.Context) (map[string]string, error) {
	res, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"list-units", "--all", "--plain", "--no-legend", "--type=service", UnitPrefix + "*"}, Timeout: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("list job units: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		n := strings.TrimSuffix(strings.TrimPrefix(f[0], UnitPrefix), ".service")
		if strings.HasPrefix(f[0], UnitPrefix) && core.NamePattern.MatchString(n) {
			out[n] = f[2]
		}
	}
	return out, nil
}

// Reset implements core.Executor. With no job running: stop the runner
// user's manager and runtime dir (every CI process, container and planted
// unit ends), start the manager again, delete containers, pods, volumes,
// networks (and images unless KeepImages) and every job directory, then fail
// if a user unit is loaded that wasn't there at install.
func (e *Executor) Reset(ctx context.Context) error {
	uid, gid, err := e.ids()
	if err != nil {
		return err
	}
	units, err := e.jobUnits(ctx)
	if err != nil {
		return err
	}
	for n, st := range units {
		if st != "inactive" && st != "failed" {
			return fmt.Errorf("reset refused: job %s is %s", n, st)
		}
	}
	// No job runs, so no job namespace should either.
	nsUnits, err := e.netnsUnits(ctx)
	if err != nil {
		return err
	}
	for _, u := range nsUnits {
		e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"stop", u}, Timeout: 30 * time.Second})
		e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"reset-failed", u}, Timeout: 30 * time.Second})
	}
	// Restart the user manager, in podman's network namespace, and make
	// podman's namespaces before any podman command, or one would make them.
	if err := e.startPodman(ctx, uid, gid); err != nil {
		return fmt.Errorf("reset: %v", err)
	}
	wipe := [][]string{
		{"podman", "pod", "rm", "-af"},
		{"podman", "rm", "-af"},
		{"podman", "volume", "prune", "-f"},
		{"podman", "network", "prune", "-f"},
	}
	if !e.o.KeepImages {
		wipe = append(wipe, []string{"podman", "rmi", "-af"})
	}
	for _, args := range wipe {
		e.h.Run(ctx, e.asRunner(uid, 10*time.Minute, args...)) // checked below
	}
	var problems []string
	if p := e.podnsProblem(ctx, uid); p != "" {
		problems = append(problems, p)
	}
	if res, err := e.h.Run(ctx, e.asRunner(uid, time.Minute, "podman", "ps", "-aq")); err != nil || strings.TrimSpace(res.Stdout) != "" {
		problems = append(problems, "containers left after reset")
	}
	if !e.o.KeepImages {
		if res, err := e.h.Run(ctx, e.asRunner(uid, time.Minute, "podman", "images", "-q")); err != nil || strings.TrimSpace(res.Stdout) != "" {
			problems = append(problems, "images left after reset")
		}
	}
	entries, err := os.ReadDir(e.jobsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, ent := range entries {
		if err := e.removeJobDir(ctx, filepath.Join(e.jobsDir(), ent.Name())); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if ents, err := os.ReadDir(e.secretDir()); err == nil {
		for _, ent := range ents {
			os.Remove(filepath.Join(e.secretDir(), ent.Name()))
		}
	}
	extra, err := e.unexpectedUnits(ctx, uid)
	if err != nil {
		problems = append(problems, err.Error())
	} else if len(extra) > 0 {
		problems = append(problems, "unexpected user units after reset: "+strings.Join(extra, " "))
	}
	if len(problems) > 0 {
		return errors.New("reset: " + strings.Join(problems, "; "))
	}
	e.log.Info("reset", "executor", Name, "images_wiped", !e.o.KeepImages)
	return nil
}

func (e *Executor) userUnits(ctx context.Context, uid int) ([]string, error) {
	res, err := e.h.Run(ctx, e.asRunner(uid, time.Minute, "systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "--type=service,socket,timer,path"))
	if err != nil {
		return nil, fmt.Errorf("list user units: %v", err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			set[f[0]] = true
		}
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (e *Executor) unexpectedUnits(ctx context.Context, uid int) ([]string, error) {
	base, err := os.ReadFile(e.baselinePath())
	if err != nil {
		return nil, fmt.Errorf("units baseline: %v (run tentmaker setup)", err)
	}
	known := map[string]bool{}
	for _, l := range strings.Fields(string(base)) {
		known[l] = true
	}
	now, err := e.userUnits(ctx, uid)
	if err != nil {
		return nil, err
	}
	var extra []string
	for _, n := range now {
		if !known[n] {
			extra = append(extra, n)
		}
	}
	return extra, nil
}

// SetPoolLimit implements core.Executor: the slice's MemoryMax and CPUQuota,
// never below what running jobs use. A zero limit with nothing running
// changes nothing (no job may start then anyway).
func (e *Executor) SetPoolLimit(ctx context.Context, limit, inUse core.Resources) error {
	mem := max(limit.MemoryBytes, inUse.MemoryBytes)
	cpus := math.Max(limit.CPUs, inUse.CPUs)
	if mem <= 0 || cpus <= 0 {
		return nil
	}
	_, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"set-property", "--runtime", Slice,
		"MemoryMax=" + strconv.FormatInt(mem, 10), "CPUQuota=" + cpuQuota(cpus)}, Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("set the pool limit: %v", err)
	}
	return nil
}

// CheckResult implements core.Executor.
func (e *Executor) CheckResult(ctx context.Context, name string) (core.CheckResult, error) {
	if err := checkName(name); err != nil {
		return core.CheckResult{}, err
	}
	res := readCheckResult(e.jobsDir(), name+"/out", name)
	// The OOM stop either killed check-run before it wrote a result or made
	// it report "cancelled"; say what happened instead.
	out, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"show", "--property=Result", "--property=MemoryMax", unitName(name)}, Timeout: 30 * time.Second})
	if err == nil {
		props := map[string]string{}
		for _, line := range strings.Split(out.Stdout, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				props[k] = v
			}
		}
		if props["Result"] == "oom-kill" {
			res.State = "failure"
			res.Description = "ran out of memory"
			if n, err := strconv.ParseInt(props["MemoryMax"], 10, 64); err == nil && n > 0 {
				res.Description += fmt.Sprintf(" (the job's limit is %.1f GiB)", float64(n)/(1<<30))
			}
		}
	}
	return res, nil
}

// redact removes a secret from text bound for an error or a log.
func redact(s string, secret []byte) string {
	if len(secret) == 0 {
		return s
	}
	return strings.ReplaceAll(s, string(secret), "[redacted]")
}
