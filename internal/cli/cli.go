// Package cli is the tentmaker command line (docs/CLI.md is its contract).
// Commands stay thin: they parse flags, call the packages that do the work
// and print. Every host effect goes through Env, so tests run anywhere.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/baney75/tentmaker/internal/binsafe"
	"github.com/baney75/tentmaker/internal/checks"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/daemon"
	"github.com/baney75/tentmaker/internal/executor"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/power"
	"github.com/baney75/tentmaker/internal/secrets"
	"github.com/baney75/tentmaker/internal/service"
	"github.com/baney75/tentmaker/internal/update"
)

// Version is set at build time with -ldflags "-X github.com/baney75/tentmaker/internal/cli.Version=...".
var Version = "0.0.0-dev"

// Exit codes from docs/CLI.md.
const (
	ExitOK       = 0
	ExitFailed   = 1
	ExitUsage    = 2
	ExitRoot     = 3
	ExitNotSetUp = 4
)

// Updater is what update and rollback need from internal/update.
type Updater interface {
	daemon.Updater
	Rollback(curBin string) error
}

// Env is every effect the command line has on the host. DefaultEnv fills
// the real ones; tests swap in fakes.
type Env struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	StdinTTY  bool
	StdoutTTY bool
	// ReadSecret reads one line from the terminal without echo.
	ReadSecret func() ([]byte, error)
	Getenv     func(string) string
	GOOS       string
	GOARCH     string
	Version    string

	IsRoot     func() bool
	ConfigPath string
	Paths      daemon.Paths
	Secrets    secrets.Store
	Service    service.Manager
	// ServiceWait bounds the wait for the control API after the service
	// starts (default 20 s).
	ServiceWait time.Duration

	Executors          daemon.ExecutorFactory
	InstallExecutors   func(ctx context.Context, cfg config.Config, binary func(core.Platform) (string, error), o InstallOptions) error
	UninstallExecutors func(ctx context.Context, cfg config.Config, purge bool) error

	// GHToken runs `gh auth token`, as user when it is not empty (sudo).
	GHToken     func(ctx context.Context, user string) (string, error)
	NewForge    daemon.ForgeFactory
	VerifyToken func(ctx context.Context, kind, baseURL, token string) (string, error)

	Control       *control.Client
	Updater       Updater
	Tailscale     mesh.Tailscale
	RemoteInstall func(ctx context.Context, host string, args mesh.InstallArgs) error

	Hostname    func() (string, error)
	Getwd       func() (string, error)
	TotalMemory func() (int64, error)
	CPUs        func() int
	Power       core.PowerSource
	Idle        core.IdleSource
	Probe       func(stateDir string) guards.Probe
	Now         func() time.Time
	Executable  func() (string, error)
	// BinaryProblem says why someone other than root (Windows: SYSTEM and
	// Administrators) could change the binary at path or a folder above it,
	// or "" when no one can. Default binsafe.Problem.
	BinaryProblem func(path string) (string, error)
	// ServiceBinDirs are root-owned folders setup may copy the binary into
	// when the service can't run it in place, in order of preference.
	// Default binsafe.ServiceDirs.
	ServiceBinDirs []string
	// Command runs a host tool (journalctl, git) with output to out.
	Command   func(ctx context.Context, out io.Writer, name string, args ...string) error
	RunBundle func(ctx context.Context, b core.CheckBundle, outDir string, log io.Writer) core.CheckResult
	RunDaemon func(ctx context.Context, o daemon.Options) error
	// Context is the base context; DefaultEnv cancels it on Ctrl-C.
	Context func() (context.Context, context.CancelFunc)
	Sleep   func(time.Duration)
}

// DefaultEnv is the real host.
func DefaultEnv() *Env {
	paths := daemon.DefaultPaths(runtime.GOOS, os.Getenv)
	paths.Config = config.Path()
	cpath := paths.Config
	return &Env{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		StdinTTY:    isTerminal(os.Stdin),
		StdoutTTY:   isTerminal(os.Stdout),
		ReadSecret:  func() ([]byte, error) { return readSecret(os.Stdin) },
		Getenv:      os.Getenv,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Version:     Version,
		IsRoot:      isRoot,
		ConfigPath:  cpath,
		Paths:       paths,
		Secrets:     secrets.Dir(paths.Secrets),
		Service:     service.Default(),
		ServiceWait: 20 * time.Second,
		Executors:   executor.ForHost,
		InstallExecutors: func(ctx context.Context, cfg config.Config, binary func(core.Platform) (string, error), o InstallOptions) error {
			return installExecutors(ctx, runtime.GOOS, cfg, binary, o)
		},
		UninstallExecutors: func(ctx context.Context, cfg config.Config, purge bool) error {
			return uninstallExecutors(ctx, runtime.GOOS, cfg, purge)
		},
		GHToken:  ghToken,
		NewForge: daemon.DefaultForge,
		VerifyToken: func(ctx context.Context, kind, base, tok string) (string, error) {
			return verifyToken(ctx, nil, kind, base, tok)
		},
		Control:        &control.Client{Addr: control.DefaultAddr},
		Updater:        update.FromEnv(os.Getenv),
		Tailscale:      mesh.NewTailscaleCLI(),
		RemoteInstall:  mesh.RemoteInstall,
		Hostname:       os.Hostname,
		Getwd:          os.Getwd,
		TotalMemory:    power.TotalMemory,
		CPUs:           power.CPUs,
		Power:          power.New(),
		Idle:           power.NewIdle(),
		Probe:          guards.NewHostProbe,
		Now:            time.Now,
		Executable:     os.Executable,
		BinaryProblem:  binsafe.Problem,
		ServiceBinDirs: binsafe.ServiceDirs(runtime.GOOS, os.Getenv),
		Command:        runCommand,
		RunBundle:      checks.RunBundle,
		RunDaemon: func(ctx context.Context, o daemon.Options) error {
			return daemon.New(o).Run(ctx)
		},
		Context: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		},
		Sleep: time.Sleep,
	}
}

// Main runs the command line on the real host and returns the exit code.
func Main(args []string) int { return Run(DefaultEnv(), args) }

// cliError is a failure with an exit code and a one-line fix.
type cliError struct {
	code int
	msg  string
	fix  string
}

func (e *cliError) Error() string { return e.msg }

func fail(fix, format string, a ...any) error {
	return &cliError{code: ExitFailed, msg: fmt.Sprintf(format, a...), fix: fix}
}

func usage(fix, format string, a ...any) error {
	return &cliError{code: ExitUsage, msg: fmt.Sprintf(format, a...), fix: fix}
}

// app is one run of the command line.
type app struct {
	env  *Env
	args []string // the whole command line, for hints
	json bool
	ctx  context.Context
	in   *bufio.Reader
}

// out prints human output; nothing in --json mode.
func (a *app) out(format string, v ...any) {
	if a.json {
		return
	}
	fmt.Fprintf(a.env.Stdout, format, v...)
}

func (a *app) color() bool {
	return a.env.StdoutTTY && a.env.Getenv("NO_COLOR") == ""
}

func (a *app) paint(status, text string) string {
	if !a.color() {
		return text
	}
	code := map[string]string{daemon.OK: "32", daemon.Warn: "33", daemon.Fail: "31", "bold": "1"}[status]
	if code == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

type command struct {
	usage string
	run   func(a *app, args []string) (any, error)
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"setup":     {usageSetup, (*app).cmdSetup},
		"status":    {"tentmaker status [--json] [--watch]", (*app).cmdStatus},
		"pool":      {"tentmaker pool [--json]", (*app).cmdPool},
		"budget":    {usageBudget, (*app).cmdBudget},
		"schedule":  {usageSchedule, (*app).cmdSchedule},
		"repo":      {usageRepo, (*app).cmdRepo},
		"connect":   {"tentmaker connect github|forgejo|gitlab [--url URL] [--token-stdin] [--json]", (*app).cmdConnect},
		"devices":   {"tentmaker devices add HOST [--channel stable|dev] [--memory S] [--boost-memory S] [--repo R]... [--json]", (*app).cmdDevices},
		"migrate":   {"tentmaker migrate [PATH] [--platform linux|macos|windows] [--write] [--json]", (*app).cmdMigrate},
		"pause":     {"tentmaker pause [--for DURATION] [REASON...] [--json]", (*app).cmdPause},
		"resume":    {"tentmaker resume [--json]", (*app).cmdResume},
		"doctor":    {"tentmaker doctor [--json]", (*app).cmdDoctor},
		"logs":      {"tentmaker logs [-f] [-n LINES] [--json]", (*app).cmdLogs},
		"update":    {"tentmaker update [--channel stable|dev] [--check] [--json]", (*app).cmdUpdate},
		"rollback":  {"tentmaker rollback [--json]", (*app).cmdRollback},
		"uninstall": {"tentmaker uninstall [--purge] [--yes] [--json]", (*app).cmdUninstall},
		"mcp":       {"tentmaker mcp\n\nStdio MCP server for agents. Register it with: claude mcp add tentmaker -- tentmaker mcp", (*app).cmdMCP},
		"version":   {"tentmaker version [--json]", (*app).cmdVersion},
		"daemon":    {"tentmaker daemon\n\nRun by the service. Not for people.", (*app).cmdDaemon},
		"check-run": {"tentmaker check-run --out DIR\n\nRuns inside a sandbox: CheckBundle JSON on stdin. Not for people.", (*app).cmdCheckRun},
	}
}

const usageMain = `tentmaker: your machines have a day job. Tentmaker gives them a second one.

Usage:
  tentmaker                       status if set up; otherwise how to set up
  tentmaker setup [flags]         install and configure this device
  tentmaker status [--watch]      this device: budget, jobs, health
  tentmaker pool                  every device in the pool
  tentmaker budget                the budget and the current decision
  tentmaker budget set [flags]    change the budget
  tentmaker schedule list|add|remove
  tentmaker repo list|add|remove  repositories this device serves
  tentmaker connect github|forgejo|gitlab
  tentmaker devices add HOST      install on another tailnet device
  tentmaker migrate [PATH]        point workflows at Tentmaker
  tentmaker pause [--for D] [REASON...]
  tentmaker resume
  tentmaker doctor                find problems, each with its fix
  tentmaker logs [-f] [-n LINES]
  tentmaker update [--check]      update to the newest release
  tentmaker rollback              go back to the previous version
  tentmaker uninstall [--purge]
  tentmaker mcp                   MCP server for agents
  tentmaker version

Every command takes --json (one JSON object on stdout) and --help.
Exit codes: 0 ok, 1 failed, 2 usage error, 3 needs root/Administrator, 4 not set up yet.
`

// Run runs args with env and returns the exit code.
func Run(env *Env, args []string) int {
	a := &app{env: env, args: args, json: wantsJSON(args)}
	if env.Context != nil {
		ctx, cancel := env.Context()
		defer cancel()
		a.ctx = ctx
	} else {
		a.ctx = context.Background()
	}
	a.in = bufio.NewReader(env.Stdin)
	res, err := a.dispatch(args)
	if err != nil {
		return a.fail(err)
	}
	if _, quiet := res.(silentExit); a.json && res != nil && !quiet {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(env.Stderr, "tentmaker:", err)
			return ExitFailed
		}
	}
	if c, ok := res.(exitCoder); ok {
		return c.exitCode()
	}
	return ExitOK
}

// exitCoder lets a successful result still exit non-zero (doctor with
// problems), after printing.
type exitCoder interface{ exitCode() int }

func (a *app) fail(err error) int {
	var ce *cliError
	if !errors.As(err, &ce) {
		ce = &cliError{code: ExitFailed, msg: err.Error()}
	}
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if a.json {
		b, _ := json.Marshal(map[string]string{"error": ce.msg, "fix": ce.fix})
		fmt.Fprintln(a.env.Stdout, string(b))
	} else {
		fmt.Fprintln(a.env.Stderr, "tentmaker: "+ce.msg)
		if ce.fix != "" {
			fmt.Fprintln(a.env.Stderr, "fix: "+ce.fix)
		}
	}
	return ce.code
}

// wantsJSON finds --json anywhere before a "--".
func wantsJSON(args []string) bool {
	for _, s := range args {
		if s == "--" {
			return false
		}
		if s == "--json" || s == "-json" || s == "--json=true" || s == "-json=true" {
			return true
		}
	}
	return false
}

func (a *app) dispatch(args []string) (any, error) {
	if len(args) == 0 || (len(args) == 1 && wantsJSON(args)) {
		return a.cmdBare()
	}
	name := args[0]
	switch name {
	case "-h", "--help", "help":
		if len(args) > 1 {
			if c, ok := commands[args[1]]; ok {
				fmt.Fprintln(a.env.Stdout, c.usage)
				return nil, nil
			}
		}
		fmt.Fprint(a.env.Stdout, usageMain)
		return nil, nil
	case "--version", "-v":
		return a.cmdVersion(args[1:])
	}
	c, ok := commands[name]
	if !ok {
		return nil, usage("run `tentmaker --help` for the commands", "unknown command %q", name)
	}
	return c.run(a, args[1:])
}

// flags is a FlagSet that never prints and knows its command's usage.
type flags struct {
	*flag.FlagSet
	name  string
	usage string
	json  *bool
}

func (a *app) flags(name, usageText string) *flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := &flags{FlagSet: fs, name: name, usage: usageText}
	f.json = fs.Bool("json", false, "print one JSON object")
	return f
}

// parse reads flags and positional arguments in any order.
func (a *app) parse(f *flags, args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(a.env.Stdout, f.usage)
				return nil, flag.ErrHelp
			}
			return nil, usage("usage: "+firstLine(f.usage), "%s: %v", f.name, err)
		}
		rest := f.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (f *flags) set(name string) bool {
	found := false
	f.Visit(func(fl *flag.Flag) {
		if fl.Name == name {
			found = true
		}
	})
	return found
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// ---------------------------------------------------------------- guards

// needRoot fails with exit 3 and the exact command to rerun.
func (a *app) needRoot() error {
	if a.env.IsRoot() {
		return nil
	}
	cmd := "tentmaker " + shellJoin(a.args)
	if a.env.GOOS == "windows" {
		return &cliError{code: ExitRoot, msg: "this command changes the device and needs Administrator",
			fix: "open PowerShell as Administrator and run: " + cmd}
	}
	return &cliError{code: ExitRoot, msg: "this command changes the device and needs root", fix: "sudo " + cmd}
}

func (a *app) notSetUp() error {
	fix := "sudo tentmaker setup"
	if a.env.GOOS == "windows" {
		fix = "open PowerShell as Administrator and run: tentmaker setup"
	}
	return &cliError{code: ExitNotSetUp, msg: "Tentmaker is not set up on this device yet", fix: fix}
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, s := range args {
		if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?![]{}#~") {
			out[i] = s
			continue
		}
		out[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// ---------------------------------------------------------------- config

func (a *app) totalMemory() int64 {
	if m, err := a.env.TotalMemory(); err == nil && m > 0 {
		return m
	}
	return 8 << 30
}

func (a *app) configExists() bool {
	_, err := os.Stat(a.env.ConfigPath)
	return err == nil
}

// loadConfig reads the config (defaults when missing) and reports whether
// the file exists.
func (a *app) loadConfig() (config.Config, bool, error) {
	exists := a.configExists()
	cfg, err := config.Load(a.env.ConfigPath, a.totalMemory(), a.env.CPUs())
	if err != nil {
		return cfg, exists, fail("fix "+a.env.ConfigPath+" or rerun `sudo tentmaker setup`", "%v", err)
	}
	return cfg, exists, nil
}

// mustConfig is loadConfig for commands that need setup.
func (a *app) mustConfig() (config.Config, error) {
	cfg, exists, err := a.loadConfig()
	if err != nil {
		return cfg, err
	}
	if !exists {
		return cfg, a.notSetUp()
	}
	return cfg, nil
}

// resolved fills directories and the device name, as the daemon does.
func (a *app) resolved(cfg config.Config) config.Config {
	cfg = daemon.Resolve(cfg, a.env.Paths)
	cfg.Device = daemon.DeviceName(cfg, a.env.Hostname)
	return cfg
}

// saveConfig validates and writes the config.
func (a *app) saveConfig(cfg config.Config) error {
	if ps := cfg.Validate(); len(ps) > 0 {
		return fail("change the value and run the command again", "%s", strings.Join(ps, "; "))
	}
	if err := config.Save(a.env.ConfigPath, cfg); err != nil {
		return fail("check that "+filepath.Dir(a.env.ConfigPath)+" is writable", "save config: %v", err)
	}
	return nil
}

// reload asks the daemon to re-read the config. It returns a note for the
// user, never an error: the change is saved either way.
func (a *app) reload() string {
	cfg, _, _ := a.loadConfig()
	err := a.controlClient(cfg).Post(a.ctx, "/v1/reload", nil)
	switch {
	case err == nil:
		return "applied"
	case errors.Is(err, control.ErrNotRunning):
		return "saved; the service is not running, so it applies when it starts"
	default:
		return "saved, but the service did not reload: " + err.Error()
	}
}

// controlClient points the client's token at the state dir.
func (a *app) controlClient(cfg config.Config) *control.Client {
	c := *a.env.Control
	if c.TokenPath == "" {
		c.TokenPath = control.TokenPath(a.resolved(cfg).StateDir)
	}
	return &c
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// runCommand runs a host tool with output streamed to out.
func runCommand(ctx context.Context, out io.Writer, name string, args ...string) error {
	path, err := lookTool(name)
	if err != nil {
		return err
	}
	cmd := execCommand(ctx, path, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = 2 * time.Second
	return cmd.Run()
}

func newDaemonLogger(path string, w io.Writer) (*slog.Logger, io.Closer, error) {
	return daemon.NewLogger(path, w)
}

// httpClient is the client for one-off forge calls.
func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }
