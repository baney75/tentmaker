package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/checks"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/executor"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/power"
	"github.com/baney75/tentmaker/internal/secrets"
	"github.com/baney75/tentmaker/internal/service"
	"github.com/baney75/tentmaker/internal/update"
)

// Defaults from docs/CLI.md.
const (
	ShutdownTimeout = 20 * time.Second
	UpdateEvery     = 6 * time.Hour
	// meshCheckEvery is how long a tailscale reading is reused in status.
	meshCheckEvery = 30 * time.Second
)

// ErrRestart is what Run returns after a self-update: the service manager
// restarts the new binary.
var ErrRestart = errors.New("updated; restarting into the new binary")

// EnvUpdateEvery is a test hook: how often auto-update checks the channel
// (a Go duration, at least 10 s), so an end-to-end test can watch an update
// and its rollback without waiting 6 hours.
const EnvUpdateEvery = "TENTMAKER_UPDATE_EVERY"

// UpdateEveryFromEnv reads EnvUpdateEvery; 0 means unset or unreadable.
func UpdateEveryFromEnv(getenv func(string) string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(getenv(EnvUpdateEvery)))
	if err != nil || d <= 0 {
		return 0
	}
	return max(d, 10*time.Second)
}

// failStart is set only in a build made with -tags tentmaker_badstart: the
// daemon exits with it right after the update guard, as a bad release
// would, so the end-to-end test can prove the rollback.
var failStart string

// Updater is what the auto-update loop needs from internal/update.
type Updater interface {
	Check(ctx context.Context, channel, current string) (update.Release, bool, error)
	Download(ctx context.Context, rel update.Release, dir string) (string, error)
	Apply(ctx context.Context, newBin, curBin, wantVersion string) error
	// Rollback restores the binary Apply replaced.
	Rollback(curBin string) error
}

// ExecutorFactory builds this host's executors (executor.ForHost).
type ExecutorFactory func(cfg config.Config, binary func(core.Platform) (string, error), log *slog.Logger) ([]core.Executor, []core.Problem)

// Options configures a Daemon. Zero values pick the host's.
type Options struct {
	ConfigPath string
	Paths      Paths
	Version    string
	Log        *slog.Logger

	Power       core.PowerSource
	Idle        core.IdleSource
	Probe       func(stateDir string) guards.Probe
	Executors   ExecutorFactory
	Binary      func(core.Platform) (string, error) // default NewBinary
	NewForge    ForgeFactory
	Secrets     secrets.Store
	Tailscale   mesh.Tailscale
	ControlAddr string // default control.DefaultAddr; "-" serves no control API
	Updater     Updater
	Service     func(ctx context.Context) (service.State, error) // for doctor; nil = the daemon is the service
	TotalMemory func() (int64, error)
	CPUs        func() int
	Hostname    func() (string, error)
	Executable  func() (string, error)
	Now         func() time.Time
	// ArchiveHTTP downloads check source archives from forges (nil: a
	// client with dial and header timeouts; the fetch has its own limit).
	ArchiveHTTP *http.Client

	UpdateEvery     time.Duration
	ShutdownTimeout time.Duration
	// ReloadOnHUP reloads the config on SIGHUP (unix; ignored on Windows).
	ReloadOnHUP bool
}

func (o *Options) defaults() {
	if o.Paths == (Paths{}) {
		o.Paths = DefaultPaths(runtime.GOOS, os.Getenv)
	}
	if o.ConfigPath == "" {
		o.ConfigPath = config.Path()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Power == nil {
		o.Power = power.New()
	}
	if o.Idle == nil {
		o.Idle = power.NewIdle()
	}
	if o.Probe == nil {
		o.Probe = guards.NewHostProbe
	}
	if o.Executors == nil {
		o.Executors = executor.ForHost
	}
	if o.NewForge == nil {
		o.NewForge = DefaultForge
	}
	if o.Secrets == nil {
		o.Secrets = secrets.Dir(o.Paths.Secrets)
	}
	if o.Tailscale == nil {
		o.Tailscale = mesh.NewTailscaleCLI()
	}
	if o.ControlAddr == "" {
		o.ControlAddr = control.DefaultAddr
	}
	if o.Updater == nil {
		o.Updater = update.Default
	}
	if o.TotalMemory == nil {
		o.TotalMemory = power.TotalMemory
	}
	if o.CPUs == nil {
		o.CPUs = power.CPUs
	}
	if o.Hostname == nil {
		o.Hostname = os.Hostname
	}
	if o.Executable == nil {
		o.Executable = os.Executable
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.UpdateEvery <= 0 {
		o.UpdateEvery = UpdateEvery
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = ShutdownTimeout
	}
}

// comp is one running component loop.
type comp struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startComp(parent context.Context, log *slog.Logger, name string, run func(ctx context.Context) error) *comp {
	ctx, cancel := context.WithCancel(parent)
	c := &comp{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer func() {
			if r := recover(); r != nil {
				log.Error("component crashed", "component", name, "panic", fmt.Sprint(r))
			}
		}()
		if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("component stopped", "component", name, "error", err)
		}
	}()
	return c
}

func (c *comp) stop() {
	if c == nil {
		return
	}
	c.cancel()
	<-c.done
}

// budgetProxy lets reload swap the budget engine under a running worker.
type budgetProxy struct{ e atomic.Pointer[budget.Engine] }

func (b *budgetProxy) Decide(ctx context.Context, now time.Time, running int) core.Decision {
	e := b.e.Load()
	if e == nil {
		return core.Decision{Profile: budget.Off, Reasons: []string{"budget not ready"}}
	}
	return e.Decide(ctx, now, running)
}

// claimerProxy forwards to the current mesh node; with none, every claim
// is granted (a single device has nobody to race).
type claimerProxy struct{ d *Daemon }

func (c claimerProxy) Claim(ctx context.Context, job core.Job, device string, ttl time.Duration) bool {
	if n := c.d.currentNode(); n != nil {
		return n.Claim(ctx, job, device, ttl)
	}
	return true
}

func (c claimerProxy) Release(ctx context.Context, job core.Job, device string) {
	if n := c.d.currentNode(); n != nil {
		n.Release(ctx, job, device)
	}
}

// brokerProxy is the local broker as the mesh node serves it.
type brokerProxy struct{ d *Daemon }

func (b brokerProxy) Name() string { return b.d.device() }
func (b brokerProxy) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	br := b.d.currentBroker()
	if br == nil {
		return core.WorkResponse{}, errors.New("this device has no broker")
	}
	return br.Work(ctx, req)
}

// FetchArchive serves a check's source archive to the mesh node.
func (b brokerProxy) FetchArchive(ctx context.Context, ref, device string, w io.Writer) error {
	br := b.d.currentBroker()
	if br == nil {
		return core.ErrNotFound
	}
	return br.FetchArchive(ctx, ref, device, w)
}

// Daemon is one running device.
type Daemon struct {
	o      Options
	log    *slog.Logger
	budget *budgetProxy

	runCtx    context.Context // parent of every component; ends after shutdown
	cancelRun context.CancelFunc

	buildMu sync.Mutex // serializes apply

	mu         sync.RWMutex
	cfg        config.Config // resolved, Device filled
	cfgErr     error
	worker     *dispatch.Worker
	broker     *dispatch.Broker
	node       *mesh.Node
	execs      []core.Executor
	forges     Forges
	problems   map[string][]core.Problem // by component
	keys       map[string]string
	pauseUntil time.Time
	started    time.Time
	meshAt     time.Time
	meshStat   MeshStatus
	updating   bool

	wComp, bComp, nComp *comp

	guard    updateGuard
	ctlAddr  string
	restart  chan struct{}
	exitErr  error
	exitOnce sync.Once
}

// New returns a Daemon. It touches nothing until Run.
func New(o Options) *Daemon {
	o.defaults()
	runCtx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		runCtx:    runCtx,
		cancelRun: cancel,
		o:         o,
		log:       o.Log,
		budget:    &budgetProxy{},
		problems:  map[string][]core.Problem{},
		keys:      map[string]string{},
		restart:   make(chan struct{}),
		guard:     updateGuard{fs: osStateFS{}, dir: o.Paths.State},
	}
}

func (d *Daemon) device() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg.Device
}

func (d *Daemon) currentNode() *mesh.Node {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.node
}

func (d *Daemon) currentBroker() *dispatch.Broker {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.broker
}

// ControlAddress is where the control API listens ("" until it does).
func (d *Daemon) ControlAddress() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ctlAddr
}

// Config is the running, resolved config.
func (d *Daemon) Config() config.Config {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg
}

// load reads and validates the config file.
func (d *Daemon) load() (config.Config, error) {
	mem, err := d.o.TotalMemory()
	if err != nil {
		mem = 8 << 30
	}
	cfg, err := config.Load(d.o.ConfigPath, mem, d.o.CPUs())
	if err != nil {
		return cfg, err
	}
	if ps := cfg.Validate(); len(ps) > 0 {
		return cfg, fmt.Errorf("config %s: %s", d.o.ConfigPath, ps[0])
	}
	cfg = Resolve(cfg, d.o.Paths)
	cfg.Device = DeviceName(cfg, d.o.Hostname)
	return cfg, nil
}

// Run builds everything, serves until ctx ends (or a self-update asks for a
// restart) and shuts down within the shutdown timeout. Component failures
// never end Run; they show up in status and doctor.
func (d *Daemon) Run(ctx context.Context) error {
	runCtx := d.runCtx
	defer d.cancelRun()
	d.mu.Lock()
	d.started = d.o.Now()
	d.mu.Unlock()

	// Before anything else, so a new binary that fails while building its
	// components still rolls back.
	watch, err := d.checkPendingUpdate()
	if err != nil {
		return err
	}
	if failStart != "" {
		d.log.Error("exiting at start", "why", failStart)
		return errors.New(failStart)
	}

	cfg, err := d.load()
	if err != nil {
		d.log.Error("config not loaded; serving status only until it is fixed and reloaded", "error", err)
		d.mu.Lock()
		d.cfgErr = err
		d.cfg = Resolve(cfg, d.o.Paths)
		d.cfg.Device = DeviceName(d.cfg, d.o.Hostname)
		d.mu.Unlock()
	} else {
		d.apply(cfg)
		d.restorePause()
	}
	d.log.Info("tentmaker daemon started", "version", d.o.Version, "device", d.device(), "config", d.o.ConfigPath)

	var ctl *comp
	if d.o.ControlAddr != "-" {
		ctl = d.startControl(runCtx)
	}
	loops := startComp(runCtx, d.log, "loops", func(ctx context.Context) error {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); d.pauseLoop(ctx) }()
		go func() { defer wg.Done(); d.updateLoop(ctx) }()
		go func() { defer wg.Done(); d.hupLoop(ctx) }()
		if watch != nil {
			wg.Add(1)
			go func() { defer wg.Done(); d.watchUpdate(ctx, *watch) }()
		}
		wg.Wait()
		return nil
	})

	select {
	case <-ctx.Done():
		d.log.Info("stopping")
	case <-d.restart:
		d.log.Info("restarting for an update")
	}
	loops.stop()
	d.shutdown()
	ctl.stop()
	return d.exitErr
}

func (d *Daemon) startControl(ctx context.Context) *comp {
	cfg := d.Config()
	ln, err := control.Listen(d.o.ControlAddr)
	if err != nil {
		d.setProblems("control", []core.Problem{{Check: "control", What: "control API not served: " + err.Error(),
			Fix: "stop whatever uses " + d.o.ControlAddr + ", then restart the service"}})
		d.log.Error("control API not served", "error", err)
		return nil
	}
	tok, err := control.NewToken(control.TokenPath(cfg.StateDir))
	if err != nil {
		d.setProblems("control", []core.Problem{{Check: "control", What: err.Error(), Fix: "check " + cfg.StateDir}})
		d.log.Error("control token not written; mutating control calls are refused", "error", err)
	}
	d.mu.Lock()
	d.ctlAddr = ln.Addr().String()
	d.mu.Unlock()
	srv := control.NewServer(d.Handler(), tok, d.log)
	return startComp(ctx, d.log, "control", func(ctx context.Context) error { return srv.Serve(ctx, ln) })
}

func (d *Daemon) setProblems(component string, ps []core.Problem) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(ps) == 0 {
		delete(d.problems, component)
		return
	}
	d.problems[component] = ps
}

func keyOf(v ...any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// planner is checks.Plan for the broker. A skipped job gets no commit
// status, so its reason goes to the log; the broker plans each commit once.
func planner(log *slog.Logger) func(path string, src []byte, forge string) ([]core.CheckJob, error) {
	return func(path string, src []byte, forge string) ([]core.CheckJob, error) {
		jobs, skipped, err := checks.Plan(path, src, forge)
		for _, s := range skipped {
			log.Info("check job skipped", "workflow", path, "job", s.Job, "why", s.Reason)
		}
		return jobs, err
	}
}

// apply builds or rebuilds the components whose inputs changed. Running
// jobs are never stopped: a worker rebuild leaves its sandboxes running and
// the new worker adopts them from state.json.
func (d *Daemon) apply(cfg config.Config) {
	d.buildMu.Lock()
	defer d.buildMu.Unlock()
	log := d.log

	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		d.setProblems("state", []core.Problem{{Check: "state", What: "can't create the state dir: " + err.Error(), Fix: "check " + cfg.StateDir}})
	} else {
		d.setProblems("state", nil)
	}

	d.mu.Lock()
	d.cfg, d.cfgErr = cfg, nil
	d.mu.Unlock()

	// Budget: always rebuilt, swapped under the running worker.
	d.budget.e.Store(budget.New(cfg, d.o.Power, d.o.Idle, d.o.Probe(cfg.StateDir)))

	// Forges and broker.
	forges := BuildForges(cfg, d.o.Secrets, d.o.NewForge, log)
	d.setProblems("forges", forges.Problems)
	bkey := keyOf(cfg.Device, cfg.Repos, cfg.Forges, cfg.DenyRepos, cfg.Job.IdleTTL, cfg.StateDir, forges.Tokens)
	if bkey != d.keys["broker"] {
		d.mu.Lock()
		old, oldComp := d.broker, d.bComp
		d.mu.Unlock()
		if old != nil {
			oldComp.stop()
			sctx, cancel := context.WithTimeout(context.Background(), d.o.ShutdownTimeout)
			old.Shutdown(sctx)
			cancel()
		}
		var b *dispatch.Broker
		var bc *comp
		if !forges.Empty() {
			b = dispatch.NewBroker(dispatch.BrokerOptions{
				Config:       cfg,
				Forges:       forges.Forges,
				StatusForges: forges.StatusForges,
				Planner:      planner(log),
				Claimer:      claimerProxy{d},
				HTTP:         d.o.ArchiveHTTP,
				StateDir:     cfg.StateDir,
				Log:          log,
				Now:          d.o.Now,
			})
			bc = startComp(d.runCtx, log, "broker", b.Run)
			log.Info("broker started", "forges", forges.Tokens, "repos", len(cfg.Repos))
		}
		d.mu.Lock()
		d.broker, d.bComp, d.forges = b, bc, forges
		d.mu.Unlock()
		d.keys["broker"] = bkey
	} else {
		d.mu.Lock()
		d.forges = forges
		d.mu.Unlock()
	}

	// Mesh node.
	hasBroker := d.currentBroker() != nil
	mkey := keyOf(cfg.Device, cfg.Mesh, hasBroker)
	if mkey != d.keys["mesh"] {
		d.mu.Lock()
		oldComp := d.nComp
		d.node, d.nComp = nil, nil
		d.meshAt = time.Time{}
		d.mu.Unlock()
		oldComp.stop()
		if cfg.Mesh.Enabled {
			var src core.WorkSource
			if hasBroker {
				src = brokerProxy{d}
			}
			n := mesh.New(mesh.Options{
				Device: cfg.Device, Pool: cfg.Mesh.Pool, Port: cfg.Mesh.Port, Members: cfg.Mesh.Members,
				Tailscale: d.o.Tailscale, Broker: src, Version: d.o.Version,
				Status: func() any { return d.Status(context.Background()) },
				Log:    log,
			})
			nc := startComp(d.runCtx, log, "mesh", n.Run)
			d.mu.Lock()
			d.node, d.nComp = n, nc
			d.mu.Unlock()
		}
		d.keys["mesh"] = mkey
	}

	// Executors and worker.
	wkey := keyOf(cfg.Device, cfg.Labels, cfg.Job, cfg.StateDir, cfg.CacheDir)
	if wkey != d.keys["worker"] {
		d.mu.Lock()
		old, oldComp := d.worker, d.wComp
		d.mu.Unlock()
		if old != nil {
			oldComp.stop()
			sctx, cancel := context.WithTimeout(context.Background(), d.o.ShutdownTimeout)
			old.Shutdown(sctx)
			cancel()
		}
		binary := d.o.Binary
		if binary == nil {
			binary = NewBinary(BinaryOptions{Version: d.o.Version, CacheDir: cfg.CacheDir, Executable: d.o.Executable, Log: log})
		}
		execs, eps := d.o.Executors(cfg, binary, log)
		d.setProblems("executor", eps)
		w := dispatch.NewWorker(dispatch.WorkerOptions{
			Config:    cfg,
			Device:    cfg.Device,
			Executors: execs,
			Budget:    d.budget,
			Sources:   d.sources,
			StateDir:  cfg.StateDir,
			Log:       log,
			Now:       d.o.Now,
			RelayAddr: d.ControlAddress,
		})
		wc := startComp(d.runCtx, log, "worker", w.Run)
		d.mu.Lock()
		d.worker, d.wComp, d.execs = w, wc, execs
		d.mu.Unlock()
		d.keys["worker"] = wkey
	}
	log.Info("config applied", "device", cfg.Device, "repos", len(cfg.Repos), "mesh", cfg.Mesh.Enabled, "broker", hasBroker)
}

// sources is the worker's broker list: the local broker first, then the
// pool's brokers.
func (d *Daemon) sources() []core.WorkSource {
	var out []core.WorkSource
	if b := d.currentBroker(); b != nil {
		out = append(out, b)
	}
	if n := d.currentNode(); n != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out = append(out, n.Brokers(ctx)...)
		cancel()
	}
	return out
}

// shutdown stops the worker (running jobs keep running), then the broker
// (idle runners are deleted), then the mesh, all within ShutdownTimeout.
func (d *Daemon) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), d.o.ShutdownTimeout)
	defer cancel()
	d.mu.RLock()
	w, wc, b, bc, nc := d.worker, d.wComp, d.broker, d.bComp, d.nComp
	d.mu.RUnlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wc.stop()
		if w != nil {
			w.Shutdown(ctx)
		}
		bc.stop()
		if b != nil {
			b.Shutdown(ctx)
		}
		nc.stop()
	}()
	select {
	case <-done:
		d.log.Info("stopped cleanly")
	case <-ctx.Done():
		d.log.Error("shutdown took longer than " + d.o.ShutdownTimeout.String() + "; exiting anyway")
	}
}

// Reload re-reads the config and rebuilds what changed. A config that
// doesn't load or validate is refused and the running one stays.
func (d *Daemon) Reload(ctx context.Context) error {
	cfg, err := d.load()
	if err != nil {
		d.log.Error("reload refused", "error", err)
		return fmt.Errorf("%w; the running config stays", err)
	}
	if d.runCtx.Err() != nil {
		return errors.New("the daemon is stopping")
	}
	d.apply(cfg)
	d.restorePause()
	return nil
}

// ---------------------------------------------------------------- pause

type pauseFile struct {
	Until time.Time `json:"until"`
}

func (d *Daemon) pausePath() string { return filepath.Join(d.Config().StateDir, "pause.json") }

// Pause takes no new jobs until Resume, or for dur when dur > 0. The pause
// itself survives restarts (the worker saves it); so does its end time.
func (d *Daemon) Pause(ctx context.Context, reason string, dur time.Duration) error {
	d.mu.Lock()
	w := d.worker
	if dur > 0 {
		d.pauseUntil = d.o.Now().Add(dur)
	} else {
		d.pauseUntil = time.Time{}
	}
	until := d.pauseUntil
	d.mu.Unlock()
	if w == nil {
		return errors.New("the worker is not running; see `tentmaker doctor`")
	}
	if reason == "" {
		reason = "paused by hand"
	}
	w.Pause(reason)
	b, _ := json.Marshal(pauseFile{Until: until})
	if err := config.WriteAtomic(d.pausePath(), b, 0o644); err != nil {
		d.log.Error("pause end time not saved", "error", err)
	}
	d.log.Info("paused", "reason", reason, "until", until)
	return nil
}

// Resume undoes Pause.
func (d *Daemon) Resume(ctx context.Context) error {
	d.mu.Lock()
	w := d.worker
	d.pauseUntil = time.Time{}
	d.mu.Unlock()
	if w == nil {
		return errors.New("the worker is not running; see `tentmaker doctor`")
	}
	w.Resume()
	os.Remove(d.pausePath())
	d.log.Info("resumed")
	return nil
}

func (d *Daemon) restorePause() {
	b, err := os.ReadFile(d.pausePath())
	if err != nil {
		return
	}
	var p pauseFile
	if json.Unmarshal(b, &p) != nil || p.Until.IsZero() {
		return
	}
	d.mu.Lock()
	d.pauseUntil = p.Until
	d.mu.Unlock()
}

func (d *Daemon) pauseLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.mu.RLock()
		until := d.pauseUntil
		d.mu.RUnlock()
		if !until.IsZero() && !d.o.Now().Before(until) {
			_ = d.Resume(ctx)
		}
	}
}

// ---------------------------------------------------------------- update

func (d *Daemon) updateLoop(ctx context.Context) {
	wait := 5 * time.Minute
	if d.o.UpdateEvery < wait {
		wait = d.o.UpdateEvery
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = d.o.UpdateEvery
		cfg := d.Config()
		if !cfg.Update.Auto {
			continue
		}
		if err := d.autoUpdate(ctx, cfg); err != nil {
			d.log.Error("auto-update failed", "error", err)
			continue
		}
	}
}

// autoUpdate checks the channel and, when a newer release exists and no job
// runs, installs it and asks Run to exit so the service manager starts it.
func (d *Daemon) autoUpdate(ctx context.Context, cfg config.Config) error {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	rel, newer, err := d.o.Updater.Check(cctx, cfg.Update.Channel, d.o.Version)
	cancel()
	if err != nil || !newer {
		return err
	}
	if d.guard.isBad(rel.Version) {
		d.log.Debug("skipping an update that was rolled back before", "version", rel.Version)
		return nil
	}
	// Wait (up to the next check) for a moment with no job running.
	deadline := d.o.Now().Add(d.o.UpdateEvery)
	for d.runningJobs() > 0 {
		if d.o.Now().After(deadline) {
			return fmt.Errorf("update %s waited %s for jobs to finish; trying again later", rel.Version, d.o.UpdateEvery)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Minute):
		}
	}
	d.log.Info("updating", "from", d.o.Version, "to", rel.Version)
	dir, err := os.MkdirTemp(cfg.CacheDir, "update-")
	if err != nil {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			return err
		}
		if dir, err = os.MkdirTemp(cfg.CacheDir, "update-"); err != nil {
			return err
		}
	}
	defer os.RemoveAll(dir)
	dctx, cancel := context.WithTimeout(ctx, BinaryDownloadTimeout)
	defer cancel()
	bin, err := d.o.Updater.Download(dctx, rel, dir)
	if err != nil {
		return err
	}
	exe, err := d.o.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if d.runningJobs() > 0 {
		return errors.New("a job started during the download; trying again later")
	}
	if err := d.o.Updater.Apply(dctx, bin, exe, rel.Version); err != nil {
		return err
	}
	if err := d.guard.markPending(d.o.Version, rel.Version, d.o.Now()); err != nil {
		d.log.Warn("pending-update marker not written; a bad update won't roll back on its own", "error", err)
	}
	d.log.Info("update applied; exiting so the service manager starts the new binary", "version", rel.Version)
	d.exitOnce.Do(func() {
		d.exitErr = ErrRestart
		close(d.restart)
	})
	return nil
}

// checkPendingUpdate acts on the marker a self-update left. It returns the
// marker to watch while this binary proves itself, or an error wrapping
// ErrRestart after a rollback, so the service manager starts the previous
// binary.
func (d *Daemon) checkPendingUpdate() (*pendingUpdate, error) {
	p, ok := d.guard.pending()
	switch decideStart(p, ok, d.o.Version, d.o.Now()) {
	case verdictWatch:
		d.log.Info("running a new update; it has to get healthy", "from", p.From, "to", p.To, "within", UpdateGrace.String())
		return &p, nil
	case verdictRollback:
		return nil, d.rollback(p)
	case verdictStale:
		d.log.Info("clearing a pending-update marker for another version", "to", p.To, "running", d.o.Version)
		if err := d.guard.clearPending(); err != nil {
			d.log.Warn("pending-update marker not cleared", "error", err)
		}
	}
	return nil, nil
}

// watchUpdate clears the marker once this binary is healthy, or rolls back
// when the grace ends first.
func (d *Daemon) watchUpdate(ctx context.Context, p pendingUpdate) {
	for {
		if d.healthy() {
			if err := d.guard.clearPending(); err != nil {
				d.log.Warn("pending-update marker not cleared", "error", err)
			}
			d.log.Info("update healthy", "version", d.o.Version)
			return
		}
		if decideStart(p, true, d.o.Version, d.o.Now()) == verdictRollback {
			if err := d.rollback(p); err != nil {
				d.exitOnce.Do(func() {
					d.exitErr = err
					close(d.restart)
				})
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// healthy: the control API serves and the worker has ticked since start.
func (d *Daemon) healthy() bool {
	d.mu.RLock()
	w, addr, started := d.worker, d.ctlAddr, d.started
	d.mu.RUnlock()
	if addr == "" && d.o.ControlAddr != "-" {
		return false
	}
	return w != nil && !w.Status().LastTick.Before(started)
}

// rollback restores the previous binary, records p.To as bad and clears
// the marker. It returns an error wrapping ErrRestart when the previous
// binary is in place, and nil when it couldn't roll back (this binary
// keeps running).
func (d *Daemon) rollback(p pendingUpdate) error {
	if err := d.guard.markBad(p.To); err != nil {
		d.log.Warn("bad version not recorded; auto-update may try it again", "version", p.To, "error", err)
	}
	defer func() {
		if err := d.guard.clearPending(); err != nil {
			d.log.Warn("pending-update marker not cleared", "error", err)
		}
	}()
	exe, err := d.o.Executable()
	if err == nil {
		if r, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = r
		}
		err = d.o.Updater.Rollback(exe)
	}
	if err != nil {
		d.log.Error("the update did not get healthy and rollback failed; staying on it", "version", p.To, "error", err)
		return nil
	}
	d.log.Error("the update did not get healthy in time; rolled back, and auto-update will skip it",
		"bad", p.To, "back_to", p.From, "within", UpdateGrace.String())
	return fmt.Errorf("rolled back the update to %s: %w", p.To, ErrRestart)
}

func (d *Daemon) runningJobs() int {
	d.mu.RLock()
	w := d.worker
	d.mu.RUnlock()
	if w == nil {
		return 0
	}
	return len(w.Status().Running)
}

// ---------------------------------------------------------------- views

// Status is this device's state now.
func (d *Daemon) Status(ctx context.Context) Status {
	d.mu.RLock()
	cfg, cfgErr, w, b, until, started := d.cfg, d.cfgErr, d.worker, d.broker, d.pauseUntil, d.started
	var problems []core.Problem
	for _, k := range []string{"state", "control", "executor", "forges"} {
		problems = append(problems, d.problems[k]...)
	}
	d.mu.RUnlock()
	if cfgErr != nil {
		problems = append([]core.Problem{{Check: "config", What: cfgErr.Error(), Fix: "fix " + d.o.ConfigPath + ", then sudo tentmaker setup"}}, problems...)
	}
	s := Status{
		Version: d.o.Version, Device: cfg.Device, Service: "running", StartedAt: started,
		Warnings: []string{}, Problems: problems, IdleS: -1,
	}
	if cfg.Mesh.Enabled {
		s.Pool = cfg.Mesh.Pool
	}
	if w != nil {
		ws := w.Status()
		s.Worker = &ws
		s.Decision = ws.Decision
		s.Paused, s.PauseReason = ws.Paused, ws.PauseReason
		s.InUse = core.Resources{
			MemoryBytes: int64(len(ws.Running)) * int64(cfg.Job.Memory),
			CPUs:        float64(len(ws.Running)) * cfg.Job.CPUs,
		}
		if n := len(ws.LastErrors); n > 0 {
			s.LastError = ws.LastErrors[n-1].What
		}
	}
	if !until.IsZero() && s.Paused {
		u := until
		s.PauseUntil = &u
	}
	if b != nil {
		bs := b.Status()
		s.Broker = &bs
		if n := len(bs.LastErrors); n > 0 && s.LastError == "" {
			s.LastError = bs.LastErrors[n-1].What
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if p, err := d.o.Power.Power(pctx); err == nil {
		s.Power = &p
	}
	idle, ierr := d.o.Idle.IdleFor(pctx)
	if ierr == nil {
		s.IdleS = int64(idle.Seconds())
	}
	s.Why = Why(s.Decision, s.Power, idle, ierr == nil)
	s.Mesh = d.meshStatus(ctx, cfg)
	if s.Mesh.Warning != "" {
		s.Warnings = append(s.Warnings, s.Mesh.Warning)
	}
	s.Next = NextStep(s)
	rt := readRuntime(started, d.o.Now())
	s.Runtime = &rt
	return s
}

func (d *Daemon) meshStatus(ctx context.Context, cfg config.Config) MeshStatus {
	if !cfg.Mesh.Enabled {
		return MeshStatus{}
	}
	d.mu.RLock()
	at, st := d.meshAt, d.meshStat
	d.mu.RUnlock()
	if !at.IsZero() && d.o.Now().Sub(at) < meshCheckEvery {
		return st
	}
	st = MeshStatus{Enabled: true}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	self, err := d.o.Tailscale.Self(tctx)
	if err != nil {
		st.Warning = "tailscale is not up; running local only: " + firstLineOf(err.Error())
	} else {
		st.Up = true
		st.Self = firstNonEmpty(self.DNSName, self.HostName)
	}
	d.mu.Lock()
	d.meshAt, d.meshStat = d.o.Now(), st
	d.mu.Unlock()
	return st
}

// Pool is every device in the pool, this one included.
func (d *Daemon) Pool(ctx context.Context) ([]mesh.DeviceStatus, error) {
	n := d.currentNode()
	if n == nil {
		raw, _ := json.Marshal(d.Status(ctx))
		return []mesh.DeviceStatus{{Device: d.device(), Self: true, Broker: d.currentBroker() != nil, Version: d.o.Version, Status: raw}}, nil
	}
	return n.Pool(ctx)
}

// Doctor runs every check with the daemon's live components.
func (d *Daemon) Doctor(ctx context.Context) Report {
	d.mu.RLock()
	cfg, cfgErr, execs, forges := d.cfg, d.cfgErr, d.execs, d.forges
	var extra []core.Problem
	for _, k := range []string{"state", "control"} {
		extra = append(extra, d.problems[k]...)
	}
	eps := d.problems["executor"]
	d.mu.RUnlock()
	_, statErr := os.Stat(d.o.ConfigPath)
	in := DoctorInput{
		Version: d.o.Version, Channel: cfg.Update.Channel, ConfigPath: d.o.ConfigPath,
		ConfigExists: statErr == nil, ConfigErr: cfgErr, Config: cfg,
		Executors: execs, ExecProblems: eps, Forges: forges,
		Service: d.o.Service, Probe: d.o.Probe(cfg.StateDir), Update: d.o.Updater.Check, Extra: extra,
	}
	if d.o.Service == nil {
		in.ServiceNote = "the service is running (pid " + fmt.Sprint(os.Getpid()) + ")"
	}
	if cfg.Mesh.Enabled {
		in.Tailscale = d.o.Tailscale
	}
	return Doctor(ctx, in)
}

// Handler is the daemon as the loopback control API serves it.
func (d *Daemon) Handler() control.Handler { return ctlHandler{d} }

type ctlHandler struct{ d *Daemon }

func (h ctlHandler) Status(ctx context.Context) (any, error) { return h.d.Status(ctx), nil }
func (h ctlHandler) Pool(ctx context.Context) (any, error)   { return h.d.Pool(ctx) }
func (h ctlHandler) Doctor(ctx context.Context) (any, error) { return h.d.Doctor(ctx), nil }
func (h ctlHandler) Pause(ctx context.Context, reason string, dur time.Duration) error {
	return h.d.Pause(ctx, reason, dur)
}
func (h ctlHandler) Resume(ctx context.Context) error { return h.d.Resume(ctx) }
func (h ctlHandler) Reload(ctx context.Context) error { return h.d.Reload(ctx) }

// Relay streams a running check job's source archive (control.Relayer).
func (h ctlHandler) Relay(ctx context.Context, id string, w io.Writer) error {
	h.d.mu.RLock()
	wk := h.d.worker
	h.d.mu.RUnlock()
	if wk == nil {
		return core.ErrNotFound
	}
	return wk.Relay(ctx, id, w)
}

var (
	_ control.Relayer    = ctlHandler{}
	_ core.ArchiveSource = brokerProxy{}
)
