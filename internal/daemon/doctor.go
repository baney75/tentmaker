package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/service"
	"github.com/baney75/tentmaker/internal/update"
)

// Finding statuses.
const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
)

// Finding is one doctor check.
type Finding struct {
	Check  string `json:"check"`
	Status string `json:"status"` // ok, warn, fail
	What   string `json:"what"`
	Fix    string `json:"fix,omitempty"`
}

// Report is doctor's answer.
type Report struct {
	OK       bool      `json:"ok"` // no check failed
	Version  string    `json:"version"`
	Checks   []Finding `json:"checks"`
	Problems int       `json:"problems"` // failed checks
	Warnings int       `json:"warnings"`
}

// DoctorInput is everything doctor looks at. Nil fields skip their check.
type DoctorInput struct {
	Version, Channel string
	ConfigPath       string
	ConfigExists     bool
	ConfigErr        error
	Config           config.Config // resolved

	Executors    []core.Executor
	ExecProblems []core.Problem // from executor.ForHost
	Forges       Forges
	// SkipExecutors, when set, says why the sandbox checks didn't run.
	SkipExecutors string
	// SkipForges, when set, says why the forge checks didn't run.
	SkipForges  string
	Tailscale   mesh.Tailscale // nil when mesh is off
	Service     func(ctx context.Context) (service.State, error)
	ServiceNote string // set when the caller already knows (the daemon is the service)
	Probe       guards.Probe
	Update      func(ctx context.Context, channel, current string) (update.Release, bool, error)
	// Extra are problems the caller found building components.
	Extra []core.Problem
	// Binary is the binary the root (SYSTEM) service runs, and
	// BinaryProblem says why someone else could change it ("" when no one
	// can). Either empty skips the check.
	Binary        string
	BinaryProblem func(path string) (string, error)
	// Now is the clock for the first-job check (default time.Now).
	Now func() time.Time
}

// FirstJobGrace is how long a job for the pool may wait for a runner before
// the first-job check fails.
const FirstJobGrace = 3 * time.Minute

// CheckTimeout bounds each doctor check.
const CheckTimeout = 15 * time.Second

// Doctor runs every check, concurrently, each with its own timeout.
func Doctor(ctx context.Context, in DoctorInput) Report {
	type job struct {
		name string
		run  func(ctx context.Context) []Finding
	}
	jobs := []job{
		{"config", func(context.Context) []Finding { return checkConfig(in) }},
		{"service", func(ctx context.Context) []Finding { return checkService(ctx, in) }},
		{"executor", func(ctx context.Context) []Finding { return checkExecutors(ctx, in) }},
		{"forge", func(ctx context.Context) []Finding { return checkForges(ctx, in) }},
		{"first-job", func(ctx context.Context) []Finding { return checkFirstJob(ctx, in) }},
		{"tailscale", func(ctx context.Context) []Finding { return checkTailscale(ctx, in) }},
		{"disk", func(ctx context.Context) []Finding { return checkDisk(ctx, in) }},
		{"version", func(ctx context.Context) []Finding { return checkVersion(ctx, in) }},
		{"binary", func(context.Context) []Finding { return checkBinary(in) }},
	}
	results := make([][]Finding, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
			defer cancel()
			done := make(chan []Finding, 1)
			go func() { done <- j.run(cctx) }()
			select {
			case f := <-done:
				results[i] = f
			case <-cctx.Done():
				results[i] = []Finding{{Check: j.name, Status: Warn, What: "check timed out after " + CheckTimeout.String(), Fix: "run `tentmaker doctor` again"}}
			}
		}()
	}
	wg.Wait()
	r := Report{OK: true, Version: in.Version}
	for _, fs := range results {
		r.Checks = append(r.Checks, fs...)
	}
	for _, p := range in.Extra {
		r.Checks = append(r.Checks, Finding{Check: p.Check, Status: Fail, What: p.What, Fix: p.Fix})
	}
	for _, f := range r.Checks {
		switch f.Status {
		case Fail:
			r.OK = false
			r.Problems++
		case Warn:
			r.Warnings++
		}
	}
	return r
}

func checkConfig(in DoctorInput) []Finding {
	if !in.ConfigExists {
		return []Finding{{Check: "config", Status: Fail, What: "not set up yet: no config at " + in.ConfigPath, Fix: "sudo tentmaker setup"}}
	}
	if in.ConfigErr != nil {
		return []Finding{{Check: "config", Status: Fail, What: in.ConfigErr.Error(), Fix: "fix " + in.ConfigPath + " or rerun `sudo tentmaker setup`"}}
	}
	var out []Finding
	for _, p := range in.Config.Validate() {
		out = append(out, Finding{Check: "config", Status: Fail, What: p, Fix: "fix " + in.ConfigPath})
	}
	if len(out) == 0 {
		what := fmt.Sprintf("%s is valid", in.ConfigPath)
		if len(in.Config.Repos) == 0 {
			out = append(out, Finding{Check: "config", Status: Warn, What: "no repos; this device only takes work from brokers in its pool", Fix: "sudo tentmaker repo add OWNER/NAME"})
		}
		out = append([]Finding{{Check: "config", Status: OK, What: what}}, out...)
	}
	return out
}

func checkService(ctx context.Context, in DoctorInput) []Finding {
	if in.ServiceNote != "" {
		return []Finding{{Check: "service", Status: OK, What: in.ServiceNote}}
	}
	if in.Service == nil {
		return nil
	}
	st, err := in.Service(ctx)
	switch {
	case err != nil:
		return []Finding{{Check: "service", Status: Warn, What: "can't read the service: " + err.Error(), Fix: "run `tentmaker doctor` with sudo"}}
	case !st.Installed:
		return []Finding{{Check: "service", Status: Fail, What: "the service is not installed", Fix: "sudo tentmaker setup"}}
	case !st.Running:
		what := "the service is installed but not running"
		if st.Detail != "" {
			what += " (" + st.Detail + ")"
		}
		return []Finding{{Check: "service", Status: Fail, What: what, Fix: "sudo tentmaker setup (it restarts the service); then `tentmaker logs`"}}
	}
	return []Finding{{Check: "service", Status: OK, What: "the service is running"}}
}

// checkBinary fails when someone other than root could change the binary
// the root service runs: they would get root on its next restart or update.
// An older setup pointed the service at ~/.local/bin when install.sh ran
// without root.
func checkBinary(in DoctorInput) []Finding {
	if in.Binary == "" || in.BinaryProblem == nil {
		return nil
	}
	why, err := in.BinaryProblem(in.Binary)
	switch {
	case err != nil:
		return []Finding{{Check: "binary", Status: Warn, What: "can't check who can change " + in.Binary + ": " + err.Error()}}
	case why != "":
		return []Finding{{Check: "binary", Status: Fail,
			What: "the service runs " + in.Binary + " as root, but " + why + ", so that user could take over this machine",
			Fix:  "run sudo tentmaker setup, which copies the binary to a folder only root can change"}}
	}
	return []Finding{{Check: "binary", Status: OK, What: "only root can change " + in.Binary}}
}

func checkExecutors(ctx context.Context, in DoctorInput) []Finding {
	if in.SkipExecutors != "" {
		return []Finding{{Check: "executor", Status: Warn, What: in.SkipExecutors, Fix: "sudo tentmaker doctor"}}
	}
	var out []Finding
	for _, p := range in.ExecProblems {
		out = append(out, Finding{Check: p.Check, Status: Fail, What: p.What, Fix: p.Fix})
	}
	for _, e := range in.Executors {
		ps := e.Preflight(ctx)
		if len(ps) == 0 {
			plats := make([]string, 0, len(e.Platforms()))
			for _, p := range e.Platforms() {
				plats = append(plats, p.String())
			}
			out = append(out, Finding{Check: "executor", Status: OK, What: e.Name() + " is ready for " + strings.Join(plats, ", ")})
			continue
		}
		for _, p := range ps {
			out = append(out, Finding{Check: p.Check, Status: Fail, What: p.What, Fix: p.Fix})
		}
	}
	// Notes are worth knowing but block nothing (macOS guests off for lack
	// of disk).
	for _, e := range in.Executors {
		if n, ok := e.(interface {
			Notes(ctx context.Context) []core.Problem
		}); ok {
			for _, p := range n.Notes(ctx) {
				out = append(out, Finding{Check: p.Check, Status: Warn, What: p.What, Fix: p.Fix})
			}
		}
	}
	return out
}

func checkForges(ctx context.Context, in DoctorInput) []Finding {
	if in.SkipForges != "" {
		if len(in.Config.Repos) == 0 {
			return nil
		}
		return []Finding{{Check: "forge", Status: Warn, What: in.SkipForges, Fix: "sudo tentmaker doctor"}}
	}
	var out []Finding
	for _, p := range in.Forges.Problems {
		out = append(out, Finding{Check: p.Check, Status: Fail, What: p.What, Fix: p.Fix})
	}
	cfg := in.Config
	if len(cfg.Repos) > 0 && len(in.Forges.Tokens) == 0 && len(in.Forges.Problems) == 0 {
		out = append(out, Finding{Check: "forge", Status: Warn,
			What: "no forge token on this device, so it is a worker only; a broker elsewhere in the pool must hold the token",
			Fix:  "sudo tentmaker connect github"})
	}
	for _, r := range cfg.Repos {
		name := RepoForge(r)
		f := in.Forges.Forges[name]
		if f == nil {
			continue
		}
		info, err := f.CheckRepo(ctx, r.Name)
		if err != nil {
			out = append(out, Finding{Check: "forge", Status: Fail, What: fmt.Sprintf("%s: can't read the repo: %v", r.Name, err),
				Fix: "check the repo name and that the token can read it: sudo tentmaker connect " + ForgeConfig(cfg, name).Kind})
			continue
		}
		bad := false
		for _, p := range info.Problems {
			bad = true
			out = append(out, Finding{Check: "forge", Status: Fail, What: r.Name + ": " + p, Fix: "give the token that permission, then sudo tentmaker connect " + ForgeConfig(cfg, name).Kind})
		}
		if info.Public && !r.AllowPublic {
			bad = true
			out = append(out, Finding{Check: "forge", Status: Fail, What: r.Name + " is public and allow_public is off, so its jobs are refused",
				Fix: "sudo tentmaker repo add " + r.Name + " --allow-public --enable"})
		}
		// The broker's own rule, for the repos it applies it to (runner mode).
		if r.AllowPublic && (r.Mode == "" || r.Mode == "runner") {
			if why := dispatch.PublicRepoRefusal(info, true); why != "" {
				bad = true
				fix := "sudo tentmaker repo add " + r.Name + " --allow-public --enable"
				if info.ForkPolicy == "approve_first_time" && ForgeConfig(cfg, name).Kind != "github" {
					fix = "this forge has no setting to require approval for every outside contributor; serve it in checks mode: sudo tentmaker repo add " + r.Name + " --forge " + name + " --mode checks --allow-public"
				}
				out = append(out, Finding{Check: "forge", Status: Fail, What: r.Name + ": " + why + ", so the broker refuses its jobs", Fix: fix})
			}
		}
		if !bad {
			out = append(out, Finding{Check: "forge", Status: OK, What: r.Name + ": token and repo settings are fine"})
		}
	}
	return out
}

// checkFirstJob asks GitHub, for each runner-mode repo this device holds a
// token for, whether a Tentmaker runner has come online for it: one online
// now, or a recent job that ran on one. Before any job targets the pool it
// warns with the next step; a job for the pool that waits longer than
// FirstJobGrace with no runner is a failure.
func checkFirstJob(ctx context.Context, in DoctorInput) []Finding {
	if in.SkipForges != "" {
		return nil
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	var out []Finding
	for _, r := range in.Config.Repos {
		if RepoForge(r) != "github" || (r.Mode != "" && r.Mode != "runner") {
			continue
		}
		fr, ok := in.Forges.Forges["github"].(core.FirstJobReporter)
		if !ok {
			continue
		}
		fj, err := fr.FirstJob(ctx, r.Name)
		if err != nil {
			out = append(out, Finding{Check: "first-job", Status: Warn, What: fmt.Sprintf("%s: can't list its runners and recent jobs: %v", r.Name, err),
				Fix: "give the token Administration: read and Actions: read on " + r.Name + ", then sudo tentmaker connect github"})
			continue
		}
		switch {
		case len(fj.Online) > 0:
			out = append(out, Finding{Check: "first-job", Status: OK, What: fmt.Sprintf("%s: Tentmaker runner %s is online", r.Name, strings.Join(fj.Online, ", "))})
		case fj.RanOn != "":
			out = append(out, Finding{Check: "first-job", Status: OK, What: fmt.Sprintf("%s: job '%s' ran on Tentmaker runner %s (%s)", r.Name, fj.RanJob, fj.RanOn, fj.RanAt.UTC().Format(time.RFC3339))})
		case fj.Queued > 0 && now().Sub(fj.OldestQueued) > FirstJobGrace:
			out = append(out, Finding{Check: "first-job", Status: Fail,
				What: fmt.Sprintf("%s: %d job(s) for the Tentmaker pool have waited since %s and no Tentmaker runner came online", r.Name, fj.Queued, fj.OldestQueued.UTC().Format(time.RFC3339)),
				Fix:  "make sure the service runs and is not paused (tentmaker status), that this device has room for a job (tentmaker budget), and that the token is a repo admin (runners need Administration: write); then read the broker's error with tentmaker logs"})
		case fj.Queued > 0:
			out = append(out, Finding{Check: "first-job", Status: Warn,
				What: fmt.Sprintf("%s: %d job(s) for the Tentmaker pool are queued; a runner should come online within a minute", r.Name, fj.Queued),
				Fix:  "run tentmaker doctor again in a minute"})
		default:
			out = append(out, Finding{Check: "first-job", Status: Warn,
				What: r.Name + ": no job has run on a Tentmaker runner yet",
				Fix:  "point a workflow at the pool from a checkout of " + r.Name + ": tentmaker migrate --pr; merge it or push to it, then run tentmaker doctor again"})
		}
	}
	return out
}

func checkTailscale(ctx context.Context, in DoctorInput) []Finding {
	if !in.Config.Mesh.Enabled {
		return []Finding{{Check: "tailscale", Status: OK, What: "mesh is off; this device works alone"}}
	}
	if in.Tailscale == nil {
		return nil
	}
	self, err := in.Tailscale.Self(ctx)
	if err != nil {
		return []Finding{{Check: "tailscale", Status: Warn, What: "tailscale is not up, so this device runs only its own repos: " + firstLineOf(err.Error()),
			Fix: "install Tailscale and run `tailscale up`, or `sudo tentmaker setup --no-mesh`"}}
	}
	return []Finding{{Check: "tailscale", Status: OK, What: "on the tailnet as " + firstNonEmpty(self.DNSName, self.HostName)}}
}

func checkDisk(ctx context.Context, in DoctorInput) []Finding {
	if in.Probe == nil {
		return nil
	}
	dir := in.Config.StateDir
	free, err := in.Probe.FreeDisk(ctx, dir)
	if err != nil {
		return []Finding{{Check: "disk", Status: Warn, What: "can't read free disk at " + dir + ": " + firstLineOf(err.Error()), Fix: "sudo tentmaker setup creates it"}}
	}
	min := int64(in.Config.Guards.MinFreeDisk)
	if free < min {
		return []Finding{{Check: "disk", Status: Fail, What: fmt.Sprintf("%s free at %s, below min_free_disk %s; no new jobs", guards.FormatBytes(free), dir, in.Config.Guards.MinFreeDisk),
			Fix: "free disk space, or lower guards.min_free_disk"}}
	}
	return []Finding{{Check: "disk", Status: OK, What: guards.FormatBytes(free) + " free at " + dir}}
}

func checkVersion(ctx context.Context, in DoctorInput) []Finding {
	ch := in.Channel
	if ch == "" {
		ch = in.Config.Update.Channel
	}
	f := Finding{Check: "version", Status: OK, What: "tentmaker " + in.Version + " (" + ch + ")"}
	if in.Update == nil {
		return []Finding{f}
	}
	rel, newer, err := in.Update(ctx, ch, in.Version)
	switch {
	case err != nil:
		f.What += "; can't check for updates: " + firstLineOf(err.Error())
	case newer:
		f.Status = Warn
		f.What += "; " + rel.Version + " is available"
		f.Fix = "sudo tentmaker update"
	default:
		f.What += ", up to date"
	}
	return []Finding{f}
}

// ProblemList turns the failing and warning findings into core.Problems.
func (r Report) ProblemList() []core.Problem {
	var out []core.Problem
	for _, f := range r.Checks {
		if f.Status != OK {
			out = append(out, core.Problem{Check: f.Check, What: f.What, Fix: f.Fix})
		}
	}
	return out
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
