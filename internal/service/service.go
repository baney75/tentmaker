// Package service installs the Tentmaker daemon as the operating system's
// own service: a systemd unit on Linux, a launchd daemon on macOS and a
// Windows service. Every command goes through an injectable Runner with a
// timeout, and the unit and plist text come from pure functions, so tests
// never touch the host.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
)

// Names of the service on each OS.
const (
	UnitName    = "tentmaker.service"
	UnitPath    = "/etc/systemd/system/tentmaker.service"
	Label       = "dev.tentmaker.daemon"
	PlistPath   = "/Library/LaunchDaemons/dev.tentmaker.daemon.plist"
	MacLogPath  = "/Library/Logs/Tentmaker/tentmaker.log"
	WindowsName = "Tentmaker"
)

// CommandTimeout bounds every systemctl or launchctl call.
const CommandTimeout = 30 * time.Second

// StopTimeout bounds a stop: the daemon itself shuts down within 20 s.
const StopTimeout = 30 * time.Second

// Spec is what the service runs.
type Spec struct {
	Binary string   // absolute path of the tentmaker binary
	Args   []string // default ["daemon"]
	// Env is extra, non-secret environment (TENTMAKER_CONFIG for a config
	// outside the default path). Secrets never go here.
	Env map[string]string
	// LogPath is where launchd sends stdout and stderr (macOS). Default
	// MacLogPath.
	LogPath string
}

func (s Spec) args() []string {
	if len(s.Args) == 0 {
		return []string{"daemon"}
	}
	return s.Args
}

func (s Spec) validate() error {
	if s.Binary == "" || !filepath.IsAbs(s.Binary) {
		return fmt.Errorf("service binary %q must be an absolute path", s.Binary)
	}
	for k := range s.Env {
		if k == "" || strings.ContainsAny(k, "=\n\"") {
			return fmt.Errorf("service environment name %q is not valid", k)
		}
	}
	for _, v := range append(append([]string{s.Binary}, s.args()...), envValues(s.Env)...) {
		if strings.ContainsAny(v, "\n\r\x00") {
			return errors.New("service binary, arguments and environment can't contain line breaks")
		}
	}
	return nil
}

func envValues(m map[string]string) []string {
	var out []string
	for _, k := range sortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// State is the service as the OS sees it.
type State struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Detail    string `json:"detail,omitempty"`
}

// Manager installs and controls the service.
type Manager interface {
	Install(ctx context.Context, s Spec) error // write, enable and start
	Uninstall(ctx context.Context) error       // stop, disable and remove
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Status(ctx context.Context) (State, error)
}

// Runner runs one command and returns its combined output. A non-zero exit
// is an error; the output is still returned.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Options configures a Manager. Zero values pick the host's.
type Options struct {
	GOOS string
	Run  Runner
	// Root prefixes every file path (tests point it at a temp dir).
	Root string
	// SCM is the Windows service manager (tests swap in a fake).
	SCM SCM
	// Timeout bounds each command (default CommandTimeout).
	Timeout time.Duration
}

// New returns the Manager for o.GOOS.
func New(o Options) Manager {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Run == nil {
		o.Run = ExecRunner
	}
	if o.Timeout <= 0 {
		o.Timeout = CommandTimeout
	}
	switch o.GOOS {
	case "linux":
		return &systemd{o: o}
	case "darwin":
		return &launchd{o: o}
	case "windows":
		if o.SCM == nil {
			o.SCM = NewSCM()
		}
		return &winService{o: o}
	default:
		return unsupported{goos: o.GOOS}
	}
}

// Default is the Manager for this host.
func Default() Manager { return New(Options{}) }

// Install installs and starts the service on this host.
func Install(ctx context.Context, s Spec) error { return Default().Install(ctx, s) }

// Uninstall stops and removes the service on this host.
func Uninstall(ctx context.Context) error { return Default().Uninstall(ctx) }

// Start starts the service on this host.
func Start(ctx context.Context) error { return Default().Start(ctx) }

// Stop stops the service on this host.
func Stop(ctx context.Context) error { return Default().Stop(ctx) }

// Status reads the service on this host.
func Status(ctx context.Context) (State, error) { return Default().Status(ctx) }

// systemPath is where bare command names are found; the caller's PATH is
// never used.
var systemPath = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin"}

// ExecRunner runs a real command, found in a fixed system path, with the
// context's deadline.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	path := name
	if !filepath.IsAbs(name) {
		path = ""
		for _, d := range systemPath {
			p := filepath.Join(d, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				path = p
				break
			}
		}
		if path == "" {
			return nil, fmt.Errorf("%s not found in %s", name, strings.Join(systemPath, ":"))
		}
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=" + strings.Join(systemPath, ":"), "LANG=C"}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tail(out.String(), 512))
	}
	return out.Bytes(), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}

func (o Options) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	return o.Run(ctx, name, args...)
}

func (o Options) path(p string) string {
	if o.Root == "" {
		return p
	}
	return filepath.Join(o.Root, filepath.FromSlash(p))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writeFile writes a root-owned service file atomically.
func writeFile(path string, data []byte) error {
	return config.WriteAtomic(path, data, 0o644)
}

type unsupported struct{ goos string }

func (u unsupported) err() error {
	return fmt.Errorf("Tentmaker has no service manager for %s; run `tentmaker daemon` yourself", u.goos)
}
func (u unsupported) Install(context.Context, Spec) error { return u.err() }
func (u unsupported) Uninstall(context.Context) error     { return u.err() }
func (u unsupported) Start(context.Context) error         { return u.err() }
func (u unsupported) Stop(context.Context) error          { return u.err() }
func (u unsupported) Restart(context.Context) error       { return u.err() }
func (u unsupported) Status(context.Context) (State, error) {
	return State{}, u.err()
}
