package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/baney75/tentmaker/internal/dispatch"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/daemon"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/power"
	"github.com/baney75/tentmaker/internal/secrets"
	"github.com/baney75/tentmaker/internal/service"
	"github.com/baney75/tentmaker/internal/update"
)

// canary is the stand-in forge credential tests pipe in. It must never
// show up in any output.
var canary = strings.Repeat("Q", 6) + "-canary-value-" + strings.Repeat("7", 12)

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStore) Get(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[name]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *memStore) Set(name string, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = append([]byte(nil), v...)
	return nil
}

func (s *memStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return nil
}

type fakeService struct {
	mu        sync.Mutex
	spec      *service.Spec
	installed bool
	running   bool
	calls     []string
}

func (s *fakeService) record(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *fakeService) Install(ctx context.Context, spec service.Spec) error {
	s.record("install")
	s.mu.Lock()
	s.spec, s.installed, s.running = &spec, true, true
	s.mu.Unlock()
	return nil
}

func (s *fakeService) Uninstall(ctx context.Context) error {
	s.record("uninstall")
	s.mu.Lock()
	s.installed, s.running = false, false
	s.mu.Unlock()
	return nil
}

func (s *fakeService) Start(ctx context.Context) error   { s.record("start"); return nil }
func (s *fakeService) Stop(ctx context.Context) error    { s.record("stop"); return nil }
func (s *fakeService) Restart(ctx context.Context) error { s.record("restart"); return nil }
func (s *fakeService) Status(ctx context.Context) (service.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return service.State{Installed: s.installed, Running: s.running}, nil
}

type fakeTS struct{ up bool }

func (f fakeTS) Self(ctx context.Context) (mesh.Peer, error) {
	if !f.up {
		return mesh.Peer{}, errors.New("tailscale: not running")
	}
	return mesh.Peer{HostName: "box", DNSName: "box.tail.ts.net", Online: true}, nil
}
func (f fakeTS) Peers(ctx context.Context) ([]mesh.Peer, error) { return nil, nil }
func (f fakeTS) WhoIs(ctx context.Context, addr string) (mesh.Identity, error) {
	return mesh.Identity{}, errors.New("unknown")
}

type fakeUpdater struct {
	mu         sync.Mutex
	onApply    func()
	newer      bool
	applied    []string
	appliedTo  []string
	rolledBack int
	rollbackTo []string
}

func (u *fakeUpdater) Check(ctx context.Context, channel, current string) (update.Release, bool, error) {
	return update.Release{Channel: channel, Tag: "v9.9.9", Version: "9.9.9"}, u.newer, nil
}

func (u *fakeUpdater) Download(ctx context.Context, rel update.Release, dir string) (string, error) {
	p := filepath.Join(dir, "tentmaker-new")
	return p, os.WriteFile(p, []byte("new"), 0o755)
}

func (u *fakeUpdater) Apply(ctx context.Context, newBin, curBin, want string) error {
	if u.onApply != nil {
		u.onApply()
	}
	u.mu.Lock()
	u.applied = append(u.applied, want)
	u.appliedTo = append(u.appliedTo, curBin)
	u.mu.Unlock()
	return nil
}

func (u *fakeUpdater) Rollback(curBin string) error {
	u.mu.Lock()
	u.rolledBack++
	u.rollbackTo = append(u.rollbackTo, curBin)
	u.mu.Unlock()
	return nil
}

// fakeDaemon answers the control API like a running service.
type fakeDaemon struct {
	mu       sync.Mutex
	version  string
	reloads  int
	paused   []string
	resumes  int
	isPaused bool
	running  int // jobs the worker reports
}

func (d *fakeDaemon) Status(ctx context.Context) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := daemon.Status{Version: d.version, Device: "box", Why: "everyday: you are working", Paused: d.isPaused}
	if d.running > 0 {
		st.Worker = &dispatch.WorkerStatus{Paused: d.isPaused}
		for i := 0; i < d.running; i++ {
			st.Worker.Running = append(st.Worker.Running, dispatch.RunningJob{Name: fmt.Sprintf("job-%d", i)})
		}
	}
	return st, nil
}
func (d *fakeDaemon) Pool(ctx context.Context) (any, error) { return []mesh.DeviceStatus{}, nil }
func (d *fakeDaemon) Doctor(ctx context.Context) (any, error) {
	return daemon.Report{OK: true, Version: "1.0.0", Checks: []daemon.Finding{{Check: "config", Status: daemon.OK, What: "fine"}}}, nil
}
func (d *fakeDaemon) Pause(ctx context.Context, reason string, dur time.Duration) error {
	d.mu.Lock()
	d.paused = append(d.paused, reason)
	d.isPaused = true
	d.mu.Unlock()
	return nil
}
func (d *fakeDaemon) Resume(ctx context.Context) error {
	d.mu.Lock()
	d.resumes++
	d.isPaused = false
	d.mu.Unlock()
	return nil
}
func (d *fakeDaemon) Reload(ctx context.Context) error {
	d.mu.Lock()
	d.reloads++
	d.mu.Unlock()
	return nil
}

// fixture is one fake host.
type fixture struct {
	t           *testing.T
	root        string
	env         *Env
	store       *memStore
	svc         *fakeService
	forge       *fakeforge.Forge
	ex          *fakeexec.Executor
	up          *fakeUpdater
	stdout      *bytes.Buffer
	stderr      *bytes.Buffer
	isRoot      bool
	verified    []string // credentials VerifyToken saw
	ghValue     string
	remote      []mesh.InstallArgs
	execInst    int
	installOpts InstallOptions
	execUnin    int
	daemon      *fakeDaemon
	// userWritable maps a path prefix to why a user could change files
	// under it; BinaryProblem reports it for any path under the prefix.
	userWritable map[string]string
	mu           sync.Mutex
}

// deadAddr is a loopback port with nothing listening.
func deadAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tentmaker")
	paths := daemon.Paths{
		Config:  filepath.Join(root, "etc", "config.toml"),
		Secrets: filepath.Join(root, "etc", "secrets"),
		State:   filepath.Join(root, "state"),
		Cache:   filepath.Join(root, "cache"),
		Log:     filepath.Join(root, "logs", "tentmaker.log"),
	}
	f := &fixture{
		t: t, root: root,
		store:        &memStore{m: map[string][]byte{}},
		svc:          &fakeService{},
		forge:        fakeforge.New("github", nil),
		ex:           fakeexec.New("fake-linux", 0, core.Platform{OS: "linux", Arch: "x64"}),
		up:           &fakeUpdater{},
		stdout:       &bytes.Buffer{},
		stderr:       &bytes.Buffer{},
		isRoot:       true,
		daemon:       &fakeDaemon{version: "1.0.0"},
		userWritable: map[string]string{},
	}
	exe := filepath.Join(root, "bin", "tentmaker")
	f.env = &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     f.stdout,
		Stderr:     f.stderr,
		ReadSecret: func() ([]byte, error) { return nil, errors.New("no terminal") },
		Getenv:     func(k string) string { return map[string]string{"SUDO_USER": "alice"}[k] },
		GOOS:       "linux",
		GOARCH:     "amd64",
		Version:    "1.0.0",
		IsRoot:     func() bool { return f.isRoot },
		ConfigPath: paths.Config,
		Paths:      paths,
		Secrets:    f.store,
		Service:    f.svc,
		Executors: func(cfg config.Config, bin func(core.Platform) (string, error), log *slog.Logger) ([]core.Executor, []core.Problem) {
			return []core.Executor{f.ex}, nil
		},
		InstallExecutors: func(ctx context.Context, cfg config.Config, bin func(core.Platform) (string, error), o InstallOptions) error {
			f.mu.Lock()
			f.execInst++
			f.installOpts = o
			f.mu.Unlock()
			return nil
		},
		UninstallExecutors: func(ctx context.Context, cfg config.Config, purge bool) error {
			f.mu.Lock()
			f.execUnin++
			f.mu.Unlock()
			return nil
		},
		GHToken: func(ctx context.Context, user string) (string, error) {
			if f.ghValue == "" {
				return "", errors.New("gh not signed in")
			}
			return f.ghValue, nil
		},
		NewForge: func(name string, fc config.Forge, tok func(context.Context) (string, error), log *slog.Logger) (core.Forge, core.StatusForge, error) {
			return f.forge, f.forge, nil
		},
		VerifyToken: func(ctx context.Context, kind, base, value string) (string, error) {
			f.mu.Lock()
			f.verified = append(f.verified, value)
			f.mu.Unlock()
			return "octo", nil
		},
		Control:   &control.Client{Addr: deadAddr(t)},
		Updater:   f.up,
		Tailscale: fakeTS{},
		RemoteInstall: func(ctx context.Context, host string, args mesh.InstallArgs) error {
			f.mu.Lock()
			f.remote = append(f.remote, args)
			f.mu.Unlock()
			return nil
		},
		Hostname:    func() (string, error) { return "Test-Box.local", nil },
		Getwd:       func() (string, error) { return root, nil },
		TotalMemory: func() (int64, error) { return 16 << 30, nil },
		CPUs:        func() int { return 8 },
		Power:       power.Fake{State: core.PowerState{Known: true, OnAC: true}},
		Idle:        power.FakeIdle{Err: errors.New("unknown")},
		Probe: func(string) guards.Probe {
			return guards.Fake{Memory: 64 << 30, Disk: 500 << 30, Load: 0.1}
		},
		Now:        time.Now,
		Executable: func() (string, error) { return exe, nil },
		BinaryProblem: func(p string) (string, error) {
			for pre, why := range f.userWritable {
				if p == pre || strings.HasPrefix(p, pre+string(filepath.Separator)) {
					return why, nil
				}
			}
			return "", nil
		},
		ServiceBinDirs: []string{filepath.Join(root, "sys", "bin"), filepath.Join(root, "sys", "libexec", "tentmaker")},
		Command: func(ctx context.Context, out io.Writer, name string, args ...string) error {
			io.WriteString(out, "line one\nline two\n")
			return nil
		},
		RunBundle: func(ctx context.Context, b core.CheckBundle, outDir string, log io.Writer) core.CheckResult {
			return core.CheckResult{Repo: b.Repo, SHA: b.SHA, State: "success", Description: "ok"}
		},
		RunDaemon:   func(ctx context.Context, o daemon.Options) error { return nil },
		ServiceWait: 2 * time.Second,
		Sleep:       func(time.Duration) { time.Sleep(time.Millisecond) },
	}
	return f
}

// serveControl starts a real control server over fakeDaemon and points
// the CLI at it, with the control token in the state dir.
func (f *fixture) serveControl() {
	f.t.Helper()
	if err := os.MkdirAll(f.env.Paths.State, 0o755); err != nil {
		f.t.Fatal(err)
	}
	tok, err := control.NewToken(control.TokenPath(f.env.Paths.State))
	if err != nil {
		f.t.Fatal(err)
	}
	// Callers are named as the real server names them: an admin when the
	// fixture runs as root, an ordinary user otherwise.
	ctl := control.NewServer(f.daemon, tok, func(net.Conn) (control.Caller, error) {
		return control.Caller{Admin: f.isRoot, Who: "test user"}, nil
	}, nil)
	srv := httptest.NewUnstartedServer(ctl)
	srv.Config.ConnContext = ctl.ConnContext
	srv.Start()
	f.t.Cleanup(srv.Close)
	f.env.Control.Addr = strings.TrimPrefix(srv.URL, "http://")
}

// run runs args with stdin and returns the exit code. It fails the test if
// the canary credential shows up in any output.
func (f *fixture) run(stdin string, args ...string) int {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	f.env.Stdin = strings.NewReader(stdin)
	code := Run(f.env, args)
	for _, s := range []string{f.stdout.String(), f.stderr.String()} {
		if strings.Contains(s, canary) {
			f.t.Fatalf("the credential was printed by %v:\n%s", args, s)
		}
	}
	return code
}

// jsonObject requires stdout to be exactly one JSON object and returns it.
func (f *fixture) jsonObject(args []string) map[string]any {
	f.t.Helper()
	dec := json.NewDecoder(bytes.NewReader(f.stdout.Bytes()))
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		f.t.Fatalf("%v: stdout is not one JSON object: %v\n%s", args, err, f.stdout.String())
	}
	if _, err := dec.Token(); err != io.EOF {
		f.t.Fatalf("%v: more than one JSON value on stdout:\n%s", args, f.stdout.String())
	}
	return m
}

func (f *fixture) config() config.Config {
	f.t.Helper()
	cfg, err := config.Load(f.env.ConfigPath, 16<<30, 8)
	if err != nil {
		f.t.Fatal(err)
	}
	return cfg
}

// setUp runs a plain setup so commands that need a config have one.
func (f *fixture) setUp(extra ...string) {
	f.t.Helper()
	args := append([]string{"setup", "--yes", "--no-service", "--no-mesh"}, extra...)
	if code := f.run("", args...); code != ExitOK {
		f.t.Fatalf("setup exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
}
