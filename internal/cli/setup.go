package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/daemon"
	"github.com/baney75/tentmaker/internal/secrets"
	"github.com/baney75/tentmaker/internal/service"
)

const usageSetup = `tentmaker setup [flags]

Install and configure this device. On a terminal it asks; with --yes every
missing value takes its default.

  --yes                  take defaults, never ask
  --device NAME          default: hostname, lowercased, [a-z0-9-] only
  --memory S             everyday memory, like 4GiB
  --boost-memory S       plugged-in memory, like 12GiB
  --cpus N --boost-cpus N
  --on-battery everyday|off
  --repo OWNER/NAME      repeatable
  --forge github|forgejo|gitlab   default github
  --forge-url URL        for forgejo or a self-hosted gitlab
  --mode runner|checks   default runner (checks for gitlab)
  --token-stdin          read the forge token from stdin
  --no-gh                don't try ` + "`gh auth token`" + `
  --pool NAME            default "default"
  --no-mesh              single device, no Tailscale
  --channel stable|dev   update channel
  --no-service           don't install the service
  --json`

var poolRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

type setupOpts struct {
	yes                 bool
	device              string
	memory, boostMemory string
	cpus, boostCPUs     float64
	onBattery           string
	repos               multi
	forge, forgeURL     string
	mode                string
	tokenStdin, noGH    bool
	pool                string
	noMesh              bool
	channel             string
	noService           bool
}

type setupStep struct {
	Step   string `json:"step"`
	Status string `json:"status"` // ok, warn, fail, skip
	Detail string `json:"detail"`
}

type setupView struct {
	OK      bool           `json:"ok"`
	Config  string         `json:"config"`
	Device  string         `json:"device"`
	Forge   string         `json:"forge"`
	Secret  string         `json:"secret,omitempty"` // the secret's name, never its value
	Login   string         `json:"login,omitempty"`
	Repos   []string       `json:"repos"`
	Service string         `json:"service"`
	Steps   []setupStep    `json:"steps"`
	Doctor  *daemon.Report `json:"doctor,omitempty"`
	Next    string         `json:"next"`
}

func (v setupView) exitCode() int {
	if v.OK {
		return ExitOK
	}
	return ExitFailed
}

func (a *app) step(v *setupView, name, status, format string, args ...any) {
	s := setupStep{Step: name, Status: status, Detail: fmt.Sprintf(format, args...)}
	v.Steps = append(v.Steps, s)
	if status == daemon.Fail {
		v.OK = false
	}
	a.out("%s %-9s %s\n", a.paint(status, fmt.Sprintf("%-4s", status)), name, s.Detail)
}

func (a *app) cmdSetup(args []string) (any, error) {
	f := a.flags("setup", usageSetup)
	var o setupOpts
	f.BoolVar(&o.yes, "yes", false, "take defaults, never ask")
	f.StringVar(&o.device, "device", "", "device name")
	f.StringVar(&o.memory, "memory", "", "everyday memory")
	f.StringVar(&o.boostMemory, "boost-memory", "", "boost memory")
	f.Float64Var(&o.cpus, "cpus", 0, "everyday CPUs")
	f.Float64Var(&o.boostCPUs, "boost-cpus", 0, "boost CPUs")
	f.StringVar(&o.onBattery, "on-battery", "", "everyday or off")
	f.Var(&o.repos, "repo", "OWNER/NAME (repeatable)")
	f.StringVar(&o.forge, "forge", "github", "github, forgejo or gitlab")
	f.StringVar(&o.forgeURL, "forge-url", "", "forge URL")
	f.StringVar(&o.mode, "mode", "", "runner or checks")
	f.BoolVar(&o.tokenStdin, "token-stdin", false, "read the token from stdin")
	f.BoolVar(&o.noGH, "no-gh", false, "don't try gh auth token")
	f.StringVar(&o.pool, "pool", "", "mesh pool")
	f.BoolVar(&o.noMesh, "no-mesh", false, "no Tailscale mesh")
	f.StringVar(&o.channel, "channel", "", "stable or dev")
	f.BoolVar(&o.noService, "no-service", false, "don't install the service")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker setup [flags]", "unexpected argument %q", pos[0])
	}
	if err := a.checkSetupFlags(f, &o); err != nil {
		return nil, err
	}
	if !o.yes && !a.env.StdinTTY {
		return nil, usage("sudo "+"tentmaker "+shellJoin(a.args)+" --yes", "stdin is not a terminal, so setup can't ask; pass --yes to take the defaults")
	}
	interactive := !o.yes

	// Step 1: check, write nothing yet.
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	v := &setupView{OK: true, Config: a.env.ConfigPath, Forge: o.forge, Repos: []string{}, Steps: []setupStep{}}
	switch a.env.GOOS {
	case "linux", "darwin", "windows":
	default:
		return nil, fail("use Linux, macOS or Windows", "Tentmaker does not run on %s", a.env.GOOS)
	}
	plat := daemon.HostPlatform(a.env.GOOS, a.env.GOARCH)
	if plat.Arch == "" {
		return nil, fail("use an amd64 or arm64 machine", "Tentmaker does not run on %s", a.env.GOARCH)
	}
	cfg, exists, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	a.step(v, "check", daemon.OK, "%s %s, %s memory, %d CPUs; config %s", plat.OS, plat.Arch, gib(a.totalMemory()), a.env.CPUs(),
		map[bool]string{true: "found, keeping its values", false: "is new"}[exists])
	if !exists {
		cfg.Update.Channel = daemon.Channel(a.env.Version)
	}

	// Step 2: flags over the existing values, then the wizard.
	if err := a.applySetupFlags(f, o, &cfg); err != nil {
		return nil, err
	}
	if interactive {
		if err := a.wizard(f, &o, &cfg); err != nil {
			return nil, err
		}
		a.step(v, "wizard", daemon.OK, "answers taken")
	} else {
		a.step(v, "wizard", "skip", "--yes: defaults and flags")
	}
	if ps := cfg.Validate(); len(ps) > 0 {
		return nil, usage("change the value and run setup again", "%s", strings.Join(ps, "; "))
	}
	v.Device = cfg.Device
	for _, r := range cfg.Repos {
		v.Repos = append(v.Repos, r.Name)
	}

	// Step 3: the forge token.
	if err := a.setupToken(o, interactive, &cfg, v); err != nil {
		return nil, err
	}

	// Step 4: save, then the sandbox.
	if err := a.saveConfig(cfg); err != nil {
		return nil, err
	}
	rc := a.resolved(cfg)
	binary := daemon.NewBinary(daemon.BinaryOptions{Version: a.env.Version, CacheDir: rc.CacheDir, Executable: a.env.Executable})
	if err := a.env.InstallExecutors(a.ctx, rc, binary); err != nil {
		a.step(v, "save", daemon.OK, "config saved to %s", a.env.ConfigPath)
		a.step(v, "sandbox", daemon.Fail, "%v", err)
	} else {
		a.step(v, "save", daemon.OK, "config saved to %s", a.env.ConfigPath)
		a.step(v, "sandbox", daemon.OK, "executors installed for %s", plat.OS)
	}

	// Step 5: the service.
	v.Service = "not installed"
	if o.noService {
		a.step(v, "service", "skip", "--no-service")
	} else if err := a.setupService(v); err != nil {
		a.step(v, "service", daemon.Fail, "%v", err)
	}

	// Step 6: doctor and the next step.
	dv := a.doctorData(a.ctx)
	v.Doctor = &dv.Report
	switch {
	case !dv.OK:
		a.step(v, "doctor", daemon.Warn, "%d problem(s), %d warning(s); run `tentmaker doctor` for the fixes", dv.Problems, dv.Warnings)
	case dv.Warnings > 0:
		a.step(v, "doctor", daemon.Warn, "%d warning(s); run `tentmaker doctor`", dv.Warnings)
	default:
		a.step(v, "doctor", daemon.OK, "all good")
	}
	v.Next = setupNext(*v, cfg)
	a.out("\nnext: %s\n", v.Next)
	return *v, nil
}

func setupNext(v setupView, cfg config.Config) string {
	switch {
	case !v.OK:
		return "fix the failed step above and run `sudo tentmaker setup` again"
	case len(cfg.Repos) == 0:
		return "add a repo: sudo tentmaker repo add OWNER/NAME --enable"
	case v.Secret == "":
		return "this device is a worker for its pool; connect a forge to broker work here: sudo tentmaker connect github"
	default:
		return "point workflows at Tentmaker: tentmaker migrate PATH (then --write)"
	}
}

// checkSetupFlags validates flag values before anything else, so a typo is
// a usage error (exit 2) even without root.
func (a *app) checkSetupFlags(f *flags, o *setupOpts) error {
	if err := validForge(o.forge); err != nil {
		return err
	}
	if o.mode == "" {
		o.mode = defaultMode(o.forge)
	}
	if o.mode != "runner" && o.mode != "checks" {
		return usage("use --mode runner or checks", "--mode %q is not runner or checks", o.mode)
	}
	if checksOnly(o.forge) && o.mode != "checks" {
		return usage("use --mode checks", "%s repos run in checks mode", o.forge)
	}
	for _, s := range []struct{ flag, v string }{{"--memory", o.memory}, {"--boost-memory", o.boostMemory}} {
		if s.v == "" {
			continue
		}
		if size, err := config.ParseSize(s.v); err != nil || size <= 0 {
			return usage("write it like 8GiB", "%s %q is not a memory size", s.flag, s.v)
		}
	}
	if o.cpus < 0 || o.boostCPUs < 0 {
		return usage("use a positive number of CPUs", "CPUs can't be negative")
	}
	switch o.onBattery {
	case "", "everyday", "off":
	default:
		return usage("use --on-battery everyday or off", "--on-battery %q: use everyday or off (the budget never boosts on battery)", o.onBattery)
	}
	for _, r := range o.repos {
		if err := validRepo(r); err != nil {
			return err
		}
	}
	if o.forgeURL != "" {
		u, err := url.Parse(o.forgeURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return usage("write it like https://codeberg.org", "--forge-url %q is not an http(s) URL", o.forgeURL)
		}
	}
	if o.pool != "" && !poolRe.MatchString(o.pool) {
		return usage("use letters, digits, dot, dash or underscore", "--pool %q is not a pool name", o.pool)
	}
	switch o.channel {
	case "", "stable", "dev":
	default:
		return usage("use --channel stable or dev", "--channel %q is not stable or dev", o.channel)
	}
	if o.device != "" && daemon.SanitizeDevice(o.device) != o.device {
		return usage("use lowercase letters, digits and dashes, like "+daemon.SanitizeDevice(o.device), "--device %q is not a device name", o.device)
	}
	if o.tokenStdin && !o.yes {
		return usage("add --yes: the token takes stdin, so setup can't ask", "--token-stdin needs --yes")
	}
	_ = f
	return nil
}

// applySetupFlags writes the flags that were given over cfg; anything not
// given keeps its current value, so a rerun changes only what it names.
func (a *app) applySetupFlags(f *flags, o setupOpts, cfg *config.Config) error {
	if o.device != "" {
		cfg.Device = o.device
	}
	if cfg.Device == "" {
		cfg.Device = daemon.DeviceName(*cfg, a.env.Hostname)
	}
	if o.memory != "" {
		cfg.Budget.Everyday.Memory, _ = config.ParseSize(o.memory)
	}
	if o.boostMemory != "" {
		cfg.Budget.Boost.Memory, _ = config.ParseSize(o.boostMemory)
	}
	if o.cpus > 0 {
		cfg.Budget.Everyday.CPUs = o.cpus
	}
	if o.boostCPUs > 0 {
		cfg.Budget.Boost.CPUs = o.boostCPUs
	}
	if o.onBattery != "" {
		cfg.Budget.OnBattery = o.onBattery
	}
	if o.pool != "" {
		cfg.Mesh.Pool = o.pool
	}
	if o.noMesh {
		cfg.Mesh.Enabled = false
	}
	if o.channel != "" {
		cfg.Update.Channel = o.channel
	}
	if o.forgeURL != "" {
		if cfg.Forges == nil {
			cfg.Forges = map[string]config.Forge{}
		}
		cfg.Forges[o.forge] = config.Forge{Kind: o.forge, URL: strings.TrimRight(o.forgeURL, "/")}
	}
	if o.forge == "forgejo" && daemon.ForgeConfig(*cfg, "forgejo").URL == "" {
		return usage("add --forge-url https://your.forge", "--forge forgejo needs --forge-url")
	}
	for _, name := range o.repos {
		addRepo(cfg, name, o.forge, o.mode, f.set("mode"))
	}
	return nil
}

func addRepo(cfg *config.Config, name, forge, mode string, setMode bool) {
	stored := forge
	if forge == "github" {
		stored = ""
	}
	for i, r := range cfg.Repos {
		if strings.EqualFold(r.Name, name) && daemon.RepoForge(r) == forge {
			if setMode {
				cfg.Repos[i].Mode = mode
			}
			return
		}
	}
	cfg.Repos = append(cfg.Repos, config.Repo{Forge: stored, Name: name, Mode: mode})
}

// ---------------------------------------------------------------- wizard

// ask prints a prompt with its default to stderr and reads one line;
// Enter takes the default.
func (a *app) ask(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(a.env.Stderr, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(a.env.Stderr, "%s: ", prompt)
	}
	b, err := readLine(a.in)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", usage("rerun with --yes", "setup ended: no answer")
		}
		return "", usage("rerun with --yes", "setup ended: %v", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return def, nil
	}
	return s, nil
}

// askUntil asks until check accepts the answer.
func (a *app) askUntil(prompt, def string, check func(string) error) (string, error) {
	for i := 0; i < 5; i++ {
		s, err := a.ask(prompt, def)
		if err != nil {
			return "", err
		}
		if err := check(s); err != nil {
			fmt.Fprintf(a.env.Stderr, "  %v\n", err)
			continue
		}
		return s, nil
	}
	return "", usage("rerun with flags, like --memory 4GiB", "too many bad answers to %q", prompt)
}

func (a *app) wizard(f *flags, o *setupOpts, cfg *config.Config) error {
	fmt.Fprintln(a.env.Stderr, "Tentmaker setup. Enter takes the value in brackets.")
	if !f.set("device") {
		s, err := a.askUntil("Device name", cfg.Device, func(s string) error {
			if daemon.SanitizeDevice(s) != s {
				return fmt.Errorf("use lowercase letters, digits and dashes, like %s", daemon.SanitizeDevice(s))
			}
			return nil
		})
		if err != nil {
			return err
		}
		cfg.Device = s
	}
	sizes := []struct {
		flag, prompt string
		dst          *config.Size
	}{
		{"memory", "Memory for jobs while you work (everyday)", &cfg.Budget.Everyday.Memory},
		{"boost-memory", "Memory for jobs when plugged in and idle (boost)", &cfg.Budget.Boost.Memory},
	}
	for _, s := range sizes {
		if f.set(s.flag) {
			continue
		}
		ans, err := a.askUntil(s.prompt, s.dst.String(), func(v string) error {
			if size, err := config.ParseSize(v); err != nil || size <= 0 {
				return errors.New("write it like 8GiB")
			}
			return nil
		})
		if err != nil {
			return err
		}
		*s.dst, _ = config.ParseSize(ans)
	}
	if !f.set("on-battery") {
		ans, err := a.askUntil("On battery: keep the everyday budget, or stop taking jobs (everyday/off)", firstNonEmpty(cfg.Budget.OnBattery, "everyday"), func(v string) error {
			if v != "everyday" && v != "off" {
				return errors.New("answer everyday or off")
			}
			return nil
		})
		if err != nil {
			return err
		}
		cfg.Budget.OnBattery = ans
	}
	if len(o.repos) == 0 {
		def := ""
		if origin := a.originRepo(o.forge, cfg); origin != "" && !hasRepo(*cfg, origin) {
			def = origin
		}
		ans, err := a.askUntil("Repos to serve, comma separated (OWNER/NAME; empty for none)", def, func(v string) error {
			for _, r := range splitList(v) {
				if err := validRepo(r); err != nil {
					return errors.New("write each as OWNER/NAME")
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, r := range splitList(ans) {
			o.repos = append(o.repos, r)
			addRepo(cfg, r, o.forge, o.mode, false)
		}
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hasRepo(cfg config.Config, name string) bool {
	for _, r := range cfg.Repos {
		if strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

// originRepo is OWNER/NAME of the current directory's git origin when it
// is on forge's host; "" otherwise. It reads .git/config and runs nothing.
func (a *app) originRepo(forge string, cfg *config.Config) string {
	if a.env.Getwd == nil {
		return ""
	}
	dir, err := a.env.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 40; i++ {
		b, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
		if err == nil {
			return repoFromOrigin(gitOrigin(string(b)), forgeHost(forge, *cfg))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func forgeHost(forge string, cfg config.Config) string {
	fc := daemon.ForgeConfig(cfg, forge)
	if fc.URL != "" {
		if u, err := url.Parse(fc.URL); err == nil {
			h := strings.TrimPrefix(u.Hostname(), "api.")
			return h
		}
	}
	return map[string]string{"github": "github.com", "gitlab": "gitlab.com"}[forge]
}

// gitOrigin finds [remote "origin"] url in a git config.
func gitOrigin(gitConfig string) string {
	sc := bufio.NewScanner(strings.NewReader(gitConfig))
	in := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = strings.ReplaceAll(line, " ", "") == `[remote"origin"]`
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// repoFromOrigin turns git@host:o/n.git or https://host/o/n(.git) into o/n
// when the host matches.
func repoFromOrigin(origin, host string) string {
	if origin == "" || host == "" {
		return ""
	}
	var h, p string
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		h, p = u.Hostname(), u.Path
	} else if at, rest, ok := strings.Cut(origin, "@"); ok && !strings.Contains(at, "/") {
		h, p, _ = strings.Cut(rest, ":")
	} else {
		return ""
	}
	if !strings.EqualFold(h, host) {
		return ""
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	if validRepo(p) != nil {
		return ""
	}
	return p
}

// ---------------------------------------------------------------- token

func (a *app) setupToken(o setupOpts, interactive bool, cfg *config.Config, v *setupView) error {
	fc := daemon.ForgeConfig(*cfg, o.forge)
	secret := daemon.SecretName(fc.Kind, fc.URL)
	needed := false
	for _, r := range cfg.Repos {
		if daemon.RepoForge(r) == o.forge {
			needed = true
		}
	}
	if !o.tokenStdin {
		if _, err := a.env.Secrets.Get(secret); err == nil {
			v.Secret = secret
			a.step(v, "forge", daemon.OK, "%s token already stored as secret %s; kept", o.forge, secret)
			return nil
		} else if !errors.Is(err, secrets.ErrNotFound) {
			a.step(v, "forge", daemon.Warn, "can't read the stored %s token: %v", o.forge, err)
		}
		if !needed {
			a.step(v, "forge", "skip", "no %s repos on this device; it takes work from brokers in its pool", o.forge)
			return nil
		}
	}
	tok, source, err := a.obtainToken(a.ctx, o.forge, o.mode, o.tokenStdin, o.noGH, interactive)
	if err != nil {
		return err
	}
	if tok == "" {
		a.step(v, "forge", daemon.Warn, "no %s token, so this device is a worker for its pool; connect later: sudo tentmaker connect %s", o.forge, o.forge)
		return nil
	}
	secret, login, err := a.storeToken(a.ctx, fc.Kind, fc.URL, tok)
	if err != nil {
		return err
	}
	if cfg.Forges == nil {
		cfg.Forges = map[string]config.Forge{}
	}
	if _, ok := cfg.Forges[o.forge]; !ok && fc.URL != "" {
		cfg.Forges[o.forge] = fc
	}
	v.Secret, v.Login = secret, login
	a.step(v, "forge", daemon.OK, "%s token from %s works for %s; stored as secret %s", o.forge, source, firstNonEmpty(login, "the account"), secret)
	return nil
}

// ---------------------------------------------------------------- service

func (a *app) serviceSpec() (service.Spec, error) {
	exe, err := a.env.Executable()
	if err != nil {
		return service.Spec{}, fmt.Errorf("find this binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	spec := service.Spec{Binary: exe, Args: []string{"daemon"}, LogPath: a.env.Paths.Log}
	if def := daemon.DefaultPaths(a.env.GOOS, a.env.Getenv).Config; a.env.ConfigPath != def && a.env.GOOS != "windows" {
		spec.Env = map[string]string{"TENTMAKER_CONFIG": a.env.ConfigPath}
	}
	return spec, nil
}

func (a *app) setupService(v *setupView) error {
	spec, err := a.serviceSpec()
	if err != nil {
		return err
	}
	if err := a.env.Service.Install(a.ctx, spec); err != nil {
		return fmt.Errorf("install the service: %v", err)
	}
	v.Service = "installed"
	wait := a.env.ServiceWait
	if wait <= 0 {
		wait = 20 * time.Second
	}
	if err := a.waitControl(wait); err != nil {
		a.step(v, "service", daemon.Warn, "installed, but its control API did not answer within %s; see `tentmaker logs`", wait)
		return nil
	}
	v.Service = "running"
	a.step(v, "service", daemon.OK, "installed and running")
	return nil
}

// waitControl polls the control API until it answers or wait passes.
func (a *app) waitControl(wait time.Duration) error {
	ctx, cancel := context.WithTimeout(a.ctx, wait)
	defer cancel()
	for {
		var st daemon.Status
		cctx, c2 := context.WithTimeout(ctx, 2*time.Second)
		err := a.env.Control.Get(cctx, "/v1/status", &st)
		c2()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.env.Sleep(250 * time.Millisecond)
	}
}
