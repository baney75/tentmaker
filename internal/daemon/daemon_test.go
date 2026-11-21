package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/power"
	"github.com/baney75/tentmaker/internal/secrets"
	"github.com/baney75/tentmaker/internal/update"
)

// memStore is an in-memory secrets.Store.
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

// fakeTS is a Tailscale that is down unless self is set.
type fakeTS struct {
	mu   sync.Mutex
	self *mesh.Peer
}

func (f *fakeTS) Self(ctx context.Context) (mesh.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.self == nil {
		return mesh.Peer{}, errors.New("tailscale: not running")
	}
	return *f.self, nil
}
func (f *fakeTS) Peers(ctx context.Context) ([]mesh.Peer, error) {
	if _, err := f.Self(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}
func (f *fakeTS) WhoIs(ctx context.Context, addr string) (mesh.Identity, error) {
	return mesh.Identity{}, errors.New("unknown peer")
}

type fakeUpdater struct {
	newer      atomic.Bool
	applied    atomic.Int32
	rolledBack atomic.Value // string: the binary rolled back
}

func (f *fakeUpdater) Rollback(curBin string) error {
	f.rolledBack.Store(curBin)
	return nil
}

func (f *fakeUpdater) Check(ctx context.Context, channel, current string) (update.Release, bool, error) {
	return update.Release{Version: "9.9.9", Channel: channel}, f.newer.Load(), nil
}
func (f *fakeUpdater) Download(ctx context.Context, rel update.Release, dir string) (string, error) {
	p := filepath.Join(dir, "tentmaker")
	return p, os.WriteFile(p, []byte("new"), 0o755)
}
func (f *fakeUpdater) Apply(ctx context.Context, newBin, curBin, want string) error {
	f.applied.Add(1)
	return nil
}

var linuxX64 = core.Platform{OS: "linux", Arch: "x64"}

type rig struct {
	t      *testing.T
	dir    string
	paths  Paths
	cfg    config.Config
	forge  *fakeforge.Forge
	ex     *fakeexec.Executor
	ts     *fakeTS
	store  *memStore
	up     *fakeUpdater
	d      *Daemon
	cancel context.CancelFunc
	done   chan error
	hup    bool
	// executors replaces the factory that always returns ex.
	executors ExecutorFactory
}

func newRig(t *testing.T, edit func(*config.Config)) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{
		t:   t,
		dir: dir,
		paths: Paths{
			Config: filepath.Join(dir, "config.toml"), Secrets: filepath.Join(dir, "secrets"),
			State: filepath.Join(dir, "state"), Cache: filepath.Join(dir, "cache"),
		},
		forge: fakeforge.New("github", nil),
		ex:    fakeexec.New("fake-linux", 0, linuxX64),
		ts:    &fakeTS{},
		store: &memStore{m: map[string][]byte{"github-token": []byte("ghp_secret_test_token")}},
		up:    &fakeUpdater{},
	}
	cfg := config.Default(16<<30, 8)
	cfg.Device = "box"
	cfg.Repos = []config.Repo{{Name: "baney75/app"}}
	cfg.Budget.Everyday = config.Profile{Memory: 4 * config.GiB, CPUs: 4}
	cfg.Budget.Boost = config.Profile{Memory: 8 * config.GiB, CPUs: 8}
	cfg.Update.Auto = false
	if edit != nil {
		edit(&cfg)
	}
	r.cfg = cfg
	r.save()
	return r
}

func (r *rig) save() {
	if err := config.Save(r.paths.Config, r.cfg); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) launch() {
	r.t.Helper()
	r.d = New(Options{
		ConfigPath: r.paths.Config,
		Paths:      r.paths,
		Version:    "1.2.3",
		Log:        slog.New(slog.NewTextHandler(testWriter{r.t}, nil)),
		Power:      power.Fake{State: core.PowerState{Known: true, OnAC: true}},
		Idle:       power.FakeIdle{Err: errors.New("not idle")},
		Probe: func(string) guards.Probe {
			return guards.Fake{Memory: 64 << 30, Disk: 500 << 30, Load: 0.1}
		},
		Executors: func(cfg config.Config, bin func(core.Platform) (string, error), log *slog.Logger) ([]core.Executor, []core.Problem) {
			if r.executors != nil {
				return r.executors(cfg, bin, log)
			}
			return []core.Executor{r.ex}, nil
		},
		NewForge: func(name string, f config.Forge, token func(context.Context) (string, error), log *slog.Logger) (core.Forge, core.StatusForge, error) {
			return r.forge, r.forge, nil
		},
		Secrets:     r.store,
		Tailscale:   r.ts,
		ControlAddr: "127.0.0.1:0",
		RelayAddr:   "127.0.0.1:0",
		Updater:     r.up,
		Hostname:    func() (string, error) { return "box", nil },
		Executable:  func() (string, error) { return "/usr/local/bin/tentmaker", nil },
		UpdateEvery: 50 * time.Millisecond,
		ReloadOnHUP: r.hup,
		ArchiveHTTP: &http.Client{Transport: forgeArchives{r.forge}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan error, 1)
	go func() { r.done <- r.d.Run(ctx) }()
}

func (r *rig) start() {
	r.t.Helper()
	r.launch()
	r.eventually("control API up", func() bool { return r.d.ControlAddress() != "" })
}

func (r *rig) client() *control.Client {
	return &control.Client{Addr: r.d.ControlAddress(), TokenPath: control.TokenPath(r.paths.State)}
}

func (r *rig) stop() error {
	r.cancel()
	select {
	case err := <-r.done:
		return err
	case <-time.After(ShutdownTimeout + 5*time.Second):
		r.t.Fatal("daemon did not stop within the shutdown timeout")
		return nil
	}
}

func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "ghp_secret_test_token") {
		w.t.Errorf("token in log: %s", p)
	}
	return len(p), nil
}

func TestDaemonEndToEnd(t *testing.T) {
	r := newRig(t, nil)
	r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "build", Labels: []string{"self-hosted", "tentmaker", "linux"}})
	r.start()

	// A queued job becomes a started runner.
	r.eventually("a runner to start", func() bool { return len(r.ex.Running()) == 1 })
	if g := r.forge.Grants(); len(g) != 1 || g[0].Repo != "baney75/app" {
		t.Fatalf("grants %+v", g)
	}

	ctx := context.Background()
	c := r.client()
	var st Status
	if err := c.Get(ctx, "/v1/status", &st); err != nil {
		t.Fatal(err)
	}
	if st.Device != "box" || st.Worker == nil || len(st.Worker.Running) != 1 || st.Broker == nil {
		t.Fatalf("status %+v", st)
	}
	if st.Mesh.Up || !strings.Contains(st.Mesh.Warning, "local only") {
		t.Errorf("mesh should be degraded to local only: %+v", st.Mesh)
	}
	if !strings.HasPrefix(st.Why, "boost: plugged in") {
		t.Errorf("why %q", st.Why)
	}
	if st.InUse.MemoryBytes != 2<<30 {
		t.Errorf("in use %+v", st.InUse)
	}

	// A reload changes the budget without touching the running job.
	r.eventually("boost decision", func() bool {
		return r.d.Status(ctx).Decision.Limit.MemoryBytes == 8<<30
	})
	r.cfg.Budget.Boost.Memory = 12 * config.GiB
	r.save()
	if err := c.Post(ctx, "/v1/reload", nil); err != nil {
		t.Fatal(err)
	}
	r.eventually("the new budget", func() bool {
		return r.d.Status(ctx).Decision.Limit.MemoryBytes == 12<<30
	})
	if n := len(r.ex.Calls("Stop")); n != 0 {
		t.Errorf("reload stopped %d runners", n)
	}

	// A broken config is refused and the running one stays.
	os.WriteFile(r.paths.Config, []byte("budget = 7\n"), 0o644)
	if err := c.Post(ctx, "/v1/reload", nil); err == nil {
		t.Error("broken config accepted")
	}
	r.save()

	// A labels change rebuilds the worker, which adopts the running job.
	r.cfg.Labels = []string{"gpu"}
	r.save()
	if err := c.Post(ctx, "/v1/reload", nil); err != nil {
		t.Fatal(err)
	}
	r.eventually("the new worker to adopt the job", func() bool {
		s := r.d.Status(ctx)
		return s.Worker != nil && len(s.Worker.Running) == 1 && strings.Contains(strings.Join(s.Worker.Labels, ","), "gpu")
	})
	if n := len(r.ex.Calls("Stop")); n != 0 {
		t.Errorf("worker rebuild stopped %d runners", n)
	}

	// Pause with a time limit, then it ends on its own.
	if err := c.Post(ctx, "/v1/pause", control.PauseRequest{Reason: "video call", ForS: 1}); err != nil {
		t.Fatal(err)
	}
	if s := r.d.Status(ctx); !s.Paused || s.PauseReason != "video call" || s.PauseUntil == nil {
		t.Errorf("not paused: %+v", s)
	}
	r.eventually("the pause to end", func() bool { return !r.d.Status(ctx).Paused })

	// Doctor through the API.
	var rep Report
	if err := c.Get(ctx, "/v1/doctor", &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) == 0 {
		t.Error("doctor returned no checks")
	}

	// Shutdown is clean and leaves the busy job running.
	start := time.Now()
	if err := r.stop(); err != nil {
		t.Fatalf("run returned %v", err)
	}
	if time.Since(start) > ShutdownTimeout {
		t.Errorf("shutdown took %s", time.Since(start))
	}
	if len(r.ex.Running()) != 1 {
		t.Errorf("shutdown stopped the running job")
	}
	if v := r.ex.Violations(); len(v) > 0 {
		t.Errorf("executor contract violations: %v", v)
	}
	if _, err := net.Dial("tcp", r.d.ControlAddress()); err == nil {
		t.Error("control API still listening after shutdown")
	}
}

func TestDaemonWithoutTokenIsWorkerOnly(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	r.store.m = map[string][]byte{}
	r.start()
	defer r.stop()
	s := r.d.Status(context.Background())
	if s.Broker != nil {
		t.Error("broker without a token")
	}
	if s.Mesh.Enabled || len(s.Warnings) != 0 {
		t.Errorf("mesh off should warn nothing: %+v", s)
	}
}

func TestDaemonMeshUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	r := newRig(t, func(c *config.Config) { c.Mesh.Port = port })
	r.ts.self = &mesh.Peer{ID: "n1", HostName: "box", DNSName: "box.tail.ts.net", IPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, Online: true, LoginName: "me@example.com"}
	r.start()
	defer r.stop()
	s := r.d.Status(context.Background())
	if !s.Mesh.Up || s.Mesh.Self != "box.tail.ts.net" || len(s.Warnings) != 0 {
		t.Errorf("mesh %+v warnings %v", s.Mesh, s.Warnings)
	}
}

func TestDaemonBrokenConfigStillServes(t *testing.T) {
	r := newRig(t, nil)
	os.WriteFile(r.paths.Config, []byte("nonsense = [\n"), 0o644)
	r.start()
	s := r.d.Status(context.Background())
	if len(s.Problems) == 0 || s.Problems[0].Check != "config" || s.Worker != nil {
		t.Errorf("status %+v", s)
	}
	// Fixing it and reloading brings everything up.
	r.save()
	if err := r.d.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := r.d.Status(context.Background()); s.Worker == nil {
		t.Error("no worker after reload")
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonAutoUpdateRestarts(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Update.Auto = true; c.Mesh.Enabled = false })
	r.up.newer.Store(true)
	r.start()
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrRestart) {
			t.Fatalf("run returned %v, want ErrRestart", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no restart after an update")
	}
	if r.up.applied.Load() != 1 {
		t.Errorf("applied %d times", r.up.applied.Load())
	}
	// The pending-update marker tells the new binary to prove itself.
	b, err := os.ReadFile(filepath.Join(r.paths.State, "update-pending.json"))
	if err != nil {
		t.Fatalf("no pending-update marker: %v", err)
	}
	var m struct {
		From, To string
		At       time.Time
	}
	if err := json.Unmarshal(b, &m); err != nil || m.From != "1.2.3" || m.To != "9.9.9" || time.Since(m.At) > time.Minute {
		t.Fatalf("marker %s (%v)", b, err)
	}
}

func writeMarker(t *testing.T, r *rig, age time.Duration, to string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"from": "1.2.2", "to": to, "at": time.Now().Add(-age)})
	p := filepath.Join(r.paths.State, "update-pending.json")
	if err := os.MkdirAll(r.paths.State, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A new binary that keeps failing before it gets healthy is restarted by
// the service manager; once the update is 2 minutes old, the next start
// rolls back and exits so the previous binary runs.
func TestDaemonRollsBackAnUnhealthyUpdate(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	marker := writeMarker(t, r, 3*time.Minute, "1.2.3")
	r.launch()
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrRestart) {
			t.Fatalf("run returned %v, want a restart", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no rollback restart")
	}
	if got, _ := r.up.rolledBack.Load().(string); got != "/usr/local/bin/tentmaker" {
		t.Fatalf("rolled back %q", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker left: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.paths.State, "update-bad.json")); !strings.Contains(string(b), `"1.2.3"`) {
		t.Errorf("bad version not recorded: %s", b)
	}
}

func TestDaemonClearsMarkerOnceHealthy(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	marker := writeMarker(t, r, 10*time.Second, "1.2.3")
	r.start()
	r.eventually("the marker to go", func() bool {
		_, err := os.Stat(marker)
		return errors.Is(err, os.ErrNotExist)
	})
	if got, _ := r.up.rolledBack.Load().(string); got != "" {
		t.Fatalf("rolled back a healthy update")
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

// A version that was rolled back is never auto-applied again.
func TestDaemonSkipsARolledBackVersion(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Update.Auto = true; c.Mesh.Enabled = false })
	if err := os.MkdirAll(r.paths.State, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.paths.State, "update-bad.json"), []byte(`{"versions":["9.9.9"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.up.newer.Store(true)
	r.start()
	time.Sleep(time.Second)
	if n := r.up.applied.Load(); n != 0 {
		t.Fatalf("applied a rolled-back version %d times", n)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonAutoUpdateWaitsForJobs(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Update.Auto = true; c.Mesh.Enabled = false })
	r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "build", Labels: []string{"self-hosted", "tentmaker", "linux"}})
	r.start()
	r.eventually("a runner to start", func() bool { return len(r.d.Status(context.Background()).Worker.Running) == 1 })
	r.up.newer.Store(true)
	time.Sleep(500 * time.Millisecond)
	if r.up.applied.Load() != 0 {
		t.Error("update applied while a job runs")
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

// forgeArchives serves the fake forge's archive URLs, only with its token.
type forgeArchives struct{ f *fakeforge.Forge }

func (t forgeArchives) RoundTrip(r *http.Request) (*http.Response, error) {
	code, body := http.StatusForbidden, "no"
	if r.URL.Host == "forge.example" && r.Header.Get("Authorization") == "Bearer "+t.f.ArchiveToken() {
		code, body = http.StatusOK, "tarball of "+r.URL.Path
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// A check job gets its source from the control API's relay, which streams
// what the broker fetched with the forge token; the job never sees the token.
func TestDaemonChecksRelay(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	r := newRig(t, func(c *config.Config) {
		c.Mesh.Enabled = false
		c.Repos = []config.Repo{{Name: "baney75/app", Mode: "checks"}}
	})
	r.forge.SetFile("baney75/app", sha, ".github/workflows/ci.yml",
		[]byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make\n"))
	r.forge.SetCommits("baney75/app", []core.Commit{{SHA: sha, Ref: "main"}})
	r.start()
	defer r.stop()
	r.eventually("a check to start", func() bool { return len(r.ex.Running()) == 1 })
	spec, ok := r.ex.Spec(r.ex.Running()[0])
	if !ok {
		t.Fatal("no spec")
	}
	if strings.Contains(string(spec.Grant.Secret), r.forge.ArchiveToken()) || strings.Contains(string(spec.Grant.Secret), "ghp_secret_test_token") {
		t.Fatalf("the check bundle carries a forge token")
	}
	var b core.CheckBundle
	if err := json.Unmarshal(spec.Grant.Secret, &b); err != nil {
		t.Fatal(err)
	}
	// The relay has its own listener, apart from the control API that the
	// job fence blocks (review r1 partB first-F1).
	if r.d.RelayAddress() == "" || r.d.RelayAddress() == r.d.ControlAddress() {
		t.Fatalf("relay %q shares the control API's listener %q", r.d.RelayAddress(), r.d.ControlAddress())
	}
	want := "http://" + r.d.RelayAddress() + "/v1/relay/"
	if !strings.HasPrefix(b.ArchiveURL, want) || b.Header != "" {
		t.Fatalf("archive URL %q, want the relay listener %s", b.ArchiveURL, want)
	}
	id := strings.TrimPrefix(b.ArchiveURL, want)
	if resp, err := http.Get("http://" + r.d.ControlAddress() + "/v1/relay/" + id); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the control listener serves the relay: %v %v", resp, err)
	}
	if resp, err := http.Get("http://" + r.d.RelayAddress() + "/v1/status"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the relay listener serves status: %v %v", resp, err)
	}
	resp, err := http.Get(b.ArchiveURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "tarball of /baney75/app/archive/"+sha+".tar.gz" {
		t.Fatalf("relay: %d %q", resp.StatusCode, body)
	}
}

// The tentmaker_badstart test build exits at start, like a bad release
// would; the update guard runs first, so it still rolls back.
func TestDaemonBadStartBuildStillRollsBack(t *testing.T) {
	old := failStart
	failStart = "bad build"
	t.Cleanup(func() { failStart = old })

	r := newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	r.launch()
	select {
	case err := <-r.done:
		if err == nil || errors.Is(err, ErrRestart) || !strings.Contains(err.Error(), "bad build") {
			t.Fatalf("run returned %v, want the bad build's exit", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the bad build kept running")
	}

	r = newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	writeMarker(t, r, 3*time.Minute, "1.2.3")
	r.launch()
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrRestart) {
			t.Fatalf("run returned %v, want the rollback restart", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no rollback restart")
	}
	if b, _ := os.ReadFile(filepath.Join(r.paths.State, "update-bad.json")); !strings.Contains(string(b), `"1.2.3"`) {
		t.Errorf("bad version not recorded: %s", b)
	}
}

func TestUpdateEveryFromEnv(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 0, "junk": 0, "-5s": 0, "20s": 20 * time.Second, "1s": 10 * time.Second, "2h": 2 * time.Hour,
	} {
		got := UpdateEveryFromEnv(func(k string) string {
			if k == EnvUpdateEvery {
				return in
			}
			return ""
		})
		if got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
}

// A job checks mode skips gets no status, so the log has to say why;
// otherwise the user sees nothing at all.
func TestPlannerLogsSkippedJobs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	src := []byte("on: push\njobs:\n  db:\n    runs-on: ubuntu-latest\n    services:\n      pg:\n        image: postgres\n    steps:\n      - run: echo hi\n  ok:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n")
	jobs, err := planner(log)(".tm-scn/f.yml", src, "github")
	if err != nil || len(jobs) != 1 || jobs[0].Name != "ok" {
		t.Fatalf("jobs %+v err %v", jobs, err)
	}
	out := buf.String()
	for _, want := range []string{"check job skipped", "job=db", "service containers (services:)", ".tm-scn/f.yml"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusRuntime(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	r.start()
	defer r.stop()
	s := r.d.Status(context.Background())
	rt := s.Runtime
	if rt == nil {
		t.Fatal("status has no runtime section")
	}
	if rt.PID != os.Getpid() || rt.Goroutines <= 0 || rt.HeapInUseBytes == 0 || rt.SysBytes < rt.HeapInUseBytes || rt.UptimeS < 0 {
		t.Errorf("runtime %+v", *rt)
	}
	switch runtime.GOOS {
	case "linux", "darwin":
		if rt.OpenFDs < 3 {
			t.Errorf("open fds %d, want at least stdin, stdout and stderr", rt.OpenFDs)
		}
	default:
		if rt.OpenFDs != -1 {
			t.Errorf("open fds %d on %s, want -1", rt.OpenFDs, runtime.GOOS)
		}
	}
	b, err := json.Marshal(s)
	if err != nil || !strings.Contains(string(b), `"runtime":{"pid":`) || !strings.Contains(string(b), `"open_fds":`) {
		t.Errorf("status JSON %s, %v", b, err)
	}
}

func TestOpenFDsCountsANewFile(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("fds are counted on Linux and macOS")
	}
	// Goroutines left by earlier tests may open or close files too, so
	// one clean attempt out of ten is enough.
	var before, after int
	for i := 0; i < 10; i++ {
		before = openFDs(runtime.GOOS)
		f, err := os.Open(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		after = openFDs(runtime.GOOS)
		f.Close()
		if after == before+1 {
			return
		}
	}
	t.Errorf("open fds %d then %d after opening a file", before, after)
}
