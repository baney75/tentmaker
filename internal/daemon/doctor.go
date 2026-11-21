package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
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
	// SkipForges, when set, says why the forge checks didn't run.
	SkipForges  string
	Tailscale   mesh.Tailscale // nil when mesh is off
	Service     func(ctx context.Context) (service.State, error)
	ServiceNote string // set when the caller already knows (the daemon is the service)
	Probe       guards.Probe
	Update      func(ctx context.Context, channel, current string) (update.Release, bool, error)
	// Extra are problems the caller found building components.
	Extra []core.Problem
}

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
		{"tailscale", func(ctx context.Context) []Finding { return checkTailscale(ctx, in) }},
		{"disk", func(ctx context.Context) []Finding { return checkDisk(ctx, in) }},
		{"version", func(ctx context.Context) []Finding { return checkVersion(ctx, in) }},
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

func checkExecutors(ctx context.Context, in DoctorInput) []Finding {
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
		if info.Public && r.AllowPublic && info.ForkPolicy != "approve_all" && info.ForkPolicy != "approve_first_time" {
			bad = true
			out = append(out, Finding{Check: "forge", Status: Fail, What: fmt.Sprintf("%s is public and fork PRs run without approval (policy %q)", r.Name, info.ForkPolicy),
				Fix: "sudo tentmaker repo add " + r.Name + " --allow-public --enable"})
		}
		if !bad {
			out = append(out, Finding{Check: "forge", Status: OK, What: r.Name + ": token and repo settings are fine"})
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
