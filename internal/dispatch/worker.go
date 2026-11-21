package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// WorkerOptions configures a Worker.
type WorkerOptions struct {
	Config    config.Config
	Device    string
	Executors []core.Executor
	Budget    core.Budgeter
	// Sources lists the brokers to ask, local broker first, then mesh brokers.
	Sources func() []core.WorkSource
	// RelayAddr is the relay listener's host:port, which serves the archive
	// relay to check jobs (GET /v1/relay/<id>). Nil means DefaultRelayAddr;
	// an empty answer means no relay, so check offers are declined.
	RelayAddr func() string
	StateDir  string
	Log       *slog.Logger
	Now       func() time.Time
}

// Worker is the device side of dispatch: it asks brokers for work and runs
// it with its executors.
type Worker struct {
	o         WorkerOptions
	log       *slog.Logger
	now       func() time.Time
	device    string
	labels    []string
	limits    core.Resources
	statePath string

	tickMu sync.Mutex // one Tick (or Shutdown) at a time
	saveMu sync.Mutex

	mu           sync.Mutex // everything below
	st           workerState
	execs        []*execInfo
	sources      map[string]*sourceHealth
	decision     core.Decision
	lastResetTry time.Time
	lastReset    time.Time
	started      bool
	shutting     bool
	warned       map[string]bool
	errs         errorLog
	lastSaved    []byte
	lastTick     time.Time
	relays       map[string]*relayEntry // relay id -> archive; memory only
	cleanErrs    map[string]string      // leftover runner -> why its Clean failed; memory only
	holds        int                    // Hold calls not yet released; memory only

	starts sync.WaitGroup // executor Starts in flight
}

// relayEntry lets one running check job fetch its source archive from the
// broker that granted it. The random id is the only capability.
type relayEntry struct {
	source  core.ArchiveSource
	ref     string
	runner  string
	expires time.Time
}

type execInfo struct {
	ex           core.Executor
	name         string
	reconciled   bool
	failures     int
	blockedUntil time.Time
	closed       bool
	lastErr      string
}

type sourceHealth struct {
	LastOK    time.Time
	LastErr   string
	LastErrAt time.Time
	Failures  int
}

// workerState is state.json. It holds no secret.
type workerState struct {
	Version     int                `json:"version"`
	Jobs        []*jobRec          `json:"jobs"`
	Unacked     map[string]*report `json:"unacked"`
	Leftovers   map[string]string  `json:"leftovers,omitempty"` // runner -> executor; Clean failed, retried
	Dirty       bool               `json:"dirty"`
	Quarantined map[string]string  `json:"quarantined,omitempty"` // source -> why
	Paused      bool               `json:"paused,omitempty"`
	PauseReason string             `json:"pause_reason,omitempty"`
}

type report struct {
	Ended   []string           `json:"ended,omitempty"`
	Results []core.CheckResult `json:"results,omitempty"`
	Since   time.Time          `json:"since"`
}

type jobRec struct {
	Name     string         `json:"name"`
	Source   string         `json:"source"`
	Kind     string         `json:"kind"`
	Executor string         `json:"executor"`
	Forge    string         `json:"forge"`
	Repo     string         `json:"repo"`
	Job      string         `json:"job"`
	JobID    int64          `json:"job_id,omitempty"`
	Platform core.Platform  `json:"platform"`
	Started  time.Time      `json:"started"`
	Limits   core.Resources `json:"limits"`
	// Stopping is why the job is being stopped, set while its Stop has not
	// been confirmed. Such a job may still run, so it stays counted and
	// blocks its executor; pollJobs retries the stop.
	Stopping string `json:"stopping,omitempty"`
	StopErr  string `json:"stop_err,omitempty"`

	// starting is set while the executor's Start runs, off the tick, so a
	// slow boot never stops the worker's heartbeat. The job holds its slot
	// meanwhile but is not reported running. Not saved: after a restart
	// State decides.
	starting bool
}

// NewWorker builds a worker and loads state.json. It touches no executor;
// the first Tick reconciles.
func NewWorker(o WorkerOptions) *Worker {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	device := o.Device
	if device == "" {
		device = o.Config.Device
	}
	if device == "" {
		device = "device"
	}
	w := &Worker{
		o:       o,
		log:     o.Log.With("component", "worker"),
		now:     o.Now,
		device:  device,
		labels:  WorkerLabels(device, o.Config.Labels),
		limits:  core.Resources{MemoryBytes: int64(o.Config.Job.Memory), CPUs: o.Config.Job.CPUs},
		sources: map[string]*sourceHealth{},
		warned:  map[string]bool{},
		relays:  map[string]*relayEntry{},
	}
	if w.o.RelayAddr == nil {
		w.o.RelayAddr = func() string { return DefaultRelayAddr }
	}
	for _, ex := range o.Executors {
		w.execs = append(w.execs, &execInfo{ex: ex, name: ex.Name()})
	}
	if o.StateDir != "" {
		w.statePath = filepath.Join(o.StateDir, "state.json")
	}
	w.load()
	return w
}

// ---------------------------------------------------------------- loop

// Run ticks every 2 s until ctx ends. The daemon calls Shutdown afterwards.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(workerTick)
	defer t.Stop()
	for {
		if err := w.Tick(ctx); err != nil {
			w.log.Error("worker tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Tick runs one pass: budget, evict, job states, pool limits, reset, then
// report to and ask every source, start offers, save.
func (w *Worker) Tick(ctx context.Context) error {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	w.reconcile(ctx)
	now := w.now()
	d := w.decide(ctx, now)
	if d.Evict {
		w.evictAll(ctx, "evicted: "+strings.Join(d.Reasons, "; "))
	}
	w.pollJobs(ctx, now)
	w.retryLeftovers(ctx)
	w.applyPoolLimits(ctx, d)
	w.maybeReset(ctx, now)
	w.exchange(ctx, now, d, true)
	w.pruneUnacked(now)
	if err := w.save(); err != nil {
		return err
	}
	w.mu.Lock()
	w.lastTick = w.now()
	w.mu.Unlock()
	return nil
}

func (w *Worker) decide(ctx context.Context, now time.Time) core.Decision {
	w.mu.Lock()
	running := len(w.st.Jobs)
	w.mu.Unlock()
	var d core.Decision
	if w.o.Budget == nil {
		d = core.Decision{Profile: "off", Reasons: []string{"no budget configured"}}
	} else {
		d = w.o.Budget.Decide(ctx, now, running)
	}
	w.mu.Lock()
	changed := d.Admit != w.decision.Admit || d.Evict != w.decision.Evict || d.Slots != w.decision.Slots ||
		strings.Join(d.Reasons, "|") != strings.Join(w.decision.Reasons, "|")
	w.decision = d
	w.mu.Unlock()
	if changed {
		w.log.Info("budget decision", "profile", d.Profile, "slots", d.Slots, "admit", d.Admit, "evict", d.Evict,
			"reasons", strings.Join(d.Reasons, "; "))
	}
	return d
}

// reconcile runs once per executor (retried until List works): anything the
// executor has that state.json doesn't know is stopped and cleaned.
func (w *Worker) reconcile(ctx context.Context) {
	w.mu.Lock()
	if !w.started {
		w.started = true
		w.st.Dirty = true // a restart never starts clean: reset at the first idle moment
	}
	var todo []*execInfo
	for _, e := range w.execs {
		if !e.reconciled {
			todo = append(todo, e)
		}
	}
	w.mu.Unlock()
	for _, e := range todo {
		lctx, cancel := context.WithTimeout(ctx, execTimeout)
		names, err := e.ex.List(lctx)
		cancel()
		if err != nil {
			w.noteErr(e.name, "list runners", err)
			w.mu.Lock()
			e.lastErr = "can't list runners: " + err.Error()
			w.mu.Unlock()
			continue
		}
		w.mu.Lock()
		known := map[string]bool{}
		for _, j := range w.st.Jobs {
			if j.Executor == e.name {
				known[j.Name] = true
			}
		}
		w.mu.Unlock()
		for _, n := range names {
			if known[n] {
				continue
			}
			w.log.Warn("runner left over from before; stopping and cleaning it", "executor", e.name, "runner", n)
			w.stopAndClean(ctx, e, n)
		}
		w.mu.Lock()
		e.reconciled, e.lastErr = true, ""
		w.mu.Unlock()
	}
}

// stopAndClean stops and cleans a runner state.json doesn't track. Until
// both work it stays a leftover, which blocks its executor and is retried.
func (w *Worker) stopAndClean(ctx context.Context, e *execInfo, name string) {
	err := w.stopped(ctx, e, name)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, stopTimeout)
		err = e.ex.Clean(cctx, name)
		cancel()
		if err != nil {
			err = fmt.Errorf("its clean failed: %v", err)
		}
	} else {
		err = fmt.Errorf("it did not stop: %v", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.errs.add(w.now(), e.name, name+": "+err.Error())
		if _, ok := w.st.Leftovers[name]; !ok {
			w.log.Warn("runner left behind; retrying", "executor", e.name, "runner", name, "error", err)
		}
		w.leftoverLocked(name, e.name, err.Error())
		w.st.Dirty = true
	} else {
		delete(w.st.Leftovers, name)
		delete(w.cleanErrs, name)
	}
}

func (w *Worker) leftoverLocked(name, exName, why string) {
	if w.st.Leftovers == nil {
		w.st.Leftovers = map[string]string{}
	}
	if w.cleanErrs == nil {
		w.cleanErrs = map[string]string{}
	}
	w.st.Leftovers[name] = exName
	w.cleanErrs[name] = why
}

// stopped stops a runner and returns nil once the executor says it no
// longer runs: Stop worked, or it failed but State reads exited or missing.
func (w *Worker) stopped(ctx context.Context, e *execInfo, name string) error {
	sctx, cancel := context.WithTimeout(ctx, stopTimeout)
	err := e.ex.Stop(sctx, name)
	cancel()
	if err == nil {
		return nil
	}
	w.noteErr(e.name, "stop "+name, err)
	sctx, cancel = context.WithTimeout(ctx, execTimeout)
	st, serr := e.ex.State(sctx, name)
	cancel()
	if serr == nil && (st == core.ExecExited || st == core.ExecMissing) {
		return nil
	}
	return err
}

func (w *Worker) execByName(name string) *execInfo {
	for _, e := range w.execs {
		if e.name == name {
			return e
		}
	}
	return nil
}

// pollJobs reads every job's state. Exited or missing jobs are finished; an
// unreadable or unknown state counts as running.
func (w *Worker) pollJobs(ctx context.Context, now time.Time) {
	w.mu.Lock()
	jobs := append([]*jobRec(nil), w.st.Jobs...)
	w.mu.Unlock()
	for _, j := range jobs {
		e := w.execByName(j.Executor)
		if e == nil {
			w.log.Warn("job's executor is gone; reporting it ended", "runner", j.Name, "executor", j.Executor)
			w.finish(ctx, nil, j, "the job's executor is no longer configured")
			continue
		}
		w.mu.Lock()
		starting := j.starting
		w.mu.Unlock()
		if starting {
			continue // the start finishes off the tick
		}
		if j.Stopping != "" {
			w.stopJob(ctx, j, j.Stopping)
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, execTimeout)
		st, err := e.ex.State(sctx, j.Name)
		cancel()
		if err != nil || st == core.ExecUnknown {
			if !w.warned[j.Name] {
				w.warned[j.Name] = true
				w.log.Warn("runner state unknown; counting it as running", "runner", j.Name, "error", err)
			}
			continue
		}
		delete(w.warned, j.Name)
		if st == core.ExecRunning {
			maxd := w.o.Config.Job.MaxDuration.D()
			if maxd > 0 && now.Sub(j.Started) > maxd+overrunGrace {
				w.log.Warn("job ran past max_duration; stopping it", "runner", j.Name, "repo", j.Repo)
				w.stopJob(ctx, j, "the job ran past max_duration")
			}
			continue
		}
		w.log.Info("job ended", "runner", j.Name, "repo", j.Repo, "job", j.Job, "state", string(st),
			"seconds", int(now.Sub(j.Started).Seconds()))
		w.finish(ctx, e, j, "")
	}
}

// finish reads a check job's result, cleans the runner and queues the
// ended report for the source that granted it.
func (w *Worker) finish(ctx context.Context, e *execInfo, j *jobRec, why string) {
	var res *core.CheckResult
	if j.Kind == core.KindCheck {
		r := core.CheckResult{State: "error", Description: why}
		if e != nil {
			rctx, cancel := context.WithTimeout(ctx, execTimeout)
			got, err := e.ex.CheckResult(rctx, j.Name)
			cancel()
			if err == nil && why == "" {
				r = got
			} else if r.Description == "" {
				r.Description = "tentmaker could not read the job's result"
			}
		}
		r.Runner, r.Repo = j.Name, j.Repo
		res = &r
	}
	if e != nil {
		cctx, cancel := context.WithTimeout(ctx, stopTimeout)
		err := e.ex.Clean(cctx, j.Name)
		cancel()
		if err != nil {
			w.log.Warn("clean failed; retrying", "executor", e.name, "runner", j.Name, "error", err)
			w.mu.Lock()
			w.leftoverLocked(j.Name, e.name, "its clean failed: "+err.Error())
			w.errs.add(w.now(), e.name, "clean "+j.Name+": "+err.Error())
			w.mu.Unlock()
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropRelaysLocked(j.Name)
	for i, x := range w.st.Jobs {
		if x == j {
			w.st.Jobs = append(w.st.Jobs[:i], w.st.Jobs[i+1:]...)
			break
		}
	}
	rep := w.reportLocked(j.Source)
	rep.Ended = append(rep.Ended, j.Name)
	if res != nil {
		rep.Results = append(rep.Results, *res)
	}
	w.st.Dirty = true
}

func (w *Worker) reportLocked(source string) *report {
	if w.st.Unacked == nil {
		w.st.Unacked = map[string]*report{}
	}
	r := w.st.Unacked[source]
	if r == nil {
		r = &report{Since: w.now()}
		w.st.Unacked[source] = r
	}
	return r
}

// evictAll stops every job now. Local stops need no network.
func (w *Worker) evictAll(ctx context.Context, why string) {
	w.mu.Lock()
	jobs := append([]*jobRec(nil), w.st.Jobs...)
	w.mu.Unlock()
	if len(jobs) == 0 {
		return
	}
	w.log.Warn("evicting every job", "jobs", len(jobs), "why", why)
	for _, j := range jobs {
		w.stopJob(ctx, j, why)
	}
}

// stopJob stops j and finishes it once its executor confirms it no longer
// runs. A job whose stop fails may still use its slot and memory, so it stays
// counted, its executor takes nothing new, and pollJobs retries the stop.
func (w *Worker) stopJob(ctx context.Context, j *jobRec, why string) {
	if why == "" {
		why = "stopped"
	}
	w.mu.Lock()
	if j.starting {
		// Stopped once its Start returns; until then it holds its slot.
		j.Stopping, j.StopErr = why, "it is still starting"
		w.st.Dirty = true
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	e := w.execByName(j.Executor)
	if e == nil {
		w.finish(ctx, nil, j, why)
		return
	}
	if err := w.stopped(ctx, e, j.Name); err != nil {
		w.mu.Lock()
		first := j.Stopping == ""
		j.Stopping, j.StopErr = why, err.Error()
		w.st.Dirty = true
		w.mu.Unlock()
		if first {
			w.log.Warn("stop failed; the job stays counted and its executor takes nothing new until it stops",
				"executor", e.name, "runner", j.Name, "error", err)
		}
		return
	}
	w.finish(ctx, e, j, why)
}

func (w *Worker) retryLeftovers(ctx context.Context) {
	w.mu.Lock()
	left := map[string]string{}
	for k, v := range w.st.Leftovers {
		left[k] = v
	}
	w.mu.Unlock()
	for name, exName := range left {
		e := w.execByName(exName)
		if e == nil {
			w.mu.Lock()
			delete(w.st.Leftovers, name)
			w.mu.Unlock()
			continue
		}
		w.stopAndClean(ctx, e, name)
	}
}

func (w *Worker) inUse(exName string) (core.Resources, int) {
	var r core.Resources
	n := 0
	for _, j := range w.st.Jobs {
		if j.Executor == exName {
			r = r.Add(j.Limits)
			n++
		}
	}
	return r, n
}

func (w *Worker) applyPoolLimits(ctx context.Context, d core.Decision) {
	for _, e := range w.execs {
		w.mu.Lock()
		use, _ := w.inUse(e.name)
		w.mu.Unlock()
		pctx, cancel := context.WithTimeout(ctx, execTimeout)
		err := e.ex.SetPoolLimit(pctx, d.Limit, use)
		cancel()
		if err != nil {
			w.noteErr(e.name, "set pool limit", err)
		}
	}
}

// maybeReset wipes CI state when nothing runs and the node is dirty, at most
// once a minute. A successful reset lifts every quarantine.
func (w *Worker) maybeReset(ctx context.Context, now time.Time) {
	w.mu.Lock()
	due := len(w.st.Jobs) == 0 && w.st.Dirty && (w.lastResetTry.IsZero() || now.Sub(w.lastResetTry) >= resetEvery)
	if due {
		w.lastResetTry = now
	}
	w.mu.Unlock()
	if !due {
		return
	}
	ok := true
	for _, e := range w.execs {
		rctx, cancel := context.WithTimeout(ctx, resetTimeout)
		err := e.ex.Reset(rctx)
		cancel()
		if err != nil {
			ok = false
			w.noteErr(e.name, "reset", err)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !ok {
		w.log.Warn("node reset failed; retrying in a minute")
		return
	}
	lifted := len(w.st.Quarantined)
	w.st.Dirty = false
	w.st.Quarantined = nil
	w.st.Leftovers = nil
	w.cleanErrs = nil
	w.lastReset = now
	w.log.Info("node reset", "quarantines_lifted", lifted)
}

// usable reports whether an executor may start a job now.
func (w *Worker) usableLocked(e *execInfo, now time.Time) bool {
	return e.reconciled && !e.closed && !now.Before(e.blockedUntil) && len(w.stuckLocked(e.name)) == 0
}

// stuckLocked says which of an executor's runners may still hold resources
// after a failed stop or clean. While any do, it takes nothing new.
func (w *Worker) stuckLocked(exName string) []string {
	var out []string
	for _, j := range w.st.Jobs {
		if j.Executor == exName && j.Stopping != "" {
			out = append(out, fmt.Sprintf("runner %s did not stop: %s", j.Name, j.StopErr))
		}
	}
	var left []string
	for name, ex := range w.st.Leftovers {
		if ex == exName {
			left = append(left, name)
		}
	}
	sort.Strings(left)
	for _, name := range left {
		why := w.cleanErrs[name]
		if why == "" {
			why = "it was left behind"
		}
		out = append(out, fmt.Sprintf("runner %s was not cleaned up: %s", name, why))
	}
	return out
}

// Problems lists what keeps an executor from taking jobs that the worker
// knows of: a runner that did not stop or was not cleaned up.
func (w *Worker) Problems() []core.Problem {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []core.Problem
	for _, e := range w.execs {
		for _, why := range w.stuckLocked(e.name) {
			out = append(out, core.Problem{Check: "worker",
				What: fmt.Sprintf("%s takes no new jobs: %s", e.name, why),
				Fix:  "Tentmaker retries every few seconds; if this persists, check the sandbox and the last errors in tentmaker status, then restart the service"})
		}
	}
	return out
}

func (w *Worker) capacityLocked(e *execInfo) int {
	_, n := w.inUse(e.name)
	if m := e.ex.MaxSlots(); m > 0 {
		if n >= m {
			return 0
		}
		return m - n
	}
	return math.MaxInt32
}

func (w *Worker) freeLocked(d core.Decision, now time.Time) int {
	if w.st.Paused || w.shutting || w.holds > 0 || !d.Admit {
		return 0
	}
	free := d.Slots - len(w.st.Jobs)
	room := 0
	for _, e := range w.execs {
		if w.usableLocked(e, now) {
			room += w.capacityLocked(e)
			if room > math.MaxInt32/2 {
				room = math.MaxInt32 / 2
			}
		}
	}
	if room < free {
		free = room
	}
	if free < 0 {
		free = 0
	}
	return free
}

func samePlatform(a, b core.Platform) bool {
	return strings.EqualFold(a.OS, b.OS) && strings.EqualFold(a.Arch, b.Arch)
}

func (w *Worker) platformsLocked(now time.Time) []core.Platform {
	var out []core.Platform
	for _, e := range w.execs {
		if !w.usableLocked(e, now) || w.capacityLocked(e) == 0 {
			continue
		}
		for _, p := range e.ex.Platforms() {
			dup := false
			for _, q := range out {
				if samePlatform(p, q) {
					dup = true
				}
			}
			if !dup {
				out = append(out, p)
			}
		}
	}
	return out
}

func (w *Worker) pickExecutorLocked(p core.Platform, now time.Time) *execInfo {
	for _, e := range w.execs {
		if !w.usableLocked(e, now) || w.capacityLocked(e) == 0 {
			continue
		}
		for _, q := range e.ex.Platforms() {
			if samePlatform(p, q) {
				return e
			}
		}
	}
	return nil
}

// exchange reports to every source and, when ask is set, takes offers.
func (w *Worker) exchange(ctx context.Context, now time.Time, d core.Decision, ask bool) {
	if w.o.Sources == nil {
		return
	}
	for _, s := range w.o.Sources() {
		if s == nil {
			continue
		}
		name := s.Name()
		w.mu.Lock()
		quarantined := w.st.Quarantined[name] != ""
		free := 0
		if ask && !quarantined {
			free = w.freeLocked(d, now)
		}
		platforms := w.platformsLocked(now)
		if len(platforms) == 0 {
			free = 0
		}
		var running []string
		for _, j := range w.st.Jobs {
			if j.Source == name && !j.starting {
				running = append(running, j.Name)
			}
		}
		var sent report
		if r := w.st.Unacked[name]; r != nil {
			sent.Ended = append([]string(nil), r.Ended...)
			sent.Results = append([]core.CheckResult(nil), fitResults(r.Results)...)
		}
		w.mu.Unlock()

		req := core.WorkRequest{Device: w.device, Labels: w.labels, Platforms: platforms, Free: free,
			Limits: w.limits, Running: running, Ended: sent.Ended, Results: sent.Results}
		wctx, cancel := context.WithTimeout(ctx, workTimeout)
		resp, err := s.Work(wctx, req)
		cancel()
		w.mu.Lock()
		h := w.sources[name]
		if h == nil {
			h = &sourceHealth{}
			w.sources[name] = h
		}
		if err != nil {
			h.Failures++
			h.LastErr, h.LastErrAt = err.Error(), now
			w.errs.add(now, "source "+name, err.Error())
			w.mu.Unlock()
			w.log.Warn("work request failed", "source", name, "failures", h.Failures, "error", err)
			continue
		}
		h.Failures, h.LastOK, h.LastErr = 0, now, ""
		w.ackLocked(name, sent)
		if resp.Quarantine && !quarantined {
			if w.st.Quarantined == nil {
				w.st.Quarantined = map[string]string{}
			}
			w.st.Quarantined[name] = "quarantined by " + name
			w.st.Dirty = true
			w.log.Warn("source quarantined this device; taking nothing from it until a reset", "source", name)
		}
		var stops []*jobRec
		for _, n := range resp.Stop {
			for _, j := range w.st.Jobs {
				if j.Name == n && j.Source == name {
					stops = append(stops, j)
				}
			}
		}
		w.mu.Unlock()
		for _, j := range stops {
			w.log.Info("broker asked to stop a runner", "source", name, "runner", j.Name, "repo", j.Repo)
			w.stopJob(ctx, j, "stopped by the broker")
		}
		started := 0
		for _, off := range resp.Offers {
			if started >= free || resp.Quarantine {
				w.decline(name, off, "no room for it")
				continue
			}
			if w.start(ctx, s, off) {
				started++
			}
		}
	}
}

// resultBudget bounds the check results one work request carries, in JSON
// bytes. The mesh refuses a body over 1 MiB, and a result's 64 KiB log tail
// can escape to 384 KiB, so the first result always fits; the rest wait for
// the next exchange, since ackLocked acks only what was sent.
const resultBudget = 512 << 10

// fitResults returns the leading results that fit resultBudget, and always
// at least one.
func fitResults(rs []core.CheckResult) []core.CheckResult {
	size := 0
	for i, x := range rs {
		b, _ := json.Marshal(x)
		size += len(b)
		if i > 0 && size > resultBudget {
			return rs[:i]
		}
	}
	return rs
}

func (w *Worker) ackLocked(source string, sent report) {
	r := w.st.Unacked[source]
	if r == nil {
		return
	}
	acked := map[string]bool{}
	for _, n := range sent.Ended {
		acked[n] = true
	}
	var ended []string
	for _, n := range r.Ended {
		if !acked[n] {
			ended = append(ended, n)
		}
	}
	ackedRes := map[string]bool{}
	for _, x := range sent.Results {
		ackedRes[x.Runner] = true
	}
	var results []core.CheckResult
	for _, x := range r.Results {
		if !ackedRes[x.Runner] {
			results = append(results, x)
		}
	}
	r.Ended, r.Results = ended, results
	if len(ended) == 0 && len(results) == 0 {
		delete(w.st.Unacked, source)
	}
}

// decline reports an offer it won't start as ended, so the broker deletes
// the runner it registered.
func (w *Worker) decline(source string, off core.WorkOffer, why string) {
	name := off.Grant.RunnerName
	w.log.Warn("offer declined", "source", source, "runner", name, "repo", off.Job.Repo, "why", why)
	if name == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, j := range w.st.Jobs {
		if j.Name == name {
			return // never report a running job as ended
		}
	}
	rep := w.reportLocked(source)
	rep.Ended = append(rep.Ended, name)
}

// start runs one offer. state.json is written before the runner exists, so
// a crash can't lose track of it.
func (w *Worker) start(ctx context.Context, src core.WorkSource, off core.WorkOffer) bool {
	source := src.Name()
	name := off.Grant.RunnerName
	if !core.NamePattern.MatchString(name) {
		w.log.Warn("offer with a bad runner name refused", "source", source)
		return false
	}
	now := w.now()
	w.mu.Lock()
	for _, j := range w.st.Jobs {
		if j.Name == name {
			w.mu.Unlock()
			w.log.Warn("offer for a runner already running ignored", "source", source, "runner", name)
			return false
		}
	}
	e := w.pickExecutorLocked(off.Platform, now)
	w.mu.Unlock()
	if e == nil {
		w.decline(source, off, "no executor free for "+off.Platform.String())
		return false
	}
	secret := off.Secret
	if len(secret) == 0 {
		secret = off.Grant.Secret
	}
	if len(secret) == 0 {
		w.decline(source, off, "offer without a grant")
		return false
	}
	kind := core.KindRunner
	if isCheckOffer(off) {
		kind = core.KindCheck
	}
	if kind == core.KindCheck {
		relayed, err := w.relayBundle(src, name, secret, now)
		if err != nil {
			w.decline(source, off, err.Error())
			return false
		}
		secret = relayed
	}
	g := off.Grant
	g.Secret = secret
	tw := int(math.Floor(w.o.Config.Job.CPUs))
	if tw < 1 {
		tw = 1
	}
	spec := core.ExecSpec{Kind: kind, Name: name, Grant: g, Platform: off.Platform, Limits: w.limits,
		MaxDuration: w.o.Config.Job.MaxDuration.D(), TestWorkers: tw}
	rec := &jobRec{Name: name, Source: source, Kind: kind, Executor: e.name, Forge: off.Job.Forge, Repo: off.Job.Repo,
		Job: off.Job.Name, JobID: off.Job.ID, Platform: off.Platform, Started: now, Limits: w.limits, starting: true}
	w.mu.Lock()
	w.st.Jobs = append(w.st.Jobs, rec)
	w.st.Dirty = true
	w.mu.Unlock()
	if err := w.save(); err != nil {
		w.log.Error("state not saved before start", "error", err)
	}
	w.starts.Add(1)
	go func() {
		defer w.starts.Done()
		w.boot(ctx, e, rec, spec)
	}()
	return true
}

// boot runs the executor's Start, which can take minutes (a VM boot, an
// image pull), off the tick so the worker keeps reporting to its sources.
func (w *Worker) boot(ctx context.Context, e *execInfo, rec *jobRec, spec core.ExecSpec) {
	name, source, kind := rec.Name, rec.Source, rec.Kind
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	err := e.ex.Start(sctx, spec)
	cancel()
	now := w.now()
	if err != nil {
		w.log.Warn("start failed", "executor", e.name, "runner", name, "repo", rec.Repo, "error", err)
		w.mu.Lock()
		for i, x := range w.st.Jobs {
			if x == rec {
				w.st.Jobs = append(w.st.Jobs[:i], w.st.Jobs[i+1:]...)
				break
			}
		}
		w.dropRelaysLocked(name)
		w.mu.Unlock()
		w.stopAndClean(ctx, e, name)
		w.mu.Lock()
		rep := w.reportLocked(source)
		rep.Ended = append(rep.Ended, name)
		if kind == core.KindCheck {
			rep.Results = append(rep.Results, core.CheckResult{Runner: name, Repo: rec.Repo, State: "error",
				Description: "could not start on " + w.device})
		}
		w.bootFailureLocked(e, now, err)
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	rec.starting = false
	e.failures, e.blockedUntil, e.lastErr = 0, time.Time{}, ""
	w.mu.Unlock()
	w.log.Info("job started", "source", source, "executor", e.name, "runner", name, "kind", kind,
		"repo", rec.Repo, "job", rec.Job, "platform", rec.Platform.String())
}

func (w *Worker) bootFailureLocked(e *execInfo, now time.Time, err error) {
	e.failures++
	delay := bootBackoff << (e.failures - 1)
	if delay > bootBackoffMax || delay <= 0 {
		delay = bootBackoffMax
	}
	e.blockedUntil = now.Add(delay)
	e.lastErr = err.Error()
	w.errs.add(now, e.name, "start: "+err.Error())
	if e.failures >= bootCloseAfter {
		e.closed = true
		w.log.Error("executor closed after failed starts in a row; restart the daemon to retry",
			"executor", e.name, "failures", e.failures)
		return
	}
	w.log.Warn("start failure backoff", "executor", e.name, "failures", e.failures, "retry_in", delay.String())
}

func (w *Worker) pruneUnacked(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for src, r := range w.st.Unacked {
		if now.Sub(r.Since) > unackedMaxAge {
			w.log.Warn("dropping reports a source never took", "source", src, "ended", len(r.Ended), "results", len(r.Results))
			delete(w.st.Unacked, src)
		}
	}
}

func (w *Worker) noteErr(where, what string, err error) {
	w.log.Warn("call failed", "where", where, "call", what, "error", err)
	w.mu.Lock()
	w.errs.add(w.now(), where, what+": "+err.Error())
	w.mu.Unlock()
}

// ---------------------------------------------------------------- control

// Pause takes no new jobs until Resume; running jobs go on.
func (w *Worker) Pause(reason string) {
	w.mu.Lock()
	w.st.Paused, w.st.PauseReason = true, reason
	w.mu.Unlock()
	w.log.Info("paused", "reason", reason)
	if err := w.save(); err != nil {
		w.log.Error("state not saved", "error", err)
	}
}

// Hold takes no new jobs until release runs, and returns how many jobs
// were counted when it took hold. The count and the hold are taken under
// one lock, so a zero count means no job can start until release: the
// daemon holds across an update's swap and restart, where checking for
// zero jobs first left a window for a job to start (review r1 partB
// sol-F5). Unlike Pause it is not saved, so a restart drops it.
func (w *Worker) Hold() (jobs int, release func()) {
	w.mu.Lock()
	w.holds++
	jobs = len(w.st.Jobs)
	w.mu.Unlock()
	var once sync.Once
	return jobs, func() {
		once.Do(func() {
			w.mu.Lock()
			w.holds--
			w.mu.Unlock()
		})
	}
}

// Resume undoes Pause.
func (w *Worker) Resume() {
	w.mu.Lock()
	w.st.Paused, w.st.PauseReason = false, ""
	w.mu.Unlock()
	w.log.Info("resumed")
	if err := w.save(); err != nil {
		w.log.Error("state not saved", "error", err)
	}
}

// Shutdown stops asking for work and sends a last report. Running jobs keep
// running (their sandboxes outlive the daemon) and are adopted on restart.
func (w *Worker) Shutdown(ctx context.Context) {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	w.mu.Lock()
	w.shutting = true
	d := w.decision
	w.mu.Unlock()
	done := make(chan struct{})
	go func() {
		w.starts.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	now := w.now()
	w.pollJobs(ctx, now)
	w.exchange(ctx, now, d, false)
	if err := w.save(); err != nil {
		w.log.Error("state not saved", "error", err)
	}
	w.mu.Lock()
	n := len(w.st.Jobs)
	w.mu.Unlock()
	w.log.Info("worker stopped", "jobs_left_running", n)
}

// ---------------------------------------------------------------- state

func (w *Worker) load() {
	w.st = workerState{Version: 1, Unacked: map[string]*report{}}
	if w.statePath == "" {
		return
	}
	data, err := os.ReadFile(w.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		w.log.Error("worker state unreadable; starting empty (leftovers will be stopped)", "error", err)
		return
	}
	var st workerState
	if err := json.Unmarshal(data, &st); err != nil {
		w.log.Error("worker state corrupt; starting empty (leftovers will be stopped)", "error", err)
		return
	}
	if st.Unacked == nil {
		st.Unacked = map[string]*report{}
	}
	var jobs []*jobRec
	for _, j := range st.Jobs {
		if j != nil && core.NamePattern.MatchString(j.Name) {
			jobs = append(jobs, j)
		}
	}
	st.Jobs = jobs
	w.st = st
	w.lastSaved = data
	w.log.Info("worker state loaded", "jobs", len(st.Jobs), "unacked_sources", len(st.Unacked), "paused", st.Paused)
}

func (w *Worker) save() error {
	if w.statePath == "" {
		return nil
	}
	w.saveMu.Lock()
	defer w.saveMu.Unlock()
	w.mu.Lock()
	w.st.Version = 1
	if w.st.Jobs == nil {
		w.st.Jobs = []*jobRec{}
	}
	data, err := json.MarshalIndent(w.st, "", "  ")
	same := bytes.Equal(data, w.lastSaved)
	w.mu.Unlock()
	if err != nil || same {
		return err
	}
	if err := config.WriteAtomic(w.statePath, data, 0o600); err != nil {
		return err
	}
	w.mu.Lock()
	w.lastSaved = data
	w.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------- status

// WorkerStatus is the worker's view for the CLI and MCP.
type WorkerStatus struct {
	Device         string           `json:"device"`
	Labels         []string         `json:"labels"`
	Decision       core.Decision    `json:"decision"`
	Paused         bool             `json:"paused"`
	PauseReason    string           `json:"pause_reason,omitempty"`
	ShuttingDown   bool             `json:"shutting_down"`
	Dirty          bool             `json:"dirty"`
	LastReset      time.Time        `json:"last_reset,omitempty"`
	Slots          SlotStatus       `json:"slots"`
	Running        []RunningJob     `json:"running"`
	Executors      []ExecutorStatus `json:"executors"`
	Sources        []SourceStatus   `json:"sources"`
	PendingReports int              `json:"pending_reports"`
	LastErrors     []ErrorEntry     `json:"last_errors"`
	// LastTick is when a tick last finished and saved its state.
	LastTick time.Time `json:"last_tick,omitempty"`
}

// SlotStatus is how many jobs may run, do run, and could start now.
type SlotStatus struct {
	Total int `json:"total"`
	Used  int `json:"used"`
	Free  int `json:"free"`
}

// RunningJob is one job on this device.
type RunningJob struct {
	Name     string    `json:"name"`
	Source   string    `json:"source"`
	Kind     string    `json:"kind"`
	Repo     string    `json:"repo"`
	Job      string    `json:"job"`
	JobID    int64     `json:"job_id,omitempty"`
	Executor string    `json:"executor"`
	Platform string    `json:"platform"`
	Started  time.Time `json:"started"`
	// Starting is set while the executor's Start has not returned.
	Starting bool `json:"starting,omitempty"`
}

// ExecutorStatus is one executor's health.
type ExecutorStatus struct {
	Name         string    `json:"name"`
	Platforms    []string  `json:"platforms"`
	MaxSlots     int       `json:"max_slots"`
	InUse        int       `json:"in_use"`
	Ready        bool      `json:"ready"`
	BootFailures int       `json:"boot_failures"`
	RetryAt      time.Time `json:"retry_at,omitempty"`
	Closed       bool      `json:"closed"`
	LastError    string    `json:"last_error,omitempty"`
	// Stuck names runners that did not stop or were not cleaned up; the
	// executor takes nothing new while any are listed.
	Stuck []string `json:"stuck,omitempty"`
}

// SourceStatus is one broker's health as this worker sees it.
type SourceStatus struct {
	Name        string    `json:"name"`
	Healthy     bool      `json:"healthy"`
	LastOK      time.Time `json:"last_ok,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Failures    int       `json:"failures"`
	Quarantined bool      `json:"quarantined"`
}

// Status reports the worker's decision, slots, jobs, executors and sources.
func (w *Worker) Status() WorkerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	s := WorkerStatus{Device: w.device, Labels: append([]string(nil), w.labels...), Decision: w.decision,
		Paused: w.st.Paused, PauseReason: w.st.PauseReason, ShuttingDown: w.shutting, Dirty: w.st.Dirty,
		LastReset: w.lastReset, LastErrors: w.errs.list(), Running: []RunningJob{}, Executors: []ExecutorStatus{},
		Sources: []SourceStatus{}, LastTick: w.lastTick}
	s.Slots = SlotStatus{Total: w.decision.Slots, Used: len(w.st.Jobs), Free: w.freeLocked(w.decision, now)}
	for _, j := range w.st.Jobs {
		s.Running = append(s.Running, RunningJob{Name: j.Name, Source: j.Source, Kind: j.Kind, Repo: j.Repo, Job: j.Job,
			JobID: j.JobID, Executor: j.Executor, Platform: j.Platform.String(), Started: j.Started, Starting: j.starting})
	}
	for _, e := range w.execs {
		_, n := w.inUse(e.name)
		es := ExecutorStatus{Name: e.name, MaxSlots: e.ex.MaxSlots(), InUse: n, Ready: w.usableLocked(e, now),
			BootFailures: e.failures, Closed: e.closed, LastError: e.lastErr, Stuck: w.stuckLocked(e.name)}
		if now.Before(e.blockedUntil) {
			es.RetryAt = e.blockedUntil
		}
		for _, p := range e.ex.Platforms() {
			es.Platforms = append(es.Platforms, p.String())
		}
		s.Executors = append(s.Executors, es)
	}
	names := map[string]bool{}
	for n := range w.sources {
		names[n] = true
	}
	for n := range w.st.Quarantined {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		ss := SourceStatus{Name: n, Quarantined: w.st.Quarantined[n] != ""}
		if h := w.sources[n]; h != nil {
			ss.LastOK, ss.LastError, ss.Failures = h.LastOK, h.LastErr, h.Failures
			ss.Healthy = h.Failures == 0 && !h.LastOK.IsZero()
		}
		s.Sources = append(s.Sources, ss)
	}
	for _, r := range w.st.Unacked {
		s.PendingReports += len(r.Ended) + len(r.Results)
	}
	return s
}

// String makes a short summary for logs and tests.
func (s WorkerStatus) String() string {
	return fmt.Sprintf("%s: %d/%d slots used, admit=%v", s.Device, s.Slots.Used, s.Slots.Total, s.Decision.Admit)
}
