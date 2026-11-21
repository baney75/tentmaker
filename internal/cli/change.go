package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/daemon"
	"github.com/baney75/tentmaker/internal/forge/github"
)

const (
	usageBudget = `tentmaker budget [--json]
tentmaker budget set [--memory S] [--boost-memory S] [--cpus N] [--boost-cpus N]
                     [--on-battery everyday|off] [--boost-needs-idle true|false]
                     [--pause-below PCT] [--stop-below PCT] [--json]`
	usageSchedule = `tentmaker schedule list [--json]
tentmaker schedule add --name N --days mon-fri|sat,sun|daily --from HH:MM --to HH:MM --profile everyday|boost|off [--json]
tentmaker schedule remove NAME [--json]`
	usageRepo = `tentmaker repo list [--json]
tentmaker repo add OWNER/NAME [--forge github|forgejo|gitlab] [--mode runner|checks]
                   [--allow-public] [--workflow PATH]... [--author LOGIN]... [--enable] [--json]
tentmaker repo remove OWNER/NAME [--json]`
)

// changed is the answer of every command that edits the config.
type changed struct {
	OK     bool   `json:"ok"`
	Config string `json:"config"`
	Note   string `json:"note"` // applied, or why not yet
}

// ---------------------------------------------------------------- budget

func (a *app) cmdBudget(args []string) (any, error) {
	if len(args) > 0 && args[0] == "set" {
		return a.cmdBudgetSet(args[1:])
	}
	f := a.flags("budget", usageBudget)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker budget [set ...]", "unknown budget subcommand %q", pos[0])
	}
	v, err := a.budgetData(a.ctx)
	if err != nil {
		return nil, err
	}
	a.printBudget(v)
	return v, nil
}

// budgetChange is a partial budget edit; zero values keep the current one.
type budgetChange struct {
	Memory, BoostMemory string
	CPUs, BoostCPUs     float64
	OnBattery           string
	BoostNeedsIdle      *bool
	PauseBelow          *int
	StopBelow           *int
}

type budgetSetView struct {
	changed
	Budget config.Budget `json:"budget"`
}

func (a *app) setBudget(ch budgetChange) (budgetSetView, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return budgetSetView{}, err
	}
	b := &cfg.Budget
	for _, s := range []struct {
		flag string
		v    string
		dst  *config.Size
	}{{"--memory", ch.Memory, &b.Everyday.Memory}, {"--boost-memory", ch.BoostMemory, &b.Boost.Memory}} {
		if s.v == "" {
			continue
		}
		size, err := config.ParseSize(s.v)
		if err != nil || size <= 0 {
			return budgetSetView{}, usage("write it like 8GiB", "%s %q is not a memory size", s.flag, s.v)
		}
		*s.dst = size
	}
	if ch.CPUs < 0 || ch.BoostCPUs < 0 {
		return budgetSetView{}, usage("use a positive number of CPUs", "CPUs can't be negative")
	}
	if ch.CPUs > 0 {
		b.Everyday.CPUs = ch.CPUs
	}
	if ch.BoostCPUs > 0 {
		b.Boost.CPUs = ch.BoostCPUs
	}
	if ch.OnBattery != "" {
		b.OnBattery = ch.OnBattery
	}
	if ch.BoostNeedsIdle != nil {
		b.BoostNeedsIdle = *ch.BoostNeedsIdle
	}
	if ch.PauseBelow != nil {
		b.PauseBelow = *ch.PauseBelow
	}
	if ch.StopBelow != nil {
		b.StopBelow = *ch.StopBelow
	}
	if err := a.saveConfig(cfg); err != nil {
		return budgetSetView{}, err
	}
	return budgetSetView{changed: changed{OK: true, Config: a.env.ConfigPath, Note: a.reload()}, Budget: cfg.Budget}, nil
}

func (a *app) cmdBudgetSet(args []string) (any, error) {
	f := a.flags("budget set", usageBudget)
	var ch budgetChange
	f.StringVar(&ch.Memory, "memory", "", "everyday memory")
	f.StringVar(&ch.BoostMemory, "boost-memory", "", "boost memory")
	f.Float64Var(&ch.CPUs, "cpus", 0, "everyday CPUs")
	f.Float64Var(&ch.BoostCPUs, "boost-cpus", 0, "boost CPUs")
	f.StringVar(&ch.OnBattery, "on-battery", "", "everyday or off")
	idle := f.String("boost-needs-idle", "", "true or false")
	pause := f.Int("pause-below", -1, "battery percent")
	stop := f.Int("stop-below", -1, "battery percent")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+firstLine(usageBudget), "unexpected argument %q", pos[0])
	}
	for _, s := range []struct{ flag, v string }{{"--memory", ch.Memory}, {"--boost-memory", ch.BoostMemory}} {
		if s.v == "" {
			continue
		}
		if size, err := config.ParseSize(s.v); err != nil || size <= 0 {
			return nil, usage("write it like 8GiB", "%s %q is not a memory size", s.flag, s.v)
		}
	}
	if ch.CPUs < 0 || ch.BoostCPUs < 0 {
		return nil, usage("use a positive number of CPUs", "CPUs can't be negative")
	}
	switch ch.OnBattery {
	case "", "everyday", "off":
	case "boost":
		return nil, usage("use --on-battery everyday or off", "the budget never boosts on battery")
	default:
		return nil, usage("use --on-battery everyday or off", "--on-battery %q is not everyday or off", ch.OnBattery)
	}
	if *idle != "" {
		v, err := strconv.ParseBool(*idle)
		if err != nil {
			return nil, usage("use --boost-needs-idle true or false", "--boost-needs-idle %q is not true or false", *idle)
		}
		ch.BoostNeedsIdle = &v
	}
	for _, p := range []struct {
		flag string
		v    *int
		dst  **int
	}{{"--pause-below", pause, &ch.PauseBelow}, {"--stop-below", stop, &ch.StopBelow}} {
		if f.set(strings.TrimPrefix(p.flag, "--")) {
			if *p.v < 0 || *p.v > 100 {
				return nil, usage("use a percent from 0 to 100", "%s %d is not a percent", p.flag, *p.v)
			}
			*p.dst = p.v
		}
	}
	if ch == (budgetChange{}) {
		return nil, usage("usage: "+strings.Split(usageBudget, "\n")[1], "budget set: nothing to change")
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	v, err := a.setBudget(ch)
	if err != nil {
		return nil, err
	}
	a.out("budget saved: everyday %s, boost %s (%s)\n", v.Budget.Everyday.Memory, v.Budget.Boost.Memory, v.Note)
	return v, nil
}

// ---------------------------------------------------------------- schedule

var dayOrder = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// parseDays reads "daily", "mon-fri", "sat,sun" or "fri-mon".
func parseDays(s string) ([]string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "daily" || s == "every day" {
		return nil, nil
	}
	idx := func(d string) int {
		for i, x := range dayOrder {
			if x == d {
				return i
			}
		}
		return -1
	}
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if from, to, ok := strings.Cut(part, "-"); ok {
			i, j := idx(from), idx(to)
			if i < 0 || j < 0 {
				return nil, fmt.Errorf("%q: use day names mon..sun", part)
			}
			for k := i; ; k = (k + 1) % 7 {
				add(dayOrder[k])
				if k == j {
					break
				}
			}
			continue
		}
		if idx(part) < 0 {
			return nil, fmt.Errorf("%q is not a day; use mon..sun", part)
		}
		add(part)
	}
	return out, nil
}

type scheduleView struct {
	changed
	Schedule []config.Window `json:"schedule"`
}

func (a *app) cmdSchedule(args []string) (any, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if wantsHelp(args) {
			fmt.Fprintln(a.env.Stdout, usageSchedule)
			return nil, nil
		}
		return nil, usage("usage: tentmaker schedule list|add|remove", "schedule needs list, add or remove")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "list":
		f := a.flags("schedule list", usageSchedule)
		if _, err := a.parse(f, args); err != nil {
			return nil, err
		}
		cfg, err := a.mustConfig()
		if err != nil {
			return nil, err
		}
		a.printSchedule(cfg.Budget.Schedule)
		return scheduleView{changed: changed{OK: true, Config: a.env.ConfigPath}, Schedule: nonNil(cfg.Budget.Schedule)}, nil
	case "add":
		f := a.flags("schedule add", usageSchedule)
		name := f.String("name", "", "window name")
		days := f.String("days", "daily", "mon-fri, sat,sun or daily")
		from := f.String("from", "", "HH:MM")
		to := f.String("to", "", "HH:MM")
		profile := f.String("profile", "", "everyday, boost or off")
		pos, err := a.parse(f, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usage("usage: "+strings.Split(usageSchedule, "\n")[1], "unexpected argument %q", pos[0])
		}
		for _, req := range []struct{ flag, v string }{{"--name", *name}, {"--from", *from}, {"--to", *to}, {"--profile", *profile}} {
			if req.v == "" {
				return nil, usage("usage: "+strings.Split(usageSchedule, "\n")[1], "schedule add needs %s", req.flag)
			}
		}
		dl, err := parseDays(*days)
		if err != nil {
			return nil, usage("use --days mon-fri, sat,sun or daily", "--days: %v", err)
		}
		for _, c := range []struct{ flag, v string }{{"--from", *from}, {"--to", *to}} {
			if _, err := config.ParseClock(c.v); err != nil {
				return nil, usage("write it like 09:00", "%s: %v", c.flag, err)
			}
		}
		switch *profile {
		case "everyday", "boost", "off":
		default:
			return nil, usage("use --profile everyday, boost or off", "--profile %q is not everyday, boost or off", *profile)
		}
		if err := a.needRoot(); err != nil {
			return nil, err
		}
		cfg, err := a.mustConfig()
		if err != nil {
			return nil, err
		}
		w := config.Window{Name: *name, Days: dl, From: *from, To: *to, Profile: *profile}
		replaced := false
		for i := range cfg.Budget.Schedule {
			if cfg.Budget.Schedule[i].Name == w.Name {
				cfg.Budget.Schedule[i], replaced = w, true
			}
		}
		if !replaced {
			cfg.Budget.Schedule = append(cfg.Budget.Schedule, w)
		}
		if err := a.saveConfig(cfg); err != nil {
			return nil, err
		}
		note := a.reload()
		a.out("schedule %s saved (%s)\n", w.Name, note)
		return scheduleView{changed: changed{OK: true, Config: a.env.ConfigPath, Note: note}, Schedule: cfg.Budget.Schedule}, nil
	case "remove":
		f := a.flags("schedule remove", usageSchedule)
		name := f.String("name", "", "window name")
		pos, err := a.parse(f, args)
		if err != nil {
			return nil, err
		}
		if *name == "" && len(pos) == 1 {
			*name = pos[0]
		} else if len(pos) > 0 || *name == "" {
			return nil, usage("usage: tentmaker schedule remove NAME", "schedule remove needs one NAME")
		}
		if err := a.needRoot(); err != nil {
			return nil, err
		}
		cfg, err := a.mustConfig()
		if err != nil {
			return nil, err
		}
		var keep []config.Window
		for _, w := range cfg.Budget.Schedule {
			if w.Name != *name {
				keep = append(keep, w)
			}
		}
		if len(keep) == len(cfg.Budget.Schedule) {
			return nil, fail("tentmaker schedule list", "no schedule window named %q", *name)
		}
		cfg.Budget.Schedule = keep
		if err := a.saveConfig(cfg); err != nil {
			return nil, err
		}
		note := a.reload()
		a.out("schedule %s removed (%s)\n", *name, note)
		return scheduleView{changed: changed{OK: true, Config: a.env.ConfigPath, Note: note}, Schedule: nonNil(keep)}, nil
	}
	return nil, usage("usage: tentmaker schedule list|add|remove", "unknown schedule subcommand %q", sub)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func wantsHelp(args []string) bool {
	for _, s := range args {
		if s == "-h" || s == "--help" || s == "-help" {
			return true
		}
	}
	return false
}

func (a *app) printSchedule(ws []config.Window) {
	if a.json {
		return
	}
	if len(ws) == 0 {
		a.out("no schedule windows\n")
		return
	}
	tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDAYS\tFROM\tTO\tPROFILE")
	for _, w := range ws {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", w.Name, daysString(w.Days), w.From, w.To, w.Profile)
	}
	tw.Flush()
}

// ---------------------------------------------------------------- repo

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)

func validRepo(name string) error {
	if !repoRe.MatchString(name) || strings.Contains(name, "..") {
		return usage("write it as OWNER/NAME, like baney75/tentmaker", "%q is not a repository", name)
	}
	return nil
}

func validForge(forge string) error {
	switch forge {
	case "github", "forgejo", "gitlab":
		return nil
	}
	return usage("use github, forgejo or gitlab", "unknown forge %q", forge)
}

func defaultMode(forge string) string {
	if forge == "gitlab" {
		return "checks"
	}
	return "runner"
}

type repoListView struct {
	Repos []config.Repo `json:"repos"`
}

type repoAddOpts struct {
	Name        string
	Forge       string
	Mode        string
	AllowPublic bool
	Workflows   []string
	Authors     []string
	Enable      bool
}

type migrateFile struct {
	File    string   `json:"file"`
	Changed bool     `json:"changed"`
	Report  []string `json:"report"`
	Error   string   `json:"error,omitempty"`
}

type repoAddView struct {
	changed
	Repo            config.Repo   `json:"repo"`
	Added           bool          `json:"added"`
	Checked         bool          `json:"checked"`
	Public          bool          `json:"public"`
	ForkPolicy      string        `json:"fork_policy,omitempty"`
	ForkApprovalSet bool          `json:"fork_approval_set"`
	Migrate         []migrateFile `json:"migrate"`
	Notes           []string      `json:"notes"`
}

// forkApprover is the GitHub client's fork policy setter.
type forkApprover interface {
	SetForkApproval(ctx context.Context, repo string) error
}

func (a *app) repoAdd(ctx context.Context, o repoAddOpts) (repoAddView, error) {
	v := repoAddView{Migrate: []migrateFile{}, Notes: []string{}}
	if err := validRepo(o.Name); err != nil {
		return v, err
	}
	if o.Forge == "" {
		o.Forge = "github"
	}
	if err := validForge(o.Forge); err != nil {
		return v, err
	}
	if o.Mode == "" {
		o.Mode = defaultMode(o.Forge)
	}
	if o.Mode != "runner" && o.Mode != "checks" {
		return v, usage("use --mode runner or checks", "--mode %q is not runner or checks", o.Mode)
	}
	if o.Forge == "gitlab" && o.Mode != "checks" {
		return v, usage("use --mode checks", "GitLab repos run in checks mode")
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return v, err
	}
	repo := config.Repo{Forge: o.Forge, Name: o.Name, AllowPublic: o.AllowPublic, Mode: o.Mode, Workflows: o.Workflows, Authors: o.Authors}
	if repo.Forge == "github" {
		repo.Forge = ""
	}
	probe := cfg
	probe.Repos = []config.Repo{repo}
	forges := daemon.BuildForges(a.resolved(probe), a.env.Secrets, a.env.NewForge, nil)
	f := forges.Forges[o.Forge]
	sf := forges.StatusForges[o.Forge]
	branch := ""
	if f != nil {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		info, err := f.CheckRepo(cctx, o.Name)
		cancel()
		if err != nil {
			return v, fail("check the name and that the token can read it: sudo tentmaker connect "+o.Forge, "can't read %s: %v", o.Name, err)
		}
		v.Checked, v.Public, v.ForkPolicy, branch = true, info.Public, info.ForkPolicy, info.DefaultBranch
		v.Notes = append(v.Notes, info.Problems...)
		if info.Public && !o.AllowPublic {
			return v, fail("sudo tentmaker repo add "+o.Name+" --allow-public --enable",
				"%s is public: anyone can open a pull request, so Tentmaker refuses it unless you pass --allow-public", o.Name)
		}
	} else {
		v.Notes = append(v.Notes, "no "+o.Forge+" token on this device, so the repo was not checked; a broker in the pool must hold the token")
	}
	if o.Enable {
		if f == nil && sf == nil {
			return v, fail("sudo tentmaker connect "+o.Forge, "--enable needs this device's %s token", o.Forge)
		}
		if v.Public {
			fa, ok := f.(forkApprover)
			if !ok {
				v.Notes = append(v.Notes, o.Forge+" has no API for the fork approval setting; it already asks for approval before a first-time contributor's workflows run")
			} else {
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := fa.SetForkApproval(cctx, o.Name)
				cancel()
				if err != nil {
					return v, fail("give the token Administration: read and write, then rerun", "set fork approval on %s: %v", o.Name, err)
				}
				v.ForkApprovalSet = true
			}
		}
		if sf != nil && o.Forge != "gitlab" {
			v.Migrate = a.previewMigration(ctx, sf, o.Name, firstNonEmpty(branch, "HEAD"))
		}
	}
	v.Added = true
	for i, r := range cfg.Repos {
		if strings.EqualFold(r.Name, o.Name) && daemon.RepoForge(r) == o.Forge {
			cfg.Repos[i], v.Added = repo, false
		}
	}
	if v.Added {
		cfg.Repos = append(cfg.Repos, repo)
	}
	if err := a.saveConfig(cfg); err != nil {
		return v, err
	}
	v.Repo = repo
	v.changed = changed{OK: true, Config: a.env.ConfigPath, Note: a.reload()}
	return v, nil
}

// statusFiles is what previewMigration needs from a forge.
type statusFiles interface {
	FileAt(ctx context.Context, repo, sha, path string) ([]byte, error)
	ListDir(ctx context.Context, repo, sha, path string) ([]string, error)
}

// previewMigration runs MigrateWorkflow over the repo's workflows on its
// default branch, without writing anything.
func (a *app) previewMigration(ctx context.Context, sf statusFiles, repo, ref string) []migrateFile {
	plat := daemon.HostPlatform(a.env.GOOS, a.env.GOARCH).OS
	out := []migrateFile{}
	for _, dir := range []string{".github/workflows", ".forgejo/workflows"} {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		names, err := sf.ListDir(cctx, repo, ref, dir)
		cancel()
		if err != nil {
			continue
		}
		for _, n := range names {
			if !strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, ".yaml") {
				continue
			}
			p := path.Join(dir, path.Base(n))
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			src, err := sf.FileAt(cctx, repo, ref, p)
			cancel()
			mf := migrateFile{File: p, Report: []string{}}
			if err != nil {
				mf.Error = err.Error()
				out = append(out, mf)
				continue
			}
			_, report, ch, err := github.MigrateWorkflow(src, plat)
			mf.Changed, mf.Report = ch, nonNil(report)
			if err != nil {
				mf.Error = err.Error()
			}
			out = append(out, mf)
		}
	}
	return out
}

func (a *app) repoRemove(name string) (changed, error) {
	if err := validRepo(name); err != nil {
		return changed{}, err
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return changed{}, err
	}
	var keep []config.Repo
	for _, r := range cfg.Repos {
		if !strings.EqualFold(r.Name, name) {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(cfg.Repos) {
		return changed{OK: true, Config: a.env.ConfigPath, Note: name + " was not served; nothing changed"}, nil
	}
	cfg.Repos = keep
	if err := a.saveConfig(cfg); err != nil {
		return changed{}, err
	}
	return changed{OK: true, Config: a.env.ConfigPath, Note: a.reload()}, nil
}

func (a *app) cmdRepo(args []string) (any, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if wantsHelp(args) {
			fmt.Fprintln(a.env.Stdout, usageRepo)
			return nil, nil
		}
		return nil, usage("usage: tentmaker repo list|add|remove", "repo needs list, add or remove")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "list":
		f := a.flags("repo list", usageRepo)
		pos, err := a.parse(f, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usage("usage: tentmaker repo list", "unexpected argument %q", pos[0])
		}
		cfg, err := a.mustConfig()
		if err != nil {
			return nil, err
		}
		if !a.json {
			if len(cfg.Repos) == 0 {
				a.out("no repos; add one with: sudo tentmaker repo add OWNER/NAME\n")
			} else {
				tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "REPO\tFORGE\tMODE\tPUBLIC")
				for _, r := range cfg.Repos {
					pub := "refused"
					if r.AllowPublic {
						pub = "allowed"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, daemon.RepoForge(r), firstNonEmpty(r.Mode, "runner"), pub)
				}
				tw.Flush()
			}
		}
		return repoListView{Repos: nonNil(cfg.Repos)}, nil
	case "add":
		f := a.flags("repo add", usageRepo)
		var o repoAddOpts
		var wf, au multi
		f.StringVar(&o.Forge, "forge", "github", "github, forgejo or gitlab")
		f.StringVar(&o.Mode, "mode", "", "runner or checks")
		f.BoolVar(&o.AllowPublic, "allow-public", false, "serve a public repo")
		f.Var(&wf, "workflow", "workflow path (checks mode, repeatable)")
		f.Var(&au, "author", "PR author whose heads checks mode runs (repeatable)")
		f.BoolVar(&o.Enable, "enable", false, "set the fork approval policy and preview the migration")
		pos, err := a.parse(f, args)
		if err != nil {
			return nil, err
		}
		if len(pos) != 1 {
			return nil, usage("usage: tentmaker repo add OWNER/NAME [flags]", "repo add needs one OWNER/NAME")
		}
		o.Name, o.Workflows, o.Authors = pos[0], wf, au
		if err := validRepo(o.Name); err != nil {
			return nil, err
		}
		if err := validForge(o.Forge); err != nil {
			return nil, err
		}
		if err := a.needRoot(); err != nil {
			return nil, err
		}
		v, err := a.repoAdd(a.ctx, o)
		if err != nil {
			return nil, err
		}
		if !a.json {
			verb := "added"
			if !v.Added {
				verb = "updated"
			}
			a.out("%s %s (%s, %s mode); %s\n", verb, o.Name, o.Forge, v.Repo.Mode, v.Note)
			if v.ForkApprovalSet {
				a.out("fork pull requests from outside contributors now need approval before they run\n")
			}
			for _, n := range v.Notes {
				a.out("note: %s\n", n)
			}
			a.printMigration(v.Migrate, false)
		}
		return v, nil
	case "remove":
		f := a.flags("repo remove", usageRepo)
		pos, err := a.parse(f, args)
		if err != nil {
			return nil, err
		}
		if len(pos) != 1 {
			return nil, usage("usage: tentmaker repo remove OWNER/NAME", "repo remove needs one OWNER/NAME")
		}
		if err := validRepo(pos[0]); err != nil {
			return nil, err
		}
		if err := a.needRoot(); err != nil {
			return nil, err
		}
		v, err := a.repoRemove(pos[0])
		if err != nil {
			return nil, err
		}
		a.out("%s: %s\n", pos[0], v.Note)
		return v, nil
	}
	return nil, usage("usage: tentmaker repo list|add|remove", "unknown repo subcommand %q", sub)
}

func (a *app) printMigration(files []migrateFile, wrote bool) {
	if a.json || len(files) == 0 {
		return
	}
	n := 0
	for _, f := range files {
		if f.Changed {
			n++
		}
	}
	verb := "would change"
	if wrote {
		verb = "changed"
	}
	a.out("\nmigrate %s %d of %d workflow file(s):\n", verb, n, len(files))
	for _, f := range files {
		a.out("  %s\n", f.File)
		for _, r := range f.Report {
			a.out("    %s\n", r)
		}
		if f.Error != "" {
			a.out("    error: %s\n", f.Error)
		}
	}
}

// ---------------------------------------------------------------- connect

type connectView struct {
	changed
	Forge  string `json:"forge"`
	URL    string `json:"url,omitempty"`
	Secret string `json:"secret"`
	Login  string `json:"login"`
	Source string `json:"source"` // stdin, gh, prompt
}

// readTokenStdin reads a token from stdin; it never echoes it anywhere.
func (a *app) readTokenStdin() (string, error) {
	b, err := io.ReadAll(io.LimitReader(a.in, 64<<10))
	if err != nil {
		return "", fail("pipe the token on stdin", "read the token from stdin: %v", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", usage("pipe the token: ... | tentmaker connect github --token-stdin", "--token-stdin got an empty stdin")
	}
	if strings.ContainsAny(tok, " \t\n\r") {
		return "", usage("pipe only the token", "stdin holds more than one token")
	}
	return tok, nil
}

// tokenHelp is where to make a token and what it needs, by forge.
func tokenHelp(forge, mode string) string {
	switch forge {
	case "forgejo":
		return "Make a token in your Forgejo user settings > Applications with scopes write:repository and read:user (and write:admin is not needed)."
	case "gitlab":
		return "Make a token at https://gitlab.com/-/user_settings/personal_access_tokens (or your GitLab's) with scope api."
	}
	perms := "Administration: read and write, Actions: read and write, Metadata: read"
	if mode == "checks" {
		perms = "Commit statuses: read and write, Contents: read, Pull requests: read, Metadata: read"
	}
	return "Make a fine-grained token at https://github.com/settings/personal-access-tokens/new for the repos you serve, with: " + perms + "."
}

// obtainToken gets a forge token from stdin, gh or the terminal. It returns
// "" with no error when none is available.
func (a *app) obtainToken(ctx context.Context, forge, mode string, fromStdin, noGH, prompt bool) (tok, source string, err error) {
	if fromStdin {
		tok, err := a.readTokenStdin()
		return tok, "stdin", err
	}
	if forge == "github" && !noGH {
		user := ""
		if a.env.Getenv != nil {
			user = a.env.Getenv("SUDO_USER")
		}
		if t, err := a.env.GHToken(ctx, user); err == nil && t != "" {
			return t, "gh", nil
		}
	}
	if prompt && a.env.StdinTTY {
		fmt.Fprintln(a.env.Stderr, tokenHelp(forge, mode))
		fmt.Fprint(a.env.Stderr, "Paste the token (it won't show; Enter to skip): ")
		b, err := a.env.ReadSecret()
		fmt.Fprintln(a.env.Stderr)
		if err != nil {
			return "", "", fail("use --token-stdin", "read the token: %v", err)
		}
		return strings.TrimSpace(string(b)), "prompt", nil
	}
	return "", "", nil
}

// storeToken verifies tok with one API call and stores it.
func (a *app) storeToken(ctx context.Context, kind, rawURL, tok string) (secret, login string, err error) {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	login, err = a.env.VerifyToken(vctx, kind, rawURL, tok)
	cancel()
	if err != nil {
		return "", "", fail(tokenHelp(kind, ""), "the %s token did not work: %v", kind, err)
	}
	secret = daemon.SecretName(kind, rawURL)
	if err := a.env.Secrets.Set(secret, []byte(tok)); err != nil {
		return "", "", fail("rerun with sudo", "store the token: %v", err)
	}
	return secret, login, nil
}

func (a *app) cmdConnect(args []string) (any, error) {
	f := a.flags("connect", commands["connect"].usage)
	rawURL := f.String("url", "", "forge URL (self-hosted)")
	fromStdin := f.Bool("token-stdin", false, "read the token from stdin")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, usage("usage: "+commands["connect"].usage, "connect needs one forge: github, forgejo or gitlab")
	}
	kind := pos[0]
	if err := validForge(kind); err != nil {
		return nil, err
	}
	if *rawURL != "" {
		u, err := url.Parse(*rawURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, usage("write it like https://codeberg.org", "--url %q is not an http(s) URL", *rawURL)
		}
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return nil, err
	}
	fc := daemon.ForgeConfig(cfg, kind)
	if *rawURL != "" {
		fc.URL = strings.TrimRight(*rawURL, "/")
	}
	if kind == "forgejo" && fc.URL == "" {
		return nil, usage("sudo tentmaker connect forgejo --url https://your.forge", "Forgejo needs --url")
	}
	tok, source, err := a.obtainToken(a.ctx, kind, "", *fromStdin, false, !a.json)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, usage("pipe one: ... | sudo tentmaker connect "+kind+" --token-stdin", "no %s token: pass --token-stdin%s", kind, map[bool]string{true: ", or sign in with `gh auth login`", false: ""}[kind == "github"])
	}
	secret, login, err := a.storeToken(a.ctx, kind, fc.URL, tok)
	if err != nil {
		return nil, err
	}
	if cfg.Forges == nil {
		cfg.Forges = map[string]config.Forge{}
	}
	cfg.Forges[kind] = fc
	if err := a.saveConfig(cfg); err != nil {
		return nil, err
	}
	v := connectView{changed: changed{OK: true, Config: a.env.ConfigPath, Note: a.reload()}, Forge: kind, URL: fc.URL, Secret: secret, Login: login, Source: source}
	a.out("connected %s as %s; token stored as secret %s (%s)\n", kind, firstNonEmpty(login, "?"), secret, v.Note)
	return v, nil
}

// ---------------------------------------------------------------- pause

type pauseView struct {
	OK     bool   `json:"ok"`
	Paused bool   `json:"paused"`
	Reason string `json:"reason,omitempty"`
	ForS   int64  `json:"for_s,omitempty"`
}

func (a *app) pause(ctx context.Context, reason string, d time.Duration) (pauseView, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return pauseView{}, err
	}
	err = a.controlClient(cfg).Post(ctx, "/v1/pause", control.PauseRequest{Reason: reason, ForS: int64(d / time.Second)})
	if err != nil {
		return pauseView{}, serviceErr(err)
	}
	return pauseView{OK: true, Paused: true, Reason: reason, ForS: int64(d / time.Second)}, nil
}

func (a *app) resume(ctx context.Context) (pauseView, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return pauseView{}, err
	}
	if err := a.controlClient(cfg).Post(ctx, "/v1/resume", nil); err != nil {
		return pauseView{}, serviceErr(err)
	}
	return pauseView{OK: true}, nil
}

func serviceErr(err error) error {
	if errors.Is(err, control.ErrNotRunning) {
		return fail("sudo tentmaker setup (it starts the service), then try again", "service not running")
	}
	return fail("tentmaker doctor", "%v", err)
}

func (a *app) cmdPause(args []string) (any, error) {
	f := a.flags("pause", commands["pause"].usage)
	dur := f.String("for", "", "how long, like 30m or 2h")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	var d time.Duration
	if *dur != "" {
		d, err = time.ParseDuration(*dur)
		if err != nil || d <= 0 {
			return nil, usage("write it like 30m or 2h", "--for %q is not a duration", *dur)
		}
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	v, err := a.pause(a.ctx, strings.Join(pos, " "), d)
	if err != nil {
		return nil, err
	}
	if d > 0 {
		a.out("paused for %s; running jobs finish\n", d)
	} else {
		a.out("paused until `sudo tentmaker resume`; running jobs finish\n")
	}
	return v, nil
}

func (a *app) cmdResume(args []string) (any, error) {
	f := a.flags("resume", commands["resume"].usage)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker resume", "unexpected argument %q", pos[0])
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	v, err := a.resume(a.ctx)
	if err != nil {
		return nil, err
	}
	a.out("resumed\n")
	return v, nil
}
