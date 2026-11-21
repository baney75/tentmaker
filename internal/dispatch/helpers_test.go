package dispatch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

var (
	linuxX64   = core.Platform{OS: "linux", Arch: "x64"}
	linuxARM   = core.Platform{OS: "linux", Arch: "arm64"}
	macosARM   = core.Platform{OS: "macos", Arch: "arm64"}
	jobLabels  = []string{"self-hosted", "tentmaker", "linux"}
	errBoom    = errors.New("boom")
	t0         = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	bg         = context.Background()
	repoApp    = "baney75/app"
	repoTools  = "baney75/tools"
	repoSite   = "baney75/site"
	ciWorkflow = ".github/workflows/ci.yml"
)

// clock is an injected clock the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// budget is a fake core.Budgeter. Like the real one, Admit is false once
// running reaches Slots.
type budget struct {
	mu          sync.Mutex
	d           core.Decision
	lastRunning int
	calls       int
}

func (b *budget) Decide(ctx context.Context, now time.Time, running int) core.Decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.lastRunning = running
	d := b.d
	d.Reasons = append([]string(nil), b.d.Reasons...)
	if running >= d.Slots {
		d.Admit = false
	}
	return d
}

func (b *budget) set(d core.Decision) {
	b.mu.Lock()
	b.d = d
	b.mu.Unlock()
}

func (b *budget) running() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastRunning
}

func admit(slots int) core.Decision {
	return core.Decision{Profile: "everyday", Slots: slots, Admit: true,
		Limit: core.Resources{MemoryBytes: 8 << 30, CPUs: 8}}
}

// logBuf is a goroutine-safe log capture.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// claimer is a fake core.Claimer: the first device to claim a job owns it.
type claimer struct {
	mu     sync.Mutex
	owner  map[string]string
	claims int
	denied int
}

func (c *claimer) Claim(ctx context.Context, job core.Job, device string, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims++
	if c.owner == nil {
		c.owner = map[string]string{}
	}
	if o, ok := c.owner[job.Key()]; ok && o != device {
		c.denied++
		return false
	}
	c.owner[job.Key()] = device
	return true
}

func (c *claimer) Release(ctx context.Context, job core.Job, device string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner[job.Key()] == device {
		delete(c.owner, job.Key())
	}
}

// source wraps a WorkSource so a test can make it fail and see requests.
type source struct {
	mu    sync.Mutex
	inner core.WorkSource
	fail  error
	reqs  []core.WorkRequest
}

func (s *source) Name() string { return s.inner.Name() }

func (s *source) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		return core.WorkResponse{}, fail
	}
	return s.inner.Work(ctx, req)
}

// FetchArchive passes the relay through to the wrapped broker.
func (s *source) FetchArchive(ctx context.Context, ref, device string, w io.Writer) error {
	as, ok := s.inner.(core.ArchiveSource)
	if !ok {
		return core.ErrNotFound
	}
	return as.FetchArchive(ctx, ref, device, w)
}

func (s *source) setFail(err error) {
	s.mu.Lock()
	s.fail = err
	s.mu.Unlock()
}

func (s *source) last() core.WorkRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

type wrig struct {
	name    string
	w       *Worker
	ex      *fakeexec.Executor
	budget  *budget
	dir     string
	sources []core.WorkSource
	silent  bool
}

type rig struct {
	t       *testing.T
	clk     *clock
	cfg     config.Config
	forge   *fakeforge.Forge
	broker  *Broker
	bdir    string
	logs    *logBuf
	log     *slog.Logger
	workers []*wrig
	planner *planner
}

// rigLikeGitHub makes newRig's fake forge behave like GitHub (see
// fakeforge.LikeGitHub). TestRunnerModeLikeGitHub sets it.
var rigLikeGitHub bool

func testConfig(device string, repos ...config.Repo) config.Config {
	c := config.Default(16<<30, 8)
	c.Device = device
	c.Repos = repos
	return c
}

func runnerRepo(name string) config.Repo { return config.Repo{Name: name, Mode: "runner"} }

func newRig(t *testing.T, repos ...config.Repo) *rig {
	t.Helper()
	clk := &clock{t: t0}
	logs := &logBuf{}
	r := &rig{t: t, clk: clk, logs: logs, planner: &planner{calls: map[string]int{}}}
	r.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.cfg = testConfig("alpha", repos...)
	r.forge = fakeforge.New("github", clk.Now)
	r.forge.LikeGitHub(rigLikeGitHub)
	r.bdir = t.TempDir()
	r.broker = r.newBroker(r.cfg, r.bdir, nil)
	return r
}

func (r *rig) newBroker(cfg config.Config, dir string, c core.Claimer) *Broker {
	return NewBroker(BrokerOptions{
		Config:       cfg,
		Forges:       map[string]core.Forge{"github": r.forge},
		StatusForges: map[string]core.StatusForge{"github": r.forge},
		Planner:      r.planner.plan,
		Claimer:      c,
		HTTP:         &http.Client{Transport: forgeTransport{r.forge}},
		StateDir:     dir,
		Log:          r.log.With("broker", cfg.Device),
		Now:          r.clk.Now,
	})
}

// forgeTransport answers the fake forge's archive URLs: the tarball, only
// with the fake's token in the URL and the header.
type forgeTransport struct{ f *fakeforge.Forge }

func (t forgeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	code, body := http.StatusOK, archiveBytes(req.URL.Path)
	if req.URL.Host != "forge.example" || req.URL.Query().Get("token") != t.f.ArchiveToken() ||
		req.Header.Get("Authorization") != "Bearer "+t.f.ArchiveToken() {
		code, body = http.StatusForbidden, "forbidden"
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
}

// archiveBytes is what the fake forge serves as the tarball at path.
func archiveBytes(path string) string { return "tarball of " + path }

// addWorker adds a worker with one fake executor and a budget of slots.
func (r *rig) addWorker(name string, slots int, platforms ...core.Platform) *wrig {
	return r.addWorkerWith(name, slots, 0, platforms...)
}

// addWorkerWith also sets the executor's MaxSlots.
func (r *rig) addWorkerWith(name string, slots, maxSlots int, platforms ...core.Platform) *wrig {
	if len(platforms) == 0 {
		platforms = []core.Platform{linuxX64}
	}
	ex := fakeexec.New("fake-"+name, maxSlots, platforms...)
	wr := &wrig{name: name, ex: ex, budget: &budget{d: admit(slots)}, dir: r.t.TempDir(),
		sources: []core.WorkSource{r.broker}}
	wr.w = r.newWorker(wr)
	r.workers = append(r.workers, wr)
	return wr
}

func (r *rig) newWorker(wr *wrig) *Worker {
	cfg := r.cfg
	cfg.Device = wr.name
	return NewWorker(WorkerOptions{
		Config:    cfg,
		Device:    wr.name,
		Executors: []core.Executor{wr.ex},
		Budget:    wr.budget,
		Sources:   func() []core.WorkSource { return wr.sources },
		StateDir:  wr.dir,
		Log:       r.log.With("worker", wr.name),
		Now:       r.clk.Now,
	})
}

// tick runs one broker pass, then one pass of every worker that is not silent.
func (r *rig) tick() {
	r.t.Helper()
	if err := r.broker.Tick(bg); err != nil {
		r.t.Fatalf("broker tick: %v", err)
	}
	for _, w := range r.workers {
		if w.silent {
			continue
		}
		if err := w.w.Tick(bg); err != nil {
			r.t.Fatalf("worker %s tick: %v", w.name, err)
		}
	}
}

// run advances the clock in steps of step for d, ticking after each.
func (r *rig) run(d, step time.Duration) {
	r.t.Helper()
	for passed := time.Duration(0); passed < d; passed += step {
		r.clk.Add(step)
		r.tick()
	}
}

func (r *rig) addJob(repo string, mods ...func(*core.Job)) core.Job {
	j := core.Job{Repo: repo, Name: "test", Workflow: "ci", Event: "push", HeadBranch: "main",
		Labels: append([]string(nil), jobLabels...), CreatedAt: r.clk.Now().Add(-time.Minute)}
	for _, m := range mods {
		m(&j)
	}
	return r.forge.AddJob(j)
}

// started lists the runner names the executor was asked to start.
func started(ex *fakeexec.Executor) []string {
	var out []string
	for _, c := range ex.Calls("Start") {
		out = append(out, c.Name)
	}
	return out
}

func called(ex *fakeexec.Executor, method, name string) bool {
	for _, c := range ex.Calls(method) {
		if c.Name == name {
			return true
		}
	}
	return false
}

func (r *rig) grantFor(name string) GrantStatus {
	for _, g := range r.broker.Status().Runners {
		if g.Name == name {
			return g
		}
	}
	return GrantStatus{}
}

func (r *rig) device(name string) DeviceStatus {
	for _, d := range r.broker.Status().Devices {
		if d.Name == name {
			return d
		}
	}
	return DeviceStatus{}
}

// assertNoSecrets fails if any grant secret or archive token appears in the
// logs or in a state file.
func (r *rig) assertNoSecrets() {
	r.t.Helper()
	var secrets []string
	for _, g := range r.forge.Grants() {
		secrets = append(secrets, string(g.Secret))
	}
	secrets = append(secrets, r.forge.ArchiveToken())
	logs := r.logs.String()
	files := []string{filepath.Join(r.bdir, "broker.json")}
	for _, w := range r.workers {
		files = append(files, filepath.Join(w.dir, "state.json"))
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(logs, s) {
			r.t.Errorf("secret %q appears in the logs", s)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err == nil && strings.Contains(string(b), s) {
				r.t.Errorf("secret %q appears in %s", s, f)
			}
		}
	}
}

// planner is a fake checks planner: every workflow file is a list of
// "name[:os]" lines, one check job each.
type planner struct {
	mu    sync.Mutex
	calls map[string]int
	err   error
}

func (p *planner) plan(path string, src []byte, forge string) ([]core.CheckJob, error) {
	p.mu.Lock()
	p.calls[path+"|"+string(src)]++
	err := p.err
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var jobs []core.CheckJob
	for _, line := range strings.Split(strings.TrimSpace(string(src)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, os, _ := strings.Cut(line, ":")
		if os == "" {
			os = "linux"
		}
		jobs = append(jobs, core.CheckJob{Workflow: path, ID: name, Name: name,
			Platform: core.Platform{OS: os, Arch: "x64"},
			Steps:    []core.CheckStep{{Name: "run", Run: "make " + name}}, Timeout: 10 * time.Minute})
	}
	return jobs, nil
}

func (p *planner) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range p.calls {
		n += v
	}
	return n
}
