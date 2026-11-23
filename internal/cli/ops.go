package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/daemon"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/forge/github"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/service"
	"github.com/baney75/tentmaker/internal/update"
)

// ---------------------------------------------------------------- devices

// badHostChars never appear in a tailnet host name.
const badHostChars = " \t/;&|$'\"`" + "\\"

type deviceView struct {
	OK      bool     `json:"ok"`
	Host    string   `json:"host"`
	Channel string   `json:"channel"`
	Pool    string   `json:"pool"`
	Repos   []string `json:"repos"`
}

func (a *app) installOnDevice(ctx context.Context, host string, args mesh.InstallArgs) (deviceView, error) {
	if args.Pool == "" {
		if cfg, _, err := a.loadConfig(); err == nil {
			args.Pool = cfg.Mesh.Pool
		}
	}
	if args.Channel == "" {
		args.Channel = daemon.Channel(a.env.Version)
		if cfg, exists, err := a.loadConfig(); err == nil && exists && cfg.Update.Channel != "" {
			args.Channel = cfg.Update.Channel
		}
	}
	if err := a.env.RemoteInstall(ctx, host, args); err != nil {
		return deviceView{}, fail("check that "+host+" is on your tailnet with Tailscale SSH on: tailscale status", "%v", err)
	}
	return deviceView{OK: true, Host: host, Channel: args.Channel, Pool: args.Pool, Repos: nonNil(args.Repos)}, nil
}

func (a *app) cmdDevices(args []string) (any, error) {
	u := commands["devices"].usage
	switch {
	case len(args) > 0 && args[0] == "remove":
		return a.removeDevice(args[1:])
	case len(args) == 0 || args[0] != "add":
		if wantsHelp(args) {
			fmt.Fprintln(a.env.Stdout, u)
			return nil, nil
		}
		f := a.flags("devices", u)
		pos, err := a.parse(f, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usage("usage: "+u, "devices takes add or remove, not %q", pos[0])
		}
		return a.listDevices()
	}
	f := a.flags("devices add", u)
	var ia mesh.InstallArgs
	var repos multi
	f.StringVar(&ia.Channel, "channel", "", "stable or dev")
	f.StringVar(&ia.Memory, "memory", "", "everyday memory there")
	f.StringVar(&ia.BoostMemory, "boost-memory", "", "boost memory there")
	f.Var(&repos, "repo", "OWNER/NAME (repeatable)")
	pos, err := a.parse(f, args[1:])
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, usage("usage: "+u, "devices add needs one HOST")
	}
	switch ia.Channel {
	case "", "stable", "dev":
	default:
		return nil, usage("use --channel stable or dev", "--channel %q is not stable or dev", ia.Channel)
	}
	for _, s := range []struct{ flag, v string }{{"--memory", ia.Memory}, {"--boost-memory", ia.BoostMemory}} {
		if s.v == "" {
			continue
		}
		if size, err := config.ParseSize(s.v); err != nil || size <= 0 {
			return nil, usage("write it like 8GiB", "%s %q is not a memory size", s.flag, s.v)
		}
	}
	for _, r := range repos {
		if err := validRepo(r); err != nil {
			return nil, err
		}
	}
	ia.Repos = repos
	host := pos[0]
	if strings.HasPrefix(host, "-") || strings.ContainsAny(host, badHostChars) {
		return nil, usage("use the device's tailnet name, like desktop", "%q is not a host name", host)
	}
	a.out("installing Tentmaker on %s over Tailscale SSH...\n", host)
	v, err := a.installOnDevice(a.ctx, host, ia)
	if err != nil {
		return nil, err
	}
	a.out("%s is set up; see it with: tentmaker pool\n", host)
	return v, nil
}

// ---------------------------------------------------------------- migrate

type migrateView struct {
	OK       bool          `json:"ok"`
	Path     string        `json:"path"`
	Platform string        `json:"platform"`
	Write    bool          `json:"write"`
	Files    []migrateFile `json:"files"`
	Changed  int           `json:"changed"`
}

// workflowFiles is path itself when it is a file, else the workflows in
// its .github/workflows and .forgejo/workflows.
func workflowFiles(p string) ([]string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{p}, nil
	}
	var out []string
	for _, dir := range []string{".github/workflows", ".forgejo/workflows"} {
		ents, err := os.ReadDir(filepath.Join(p, filepath.FromSlash(dir)))
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			if e.Type().IsRegular() && (strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml")) {
				out = append(out, filepath.Join(p, filepath.FromSlash(dir), n))
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no workflows in %s/.github/workflows or .forgejo/workflows", p)
	}
	return out, nil
}

func (a *app) migrate(p, platform string, write bool, o github.MigrateOptions) (migrateView, error) {
	if p == "" {
		wd, err := a.env.Getwd()
		if err != nil {
			return migrateView{}, fail("pass a PATH", "find the current folder: %v", err)
		}
		p = wd
	}
	if platform == "" {
		platform = daemon.HostPlatform(a.env.GOOS, a.env.GOARCH).OS
	}
	switch platform {
	case "linux", "macos", "windows":
	default:
		return migrateView{}, usage("use --platform linux, macos or windows", "--platform %q is not linux, macos or windows", platform)
	}
	files, err := workflowFiles(p)
	if err != nil {
		return migrateView{}, fail("run it in a repo, or pass the repo folder or a workflow file", "%v", err)
	}
	v := migrateView{OK: true, Path: p, Platform: platform, Write: write, Files: []migrateFile{}}
	for _, fp := range files {
		mf := migrateFile{File: fp, Report: []string{}}
		st, err := os.Stat(fp)
		var src []byte
		if err == nil {
			src, err = os.ReadFile(fp)
		}
		if err != nil {
			mf.Error = err.Error()
			v.OK = false
			v.Files = append(v.Files, mf)
			continue
		}
		res, err := github.Migrate(src, platform, o)
		ch := res.Changed
		mf.Changed, mf.Report, mf.Moves = ch, nonNil(res.Report), res.Moves
		if err != nil {
			mf.Error = err.Error()
			v.OK = false
		}
		if ch {
			v.Changed++
			if write && err == nil {
				if werr := config.WriteAtomic(fp, res.Out, st.Mode().Perm()); werr != nil {
					mf.Error = werr.Error()
					v.OK = false
				}
			}
		}
		v.Files = append(v.Files, mf)
	}
	return v, nil
}

func (v migrateView) exitCode() int {
	if v.OK {
		return ExitOK
	}
	return ExitFailed
}

func (a *app) cmdMigrate(args []string) (any, error) {
	f := a.flags("migrate", commands["migrate"].usage)
	platform := f.String("platform", "", "linux, macos or windows")
	write := f.Bool("write", false, "write the changes")
	pr := f.Bool("pr", false, "commit the changes on branch "+migrateBranch+", push it and open or update a pull request")
	all := f.Bool("all", false, "send fork pull requests to the pool too (no fork-safe expression)")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 1 {
		return nil, usage("usage: "+commands["migrate"].usage, "migrate takes one PATH")
	}
	p := ""
	if len(pos) == 1 {
		p = pos[0]
	}
	if *pr {
		pv, err := a.migratePR(a.ctx, p, *platform, github.MigrateOptions{All: *all})
		if err != nil {
			return nil, err
		}
		a.printMigratePR(pv)
		return pv, nil
	}
	v, err := a.migrate(p, *platform, *write, github.MigrateOptions{All: *all})
	if err != nil {
		return nil, err
	}
	a.printMigration(v.Files, *write)
	if !*write && v.Changed > 0 {
		a.out("\ndry run: nothing written. Write it with: tentmaker migrate %s --write\n", shellJoin([]string{v.Path}))
	}
	return v, nil
}

// ---------------------------------------------------------------- logs

type logsView struct {
	Source string   `json:"source"`
	Lines  []string `json:"lines"`
}

func (a *app) cmdLogs(args []string) (any, error) {
	f := a.flags("logs", commands["logs"].usage)
	follow := f.Bool("f", false, "follow")
	f.BoolVar(follow, "follow", false, "follow")
	n := f.Int("n", 100, "lines")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 1 {
		return nil, usage("usage: "+firstLine(commands["logs"].usage), "unexpected argument %q", pos[1])
	}
	if *n <= 0 || *n > 100000 {
		return nil, usage("use -n from 1 to 100000", "-n %d is out of range", *n)
	}
	if *follow && a.json {
		return nil, usage("drop -f, or drop --json", "-f streams lines, and --json prints one object")
	}
	if len(pos) == 1 {
		return a.cmdJobLog(pos[0], *n, f.set("n"), *follow)
	}
	if a.env.Paths.Log == "" {
		return a.journalLogs(*n, *follow)
	}
	return a.fileLogs(a.env.Paths.Log, *n, *follow)
}

func (a *app) journalLogs(n int, follow bool) (any, error) {
	args := []string{"--no-pager", "-u", service.UnitName, "-n", strconv.Itoa(n), "-o", "short-iso"}
	if follow {
		err := a.env.Command(a.ctx, a.env.Stdout, "journalctl", append(args, "-f")...)
		if err != nil && a.ctx.Err() == nil {
			return nil, fail("journalctl -u tentmaker", "journalctl: %v", err)
		}
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	var buf bytes.Buffer
	if err := a.env.Command(ctx, &buf, "journalctl", args...); err != nil {
		return nil, fail("run it with sudo, or join the systemd-journal group", "journalctl: %v", err)
	}
	if !a.json {
		a.env.Stdout.Write(buf.Bytes())
	}
	return logsView{Source: "journald", Lines: splitLines(buf.String())}, nil
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// tailFile reads the last n lines of path, looking at most at its last 8 MiB.
func tailFile(path string, n int) ([]string, int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, 0, err
	}
	const window = 8 << 20
	start := st.Size() - window
	if start < 0 {
		start = 0
	}
	if _, err := fh.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	b, err := io.ReadAll(io.LimitReader(fh, window))
	if err != nil {
		return nil, 0, err
	}
	lines := splitLines(string(b))
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line is cut
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, start + int64(len(b)), nil
}

func (a *app) fileLogs(path string, n int, follow bool) (any, error) {
	lines, off, err := tailFile(path, n)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fail("start the service: sudo tentmaker setup", "no log at %s yet", path)
		}
		return nil, fail("run it with sudo", "read %s: %v", path, err)
	}
	if a.json {
		return logsView{Source: path, Lines: lines}, nil
	}
	for _, l := range lines {
		fmt.Fprintln(a.env.Stdout, l)
	}
	for follow && a.ctx.Err() == nil {
		a.env.Sleep(500 * time.Millisecond)
		fh, err := os.Open(path)
		if err != nil {
			continue
		}
		st, err := fh.Stat()
		if err == nil && st.Size() < off {
			off = 0 // rotated
		}
		if err == nil && st.Size() > off {
			if _, err := fh.Seek(off, io.SeekStart); err == nil {
				m, _ := io.Copy(a.env.Stdout, io.LimitReader(fh, st.Size()-off))
				off += m
			}
		}
		fh.Close()
	}
	return logsView{Source: path, Lines: lines}, nil
}

// ---------------------------------------------------------------- update

// UpdateHealthWait bounds the wait for the restarted service to report the
// new version before update rolls back. Tests shorten it.
var UpdateHealthWait = 60 * time.Second

type updateView struct {
	OK         bool   `json:"ok"`
	Channel    string `json:"channel"`
	Current    string `json:"current"`
	Latest     string `json:"latest,omitempty"`
	Available  bool   `json:"available"`
	Applied    bool   `json:"applied"`
	RolledBack bool   `json:"rolled_back"`
	Note       string `json:"note"`
}

func (a *app) channel(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if cfg, exists, err := a.loadConfig(); err == nil && exists && cfg.Update.Channel != "" {
		return cfg.Update.Channel
	}
	return daemon.Channel(a.env.Version)
}

// UpdateDrainWait bounds how long update waits, with new jobs paused, for
// running jobs to finish before it restarts the service.
var UpdateDrainWait = 30 * time.Minute

func (a *app) update(ctx context.Context, channel string, checkOnly, force bool) (updateView, error) {
	channel = a.channel(channel)
	v := updateView{OK: true, Channel: channel, Current: a.env.Version}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	rel, newer, err := a.env.Updater.Check(cctx, channel, a.env.Version)
	cancel()
	if err != nil {
		return v, fail("check the network and try again", "check for updates: %v", err)
	}
	v.Latest, v.Available = rel.Version, newer
	if !newer {
		v.Note = "already the newest " + channel + " release"
		return v, nil
	}
	if checkOnly {
		v.Note = "update with: sudo tentmaker update"
		return v, nil
	}
	cfg, _, _ := a.loadConfig()
	rc := a.resolved(cfg)
	cur, err := a.rootBinary()
	if err != nil {
		return v, err
	}
	dir := filepath.Join(rc.CacheDir, "update")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return v, fail("rerun with sudo", "make %s: %v", dir, err)
	}
	newBin, err := a.env.Updater.Download(ctx, rel, dir)
	if err != nil {
		return v, fail("try again later", "download %s: %v", rel.Version, err)
	}
	// The restart ends running jobs (on Windows the daemon's job object
	// kills them), so new jobs pause and running ones finish first
	// (review r1 partB first-F5).
	// A status error is not "not installed": the service may be running
	// jobs, so nothing changes unless --force says ending them is fine.
	// With --force an unknown service is treated as installed (review r2
	// partB sol-F1).
	st, serr := a.env.Service.Status(ctx)
	if serr != nil && !force {
		return v, fail("try again, or pass --force to restart anyway, which ends running jobs", "can't tell whether the service is running: %v; nothing changed", serr)
	}
	installed := serr != nil || st.Installed
	stopped := serr == nil && !st.Running
	undrain := func() string { return "" }
	if installed {
		if undrain, err = a.drain(ctx, rc, rel.Version, force, stopped); err != nil {
			return v, err
		}
	}
	if err := a.env.Updater.Apply(ctx, newBin, cur, rel.Version); err != nil {
		undrain()
		return v, fail("nothing changed; try again later", "apply %s: %v", rel.Version, err)
	}
	v.Applied = true
	if !installed {
		v.Note = "updated the binary; the service is not installed"
		return v, nil
	}
	if err := a.env.Service.Restart(ctx); err == nil && a.waitVersion(rc, rel.Version, UpdateHealthWait) == nil {
		v.Note = "updated and restarted" + undrain()
		return v, nil
	}
	if err := a.env.Updater.Rollback(cur); err != nil {
		return v, fail("sudo tentmaker rollback, then sudo tentmaker resume", "%s did not come up, and rollback failed: %v", rel.Version, err)
	}
	a.env.Service.Restart(ctx)
	v.OK, v.RolledBack = false, true
	v.Note = rel.Version + " did not come up within " + UpdateHealthWait.String() + ", so the previous version is back"
	if a.waitVersion(rc, a.env.Version, UpdateHealthWait) == nil {
		v.Note += undrain()
	} else {
		v.Note += "; run sudo tentmaker resume once the service is back"
	}
	return v, nil
}

// drain pauses new jobs through the control API and waits, up to
// UpdateDrainWait, for running jobs to finish; with force it does not
// wait. It returns a function that resumes the pause it set (none when the
// device was already paused) and says so for the update's note. When the
// control API doesn't answer, drain goes on only if the service manager
// reported the service stopped, or with force; a running service that
// doesn't answer may still have jobs, so the update changes nothing.
func (a *app) drain(ctx context.Context, cfg config.Config, version string, force, stopped bool) (func() string, error) {
	c := a.controlClient(cfg)
	status := func() (daemon.Status, error) {
		var st daemon.Status
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return st, c.Get(cctx, "/v1/status", &st)
	}
	running := func(st daemon.Status) int {
		if st.Worker == nil {
			return 0
		}
		return len(st.Worker.Running)
	}
	st, err := status()
	if err != nil {
		if stopped || force {
			return func() string { return "" }, nil
		}
		return nil, fail("try again, or pass --force to restart anyway, which ends running jobs", "the service is running but its control API did not answer, so running jobs can't be checked: %v; nothing changed", err)
	}
	was := st.Paused || (st.Worker != nil && st.Worker.Paused)
	if !was {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.Post(cctx, "/v1/pause", control.PauseRequest{Reason: "updating to " + version})
		cancel()
		if err != nil {
			return nil, fail("tentmaker doctor", "pause new jobs before the update: %v", err)
		}
	}
	resume := func() string {
		if was {
			return "; the device stays paused as it was"
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := c.Post(cctx, "/v1/resume", nil); err != nil {
			return "; new jobs are still paused: run sudo tentmaker resume"
		}
		return ""
	}
	if force {
		return resume, nil
	}
	wctx, cancel := context.WithTimeout(ctx, UpdateDrainWait)
	defer cancel()
	for {
		st, err := status()
		n := running(st)
		if err == nil && n == 0 {
			return resume, nil
		}
		if wctx.Err() != nil {
			resume()
			if err != nil {
				return nil, fail("tentmaker doctor", "can't see whether jobs are running: %v; nothing changed", err)
			}
			return nil, fail("wait for them, or pass --force to end them", "%d job(s) still running after %s; nothing changed", n, UpdateDrainWait)
		}
		a.env.Sleep(2 * time.Second)
	}
}

func (v updateView) exitCode() int {
	if v.OK {
		return ExitOK
	}
	return ExitFailed
}

// waitVersion waits for the service's control API to report version.
func (a *app) waitVersion(cfg config.Config, version string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(a.ctx, wait)
	defer cancel()
	c := a.controlClient(cfg)
	for {
		var st daemon.Status
		cctx, c2 := context.WithTimeout(ctx, 2*time.Second)
		err := c.Get(cctx, "/v1/status", &st)
		c2()
		if err == nil && st.Version == version {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.env.Sleep(time.Second)
	}
}

func (a *app) cmdUpdate(args []string) (any, error) {
	f := a.flags("update", commands["update"].usage)
	ch := f.String("channel", "", "stable or dev")
	check := f.Bool("check", false, "only check")
	force := f.Bool("force", false, "restart without waiting for running jobs, which ends them")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+commands["update"].usage, "unexpected argument %q", pos[0])
	}
	switch *ch {
	case "", update.Stable, update.Dev:
	default:
		return nil, usage("use --channel stable or dev", "--channel %q is not stable or dev", *ch)
	}
	if !*check {
		if err := a.needRoot(); err != nil {
			return nil, err
		}
	}
	v, err := a.update(a.ctx, *ch, *check, *force)
	if err != nil {
		return nil, err
	}
	a.out("%s (current %s, latest %s on %s)\n", v.Note, v.Current, firstNonEmpty(v.Latest, "?"), v.Channel)
	return v, nil
}

type rollbackView struct {
	OK   bool   `json:"ok"`
	Note string `json:"note"`
}

func (a *app) cmdRollback(args []string) (any, error) {
	f := a.flags("rollback", commands["rollback"].usage)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker rollback", "unexpected argument %q", pos[0])
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	cur, err := a.rootBinary()
	if err != nil {
		return nil, err
	}
	if err := a.env.Updater.Rollback(cur); err != nil {
		return nil, fail("reinstall with the install script", "%v", err)
	}
	v := rollbackView{OK: true, Note: "the previous version is back"}
	if st, err := a.env.Service.Status(a.ctx); err == nil && st.Installed {
		if err := a.env.Service.Restart(a.ctx); err != nil {
			v.Note += "; restart the service: " + err.Error()
		} else {
			v.Note += " and the service restarted"
		}
	}
	a.out("%s\n", v.Note)
	return v, nil
}

// ---------------------------------------------------------------- uninstall

type uninstallView struct {
	OK      bool        `json:"ok"`
	Purged  bool        `json:"purged"`
	Steps   []setupStep `json:"steps"`
	Removed []string    `json:"removed"`
	Kept    []string    `json:"kept"`
}

func (v uninstallView) exitCode() int {
	if v.OK {
		return ExitOK
	}
	return ExitFailed
}

func (a *app) ustep(v *uninstallView, name string, err error, okDetail string) {
	s := setupStep{Step: name, Status: daemon.OK, Detail: okDetail}
	if err != nil {
		s.Status, s.Detail = daemon.Fail, err.Error()
		v.OK = false
	}
	v.Steps = append(v.Steps, s)
	a.out("%s %-9s %s\n", a.paint(s.Status, fmt.Sprintf("%-4s", s.Status)), name, s.Detail)
}

func (a *app) cmdUninstall(args []string) (any, error) {
	f := a.flags("uninstall", commands["uninstall"].usage)
	purge := f.Bool("purge", false, "also remove config, secrets, state and cache")
	yes := f.Bool("yes", false, "don't ask")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+commands["uninstall"].usage, "unexpected argument %q", pos[0])
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	if !*yes {
		if !a.env.StdinTTY || a.json {
			return nil, usage("sudo tentmaker "+shellJoin(a.args)+" --yes", "uninstall needs --yes when it can't ask")
		}
		what := "the service and sandboxes"
		if *purge {
			what += ", plus config, tokens, state and cache"
		}
		ans, err := a.ask("Remove "+what+" from this device? (y/N)", "")
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(ans, "y") && !strings.EqualFold(ans, "yes") {
			return nil, fail("", "nothing removed")
		}
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		cfg = config.Default(a.totalMemory(), a.env.CPUs())
	}
	rc := a.resolved(cfg)
	v := uninstallView{OK: true, Purged: *purge, Steps: []setupStep{}, Removed: []string{}, Kept: []string{}}

	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	a.ustep(&v, "service", a.env.Service.Uninstall(ctx), "stopped and removed")

	// Idle runners the broker registered are deleted from the forge; busy
	// ones end with their job.
	forges := daemon.BuildForges(rc, a.env.Secrets, a.env.NewForge, nil)
	if !forges.Empty() {
		b := dispatch.NewBroker(dispatch.BrokerOptions{Config: rc, Forges: forges.Forges, StatusForges: forges.StatusForges, StateDir: rc.StateDir})
		sctx, c2 := context.WithTimeout(ctx, 30*time.Second)
		b.Shutdown(sctx)
		c2()
		a.ustep(&v, "runners", nil, "idle runners deleted from the forge")
	}
	a.ustep(&v, "sandbox", a.env.UninstallExecutors(ctx, rc, *purge), "sandboxes removed")

	if *purge {
		for _, p := range []string{a.env.ConfigPath, a.env.Paths.Secrets, rc.StateDir, rc.CacheDir, logDir(a.env.Paths.Log)} {
			if p == "" {
				continue
			}
			if err := removeOwned(p); err != nil {
				a.ustep(&v, "purge", err, "")
				continue
			}
			v.Removed = append(v.Removed, p)
		}
		if dir := filepath.Dir(a.env.ConfigPath); dir != "" {
			os.Remove(dir) // only when empty
		}
		a.ustep(&v, "purge", nil, "removed "+strings.Join(v.Removed, ", "))
	} else {
		v.Kept = []string{a.env.ConfigPath, a.env.Paths.Secrets, rc.StateDir, rc.CacheDir}
		a.out("kept config, tokens, state and cache; remove them too with: sudo tentmaker uninstall --purge\n")
	}
	if exe, err := a.env.Executable(); err == nil {
		a.out("the binary stays at %s; delete it to finish\n", exe)
	}
	return v, nil
}

func logDir(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// removeOwned removes a Tentmaker path. It refuses anything that doesn't
// name Tentmaker, so a bad config can't point it at a system folder.
func removeOwned(p string) error {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) || !strings.Contains(strings.ToLower(clean), "tentmaker") {
		return fmt.Errorf("refusing to remove %s: not a Tentmaker path", p)
	}
	if err := os.RemoveAll(clean); err != nil {
		return fmt.Errorf("remove %s: %v", p, err)
	}
	return nil
}

// ---------------------------------------------------------------- daemon

func (a *app) cmdDaemon(args []string) (any, error) {
	f := a.flags("daemon", commands["daemon"].usage)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker daemon", "unexpected argument %q", pos[0])
	}
	log, closer, err := newDaemonLogger(a.env.Paths.Log, a.env.Stderr)
	if err != nil {
		return nil, fail("check that "+filepath.Dir(a.env.Paths.Log)+" is writable", "open the log: %v", err)
	}
	defer closer.Close()
	o := daemon.Options{
		ConfigPath:  a.env.ConfigPath,
		Paths:       a.env.Paths,
		Version:     a.env.Version,
		Log:         log,
		Power:       a.env.Power,
		Idle:        a.env.Idle,
		Probe:       a.env.Probe,
		Executors:   a.env.Executors,
		NewForge:    a.env.NewForge,
		Secrets:     a.env.Secrets,
		Tailscale:   a.env.Tailscale,
		Updater:     a.env.Updater,
		TotalMemory: a.env.TotalMemory,
		CPUs:        a.env.CPUs,
		Hostname:    a.env.Hostname,
		Executable:  a.env.Executable,
		Now:         a.env.Now,
		ReloadOnHUP: true,
		UpdateEvery: daemon.UpdateEveryFromEnv(a.env.Getenv),
	}
	if a.env.IsRoot() {
		o.BinaryProblem = a.env.BinaryProblem
	}
	run := func(ctx context.Context) error { return a.env.RunDaemon(ctx, o) }
	if a.env.GOOS == "windows" && service.IsWindowsService() {
		err = service.RunWindowsService(run)
	} else {
		err = run(a.ctx)
	}
	if errors.Is(err, daemon.ErrRestart) {
		log.Info("exiting so the service manager starts the new binary")
		return nil, &cliError{code: ExitFailed, msg: "restarting for an update"}
	}
	if err != nil {
		return nil, fail("tentmaker logs", "daemon: %v", err)
	}
	return nil, nil
}

// ---------------------------------------------------------------- check-run

// maxBundle caps the CheckBundle read from stdin.
const maxBundle = 16 << 20

type checkRunView struct {
	core.CheckResult
	code int
}

func (v checkRunView) exitCode() int { return v.code }

func (a *app) cmdCheckRun(args []string) (any, error) {
	f := a.flags("check-run", commands["check-run"].usage)
	out := f.String("out", "", "output folder")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 || *out == "" {
		return nil, usage("usage: tentmaker check-run --out DIR", "check-run needs --out DIR and the bundle on stdin")
	}
	data, err := io.ReadAll(io.LimitReader(a.in, maxBundle+1))
	if err != nil {
		return nil, fail("", "read the bundle: %v", err)
	}
	if len(data) > maxBundle {
		return nil, usage("", "the bundle on stdin is over %d bytes", maxBundle)
	}
	var b core.CheckBundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, usage("pipe a CheckBundle as JSON", "the bundle on stdin is not JSON")
	}
	var live io.Writer = a.env.Stderr
	if a.json {
		live = nil
	}
	res := a.env.RunBundle(a.ctx, b, *out, live)
	v := checkRunView{CheckResult: res}
	if res.State == "error" {
		v.code = ExitFailed
	}
	a.out("%s: %s\n", res.State, res.Description)
	return v, nil
}
