package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/daemon"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/mesh"
)

// ---------------------------------------------------------------- version

type versionView struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Channel string `json:"channel"`
}

func (a *app) cmdVersion(args []string) (any, error) {
	f := a.flags("version", commands["version"].usage)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker version [--json]", "unexpected argument %q", pos[0])
	}
	v := versionView{Version: a.env.Version, OS: a.env.GOOS, Arch: a.env.GOARCH, Channel: daemon.Channel(a.env.Version)}
	// The installers read this exact line.
	a.out("tentmaker %s\n", v.Version)
	return v, nil
}

// ---------------------------------------------------------------- bare

func (a *app) cmdBare() (any, error) {
	if !a.configExists() {
		if a.json {
			return nil, a.notSetUp()
		}
		fix := "sudo tentmaker setup"
		if a.env.GOOS == "windows" {
			fix = "tentmaker setup (in PowerShell as Administrator)"
		}
		fmt.Fprintf(a.env.Stdout, "Tentmaker is not set up on this device yet.\nSet it up:  %s\nMore help:  tentmaker --help\n", fix)
		return silentExit(ExitNotSetUp), nil
	}
	return a.cmdStatus(nil)
}

// ---------------------------------------------------------------- status

// statusData asks the daemon, or computes what it can locally.
func (a *app) statusData(ctx context.Context) (daemon.Status, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return daemon.Status{}, err
	}
	var st daemon.Status
	err = a.controlClient(cfg).Get(ctx, "/v1/status", &st)
	if err == nil {
		return st, nil
	}
	return a.localStatus(ctx, cfg, err), nil
}

func (a *app) localStatus(ctx context.Context, cfg config.Config, why error) daemon.Status {
	rc := a.resolved(cfg)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := budget.New(rc, a.env.Power, a.env.Idle, a.env.Probe(rc.StateDir)).Decide(ctx, a.env.Now(), 0)
	st := daemon.Status{
		Version: a.env.Version, Device: rc.Device, Service: "not running", Decision: d, IdleS: -1,
		Warnings: []string{"service not running: this is what can be computed here"},
		Problems: []core.Problem{{Check: "service", What: "the service is not running", Fix: "sudo tentmaker setup"}},
	}
	if !errors.Is(why, control.ErrNotRunning) {
		st.Warnings = append(st.Warnings, "control API: "+why.Error())
	}
	if p, err := a.env.Power.Power(ctx); err == nil {
		st.Power = &p
	}
	idle, ierr := a.env.Idle.IdleFor(ctx)
	if ierr == nil {
		st.IdleS = int64(idle.Seconds())
	}
	st.Why = daemon.Why(d, st.Power, idle, ierr == nil)
	if rc.Mesh.Enabled {
		st.Pool = rc.Mesh.Pool
		st.Mesh.Enabled = true
		if self, err := a.env.Tailscale.Self(ctx); err == nil {
			st.Mesh.Up, st.Mesh.Self = true, self.DNSName
		} else {
			st.Mesh.Warning = "tailscale is not up; running local only"
		}
	}
	st.Next = daemon.NextStep(st)
	return st
}

func (a *app) cmdStatus(args []string) (any, error) {
	f := a.flags("status", commands["status"].usage)
	watch := f.Bool("watch", false, "refresh every 2 s")
	if _, err := a.parse(f, args); err != nil {
		return nil, err
	}
	if *watch && a.json {
		return nil, usage("drop --watch, or drop --json", "--watch prints a live view; --json prints one object")
	}
	st, err := a.statusData(a.ctx)
	if err != nil {
		return nil, err
	}
	if !*watch {
		a.printStatus(a.env.Stdout, st)
		return st, nil
	}
	for {
		if a.env.StdoutTTY {
			fmt.Fprint(a.env.Stdout, "\x1b[H\x1b[2J")
		}
		a.printStatus(a.env.Stdout, st)
		select {
		case <-a.ctx.Done():
			return nil, nil
		case <-time.After(2 * time.Second):
		}
		if st, err = a.statusData(a.ctx); err != nil {
			return nil, err
		}
	}
}

func gib(n int64) string { return guards.FormatBytes(n) }

func cpus(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.1f", f)
}

func (a *app) printStatus(w io.Writer, st daemon.Status) {
	if a.json {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	dev := st.Device
	if st.Pool != "" {
		dev += " (pool " + st.Pool + ")"
	}
	row("device", dev)
	svc := st.Service + ", version " + st.Version
	if st.Service != "running" {
		svc = a.paint(daemon.Fail, st.Service) + ", version " + st.Version
	}
	row("service", svc)
	prof := st.Why
	if st.Paused {
		prof = a.paint(daemon.Warn, "paused") + ": " + firstNonEmpty(st.PauseReason, "by hand")
		if st.PauseUntil != nil {
			prof += " until " + st.PauseUntil.Local().Format("15:04")
		}
		prof += " (" + st.Why + ")"
	}
	row("profile", prof)
	d := st.Decision
	row("memory", gib(st.InUse.MemoryBytes)+" of "+gib(d.Limit.MemoryBytes))
	row("cpus", cpus(st.InUse.CPUs)+" of "+cpus(d.Limit.CPUs))
	used := 0
	if st.Worker != nil {
		used = st.Worker.Slots.Used
	}
	slots := fmt.Sprintf("%d of %d used", used, d.Slots)
	if !d.Admit {
		slots += ", taking no new jobs"
	}
	row("slots", slots)
	if st.Worker != nil && len(st.Worker.Running) > 0 {
		for i, j := range st.Worker.Running {
			k := "jobs"
			if i > 0 {
				k = ""
			}
			age := ""
			if !j.Started.IsZero() {
				age = daemon.ShortDuration(time.Since(j.Started))
			}
			row(k, fmt.Sprintf("%s  %s  %s  %s", j.Repo, firstNonEmpty(j.Job, j.Name), firstNonEmpty(j.Kind, core.KindRunner), age))
		}
	} else {
		row("jobs", "none running")
	}
	if st.Worker != nil {
		for i, k := range st.Worker.Kept {
			key := "kept"
			if i > 0 {
				key = ""
			}
			row(key, fmt.Sprintf("%s  %s  until %s  (sudo tentmaker debug %s)", k.Repo, firstNonEmpty(k.Job, k.Name), k.Until.Local().Format("15:04"), k.Name))
		}
	}
	row("brokers", brokerLine(st))
	if st.Broker != nil {
		for i, r := range st.Broker.Repos {
			k := "repos"
			if i > 0 {
				k = ""
			}
			health := a.paint(daemon.OK, "ok")
			if !r.Healthy {
				health = a.paint(daemon.Fail, firstNonEmpty(r.Refused, r.LastError, "unhealthy"))
			}
			row(k, fmt.Sprintf("%s (%s, %s) %s, %d queued", r.Name, r.Forge, firstNonEmpty(r.Mode, "runner"), health, r.Queued+r.Pending))
		}
	}
	if st.Mesh.Enabled {
		m := "on the tailnet as " + st.Mesh.Self
		if !st.Mesh.Up {
			m = a.paint(daemon.Warn, "tailscale is not up; local only")
		}
		row("mesh", m)
	} else {
		row("mesh", "off (this device works alone)")
	}
	if st.LastError != "" {
		row("last error", st.LastError)
	}
	for _, p := range st.Problems {
		row("problem", a.paint(daemon.Fail, p.What))
	}
	tw.Flush()
	if st.Next != "" {
		fmt.Fprintf(w, "\nnext: %s\n", st.Next)
	}
}

func brokerLine(st daemon.Status) string {
	if st.Worker == nil || len(st.Worker.Sources) == 0 {
		if st.Broker != nil {
			return "this device"
		}
		return "none reachable"
	}
	var parts []string
	for _, s := range st.Worker.Sources {
		h := "ok"
		switch {
		case s.Quarantined:
			h = "quarantined"
		case !s.Healthy:
			h = "failing"
		}
		parts = append(parts, s.Name+" "+h)
	}
	return strings.Join(parts, ", ")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- pool

type poolView struct {
	Service string               `json:"service"`
	Devices []mesh.DeviceStatus  `json:"devices"`
	Summary map[string]poolEntry `json:"-"`
}

type poolEntry struct {
	Profile string
	Slots   string
	Jobs    int
}

func (a *app) poolData(ctx context.Context) (poolView, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return poolView{}, err
	}
	var devs []mesh.DeviceStatus
	err = a.controlClient(cfg).Get(ctx, "/v1/pool", &devs)
	if err == nil {
		return poolView{Service: "running", Devices: devs}, nil
	}
	st := a.localStatus(ctx, cfg, err)
	raw, _ := json.Marshal(st)
	return poolView{Service: "not running", Devices: []mesh.DeviceStatus{{Device: st.Device, Self: true, Version: a.env.Version, Status: raw,
		Error: "service not running; the pool is visible only through the service"}}}, nil
}

func (a *app) cmdPool(args []string) (any, error) {
	f := a.flags("pool", commands["pool"].usage)
	if _, err := a.parse(f, args); err != nil {
		return nil, err
	}
	pv, err := a.poolData(a.ctx)
	if err != nil {
		return nil, err
	}
	if !a.json {
		tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "DEVICE\tBROKER\tVERSION\tPROFILE\tSLOTS\tJOBS\tNOTE")
		for _, d := range pv.Devices {
			var st daemon.Status
			_ = json.Unmarshal(d.Status, &st)
			name := d.Device
			if d.Self {
				name += " (this)"
			}
			broker := "no"
			if d.Broker {
				broker = "yes"
			}
			jobs, used := 0, 0
			if st.Worker != nil {
				jobs, used = len(st.Worker.Running), st.Worker.Slots.Used
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%d\t%s\n", name, broker, d.Version, firstNonEmpty(st.Decision.Profile, "?"), used, st.Decision.Slots, jobs, d.Error)
		}
		tw.Flush()
	}
	return pv, nil
}

// ---------------------------------------------------------------- budget

type budgetView struct {
	SetUp    bool             `json:"set_up"`
	Service  string           `json:"service"`
	Source   string           `json:"source"` // "service" or "local"
	Budget   config.Budget    `json:"budget"`
	Job      config.Job       `json:"job"`
	Decision core.Decision    `json:"decision"`
	Why      string           `json:"why"`
	Power    *core.PowerState `json:"power,omitempty"`
}

func (a *app) budgetData(ctx context.Context) (budgetView, error) {
	cfg, exists, err := a.loadConfig()
	if err != nil {
		return budgetView{}, err
	}
	v := budgetView{SetUp: exists, Budget: cfg.Budget, Job: cfg.Job}
	if exists {
		var st daemon.Status
		if err := a.controlClient(cfg).Get(ctx, "/v1/status", &st); err == nil {
			v.Service, v.Source, v.Decision, v.Why, v.Power = "running", "service", st.Decision, st.Why, st.Power
			return v, nil
		}
	}
	st := a.localStatus(ctx, cfg, control.ErrNotRunning)
	v.Service, v.Source, v.Decision, v.Why, v.Power = "not running", "local", st.Decision, st.Why, st.Power
	return v, nil
}

func (a *app) printBudget(v budgetView) {
	if a.json {
		return
	}
	b := v.Budget
	tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
	row := func(k, val string) { fmt.Fprintf(tw, "%s\t%s\n", k, val) }
	row("everyday", fmt.Sprintf("%s, %s CPUs", b.Everyday.Memory, cpus(b.Everyday.CPUs)))
	boost := fmt.Sprintf("%s, %s CPUs when plugged in", b.Boost.Memory, cpus(b.Boost.CPUs))
	if b.BoostNeedsIdle {
		boost += " and idle " + b.IdleAfter.D().String()
	}
	row("boost", boost)
	row("on battery", fmt.Sprintf("%s; pause below %d%%, stop below %d%%", firstNonEmpty(b.OnBattery, "everyday"), b.PauseBelow, b.StopBelow))
	if len(b.Schedule) == 0 {
		row("schedule", "none")
	}
	for i, w := range b.Schedule {
		k := "schedule"
		if i > 0 {
			k = ""
		}
		row(k, fmt.Sprintf("%s: %s %s-%s %s", w.Name, daysString(w.Days), w.From, w.To, w.Profile))
	}
	row("job size", fmt.Sprintf("%s, %s CPUs", v.Job.Memory, cpus(v.Job.CPUs)))
	d := v.Decision
	admit := "taking jobs"
	if !d.Admit {
		admit = "taking no new jobs"
	}
	row("now", fmt.Sprintf("%s (%s)", v.Why, v.Source))
	row("limit", fmt.Sprintf("%s, %s CPUs, %d slots, %s", gib(d.Limit.MemoryBytes), cpus(d.Limit.CPUs), d.Slots, admit))
	tw.Flush()
	if !v.SetUp {
		fmt.Fprintln(a.env.Stdout, "\nnot set up yet; these are the defaults. Set up: sudo tentmaker setup")
	} else if v.Service != "running" {
		fmt.Fprintln(a.env.Stdout, "\nservice not running; computed here")
	}
}

func daysString(days []string) string {
	if len(days) == 0 {
		return "daily"
	}
	return strings.Join(days, ",")
}

// ---------------------------------------------------------------- doctor

type doctorView struct {
	daemon.Report
	Service string `json:"service"`
}

func (d doctorView) exitCode() int {
	if d.OK {
		return ExitOK
	}
	return ExitFailed
}

func (a *app) doctorData(ctx context.Context) doctorView {
	cfg, exists, cerr := a.loadConfig()
	if exists && cerr == nil {
		var rep daemon.Report
		if err := a.controlClient(cfg).Get(ctx, "/v1/doctor", &rep); err == nil {
			return doctorView{Report: rep, Service: "running"}
		}
	}
	rc := a.resolved(cfg)
	in := daemon.DoctorInput{
		Version: a.env.Version, Channel: a.channel(""), ConfigPath: a.env.ConfigPath,
		ConfigExists: exists, Config: rc,
		Service: a.env.Service.Status, Probe: a.env.Probe(rc.StateDir), Update: a.env.Updater.Check,
	}
	if cerr != nil {
		in.ConfigErr = cerr
	}
	binary := daemon.NewBinary(daemon.BinaryOptions{Version: a.env.Version, CacheDir: rc.CacheDir, Executable: a.env.Executable})
	if a.env.IsRoot() {
		in.Executors, in.ExecProblems = a.env.Executors(rc, binary, nil)
		in.Forges = daemon.BuildForges(rc, a.env.Secrets, a.env.NewForge, nil)
	} else {
		// The sandboxes keep their state where only root can read it, so
		// as a user these checks would call a working machine broken.
		in.SkipExecutors = "the sandbox checks read files only root can read, so they were not run"
		in.SkipForges = "forge tokens are readable only as root, so they were not checked"
	}
	if rc.Mesh.Enabled {
		in.Tailscale = a.env.Tailscale
	}
	rep := daemon.Doctor(ctx, in)
	return doctorView{Report: rep, Service: "not running"}
}

func (a *app) cmdDoctor(args []string) (any, error) {
	f := a.flags("doctor", commands["doctor"].usage)
	if _, err := a.parse(f, args); err != nil {
		return nil, err
	}
	v := a.doctorData(a.ctx)
	if !a.json {
		tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
		for _, c := range v.Checks {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", a.paint(c.Status, c.Status), c.Check, c.What)
			if c.Fix != "" && c.Status != daemon.OK {
				fmt.Fprintf(tw, "\t\tfix: %s\n", c.Fix)
			}
		}
		tw.Flush()
		switch {
		case v.OK && v.Warnings == 0:
			fmt.Fprintln(a.env.Stdout, "\nall good")
		case v.OK:
			fmt.Fprintf(a.env.Stdout, "\n%d warning(s)\n", v.Warnings)
		default:
			fmt.Fprintf(a.env.Stdout, "\n%d problem(s), %d warning(s)\n", v.Problems, v.Warnings)
		}
	}
	return v, nil
}
