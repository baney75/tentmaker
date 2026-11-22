package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/alerts"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/runs"
	"github.com/baney75/tentmaker/internal/testresults"
)

// history is this device's run history and alerts. apply rebuilds it when
// state_dir, history_max or alerts change.
type history struct {
	store  *runs.Store
	alerts *alerts.Notifier
	log    *slog.Logger
	stop   context.CancelFunc
	device string

	mu sync.Mutex // one record update at a time; early below
	// early holds forge results for this device's jobs that came before
	// the worker recorded them; memory only.
	early map[string]runs.ForgeReport
}

// maxEarly caps the forge results kept for jobs not recorded yet.
const maxEarly = 256

func (d *Daemon) newHistory(cfg config.Config, log *slog.Logger) *history {
	max := cfg.HistoryMax
	if max <= 0 {
		max = config.DefaultHistoryMax
	}
	h := &history{store: runs.Open(filepath.Join(cfg.StateDir, "runs"), max), log: log, device: cfg.Device,
		early: map[string]runs.ForgeReport{}}
	if len(cfg.Alerts) > 0 {
		var resolve func(string) (string, error)
		if d.o.Secrets != nil {
			resolve = func(name string) (string, error) {
				b, err := d.o.Secrets.Get(name)
				return string(b), err
			}
		}
		h.alerts = alerts.New(alerts.Options{Alerts: cfg.Alerts, Resolve: resolve, Log: log, Now: d.o.Now})
		ctx, cancel := context.WithCancel(d.runCtx)
		h.stop = cancel
		go h.alerts.Run(ctx)
	}
	return h
}

func (h *history) close() {
	if h != nil && h.stop != nil {
		h.stop()
	}
}

// JobEnded keeps the record, its redacted log tail and its test results,
// then queues alerts. A forge result that came first replaces the runner's
// diag conclusion and log. go test -json output in the log adds to the
// JUnit reports the executor found.
func (h *history) JobEnded(rec runs.Record, log []byte, tests *runs.TestResults) {
	h.mu.Lock()
	if rep, ok := h.early[rec.ID]; ok {
		delete(h.early, rec.ID)
		rec.ApplyForge(rep)
		if rep.Log != nil {
			log = rep.Log
		}
	}
	h.add(rec, log)
	h.putTests(rec, tests, log)
	if r, ok := h.store.Get(rec.ID); ok {
		rec = r
	}
	h.mu.Unlock()
	h.alerts.JobEnded(rec)
}

// putTests keeps rec's test results: the executor's, plus what go test
// -json printed to log. It keeps nothing when neither has a test.
func (h *history) putTests(rec runs.Record, tests *runs.TestResults, log []byte) {
	var t runs.TestResults
	if tests != nil {
		t = *tests
	}
	if len(log) > 0 {
		t = t.Merge(runs.TestResults{Cases: testresults.ParseGoTestJSON(log)})
	}
	if len(t.Cases) == 0 && t.Dropped == 0 && len(t.Problems) == 0 {
		return
	}
	t.Device = rec.Device
	if err := h.store.PutTests(rec.ID, t); err != nil {
		h.log.Warn("run history: can't keep the test results", "runner", rec.ID, "error", err)
	}
}

// ForgeReport takes a broker's forge result for a runner job: it updates
// this device's record, or keeps a copy for a peer's job that the pool
// merges into the peer's record. A conclusion that changes fires alerts.
func (h *history) ForgeReport(rep runs.ForgeReport) {
	h.mu.Lock()
	rec, ok := h.store.Get(rep.ID)
	if !ok && rep.Device == h.device {
		if len(h.early) < maxEarly {
			h.early[rep.ID] = rep
		}
		h.mu.Unlock()
		return
	}
	prev := ""
	if ok {
		prev = rec.Conclusion
		rec.ApplyForge(rep)
	} else {
		rec = rep.Record()
	}
	h.add(rec, rep.Log)
	if ok && len(rep.Log) > 0 {
		// The forge's log may carry go test -json the runner's diag lacked.
		old, err := h.store.Tests(rep.ID)
		if err != nil {
			old = runs.TestResults{}
		}
		h.putTests(rec, &old, rep.Log)
		if r, found := h.store.Get(rec.ID); found {
			rec = r
		}
	}
	h.mu.Unlock()
	if rec.Conclusion != prev {
		h.alerts.JobEnded(rec)
	}
}

func (h *history) add(rec runs.Record, log []byte) {
	if err := h.store.Add(rec, log); err != nil {
		h.log.Warn("run history: can't keep the record", "runner", rec.ID, "error", err)
	}
}

func (d *Daemon) currentHistory() *history {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.hist
}

// historyProxy is the worker's recorder; it follows apply's rebuilds.
type historyProxy struct{ d *Daemon }

func (p historyProxy) JobEnded(rec runs.Record, log []byte, tests *runs.TestResults) {
	if h := p.d.currentHistory(); h != nil {
		h.JobEnded(rec, log, tests)
	}
}

// ForgeReport is the broker's way in (dispatch.ReportRecorder).
func (p historyProxy) ForgeReport(rep runs.ForgeReport) {
	if h := p.d.currentHistory(); h != nil {
		h.ForgeReport(rep)
	}
}

// localRuns answers q from this device alone.
func (d *Daemon) localRuns(q runs.Query) runs.Answer {
	var lists [][]runs.Record
	if h := d.currentHistory(); h != nil {
		lists = append(lists, h.store.List(q))
		if err := h.store.Err(); err != nil {
			// Say so, but don't fail the pool view over one file.
			return runs.Answer{Records: runs.Apply(runs.Merge(append(lists, d.runningRecords())...), q),
				Errors: []string{d.device() + ": run history: " + err.Error()}}
		}
	}
	lists = append(lists, d.runningRecords())
	a := runs.Answer{Records: runs.Apply(runs.Merge(lists...), q)}
	if b := d.currentBroker(); b != nil && q.Repo != "" {
		a.Queued = b.Queued(q.Repo, q.SHA, q.RunID)
	}
	if a.Records == nil {
		a.Records = []runs.Record{}
	}
	return a
}

func (d *Daemon) runningRecords() []runs.Record {
	d.mu.RLock()
	w := d.worker
	d.mu.RUnlock()
	if w == nil {
		return nil
	}
	return w.RunningRecords()
}

// localRunLog is a job's log tail on this device: live while it runs,
// kept once it ended.
func (d *Daemon) localRunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error) {
	if !runs.ValidID(id) {
		return runs.LogAnswer{}, errors.New("bad job id")
	}
	if max <= 0 || max > runs.LogTailBytes {
		max = runs.LogTailBytes
	}
	d.mu.RLock()
	w := d.worker
	d.mu.RUnlock()
	if w != nil {
		l, ok, err := w.JobLog(ctx, id, max)
		if ok {
			if errors.Is(err, core.ErrNotFound) {
				// Running, but its log isn't written yet.
				return runs.LogAnswer{ID: id, Device: d.device(), Running: true, Data: []byte{}}, nil
			}
			if err != nil {
				return runs.LogAnswer{}, fmt.Errorf("read the running job's log: %w", err)
			}
			return runs.LogAnswer{ID: id, Device: d.device(), Running: true, Size: l.Size, Data: runs.Redact(l.Data)}, nil
		}
	}
	h := d.currentHistory()
	if h == nil {
		return runs.LogAnswer{}, core.ErrNotFound
	}
	rec, ok := h.store.Get(id)
	if !ok || (rec.Device != d.device() && !rec.HasLog) {
		// A peer's job this broker has only a forge result for: the peer
		// has the log.
		return runs.LogAnswer{}, core.ErrNotFound
	}
	data, err := h.store.Log(id)
	if errors.Is(err, core.ErrNotFound) {
		return runs.LogAnswer{ID: id, Device: rec.Device, Data: []byte{}, Source: rec.LogSource}, nil
	}
	if err != nil {
		return runs.LogAnswer{}, err
	}
	size := int64(len(data))
	return runs.LogAnswer{ID: id, Device: rec.Device, Size: size, Data: runs.Tail(data, int(max)), Source: rec.LogSource}, nil
}

// localRerun reruns failed jobs with this device's forge token for repo,
// or says core.ErrNotFound when it has none.
func (d *Daemon) localRerun(ctx context.Context, repo string, runID int64) error {
	d.mu.RLock()
	cfg, forges := d.cfg, d.forges
	d.mu.RUnlock()
	for _, r := range cfg.Repos {
		if !strings.EqualFold(r.Name, repo) {
			continue
		}
		f, ok := forges.Forges[RepoForge(r)]
		if !ok {
			return core.ErrNotFound
		}
		rr, ok := f.(core.Rerunner)
		if !ok {
			return fmt.Errorf("the %s forge can't rerun jobs", RepoForge(r))
		}
		if r.Mode == "checks" {
			return errors.New("checks-mode jobs are not forge runs; push again or ask the broker to retry")
		}
		cctx, cancel := context.WithTimeout(ctx, forgeCallTimeout)
		defer cancel()
		return rr.RerunFailed(cctx, r.Name, runID)
	}
	return core.ErrNotFound
}

// forgeCallTimeout bounds one rerun request to the forge.
const forgeCallTimeout = 15 * time.Second

// runsSource is the daemon as the mesh serves its history.
type runsSource struct{ d *Daemon }

func (s runsSource) Runs(q runs.Query) runs.Answer { return s.d.localRuns(q) }
func (s runsSource) RunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error) {
	return s.d.localRunLog(ctx, id, max)
}
func (s runsSource) Rerun(ctx context.Context, repo string, runID int64) error {
	return s.d.localRerun(ctx, repo, runID)
}

// Runs answers q from the whole pool (this device alone without a mesh).
func (h ctlHandler) Runs(ctx context.Context, q runs.Query) (runs.Answer, error) {
	if n := h.d.currentNode(); n != nil {
		return n.PoolRuns(ctx, q)
	}
	return h.d.localRuns(q), nil
}

func (h ctlHandler) RunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error) {
	if n := h.d.currentNode(); n != nil {
		return n.PoolRunLog(ctx, id, max)
	}
	return h.d.localRunLog(ctx, id, max)
}

func (h ctlHandler) Rerun(ctx context.Context, req control.RerunRequest) (control.RerunAnswer, error) {
	ans := control.RerunAnswer{Repo: req.Repo, RunID: req.RunID}
	if n := h.d.currentNode(); n != nil {
		dev, err := n.PoolRerun(ctx, req.Repo, req.RunID)
		ans.Broker = dev
		return ans, err
	}
	err := h.d.localRerun(ctx, req.Repo, req.RunID)
	if errors.Is(err, core.ErrNotFound) {
		return ans, fmt.Errorf("this device has no forge token for %s and no mesh to ask a broker", req.Repo)
	}
	ans.Broker = h.d.device()
	return ans, err
}

// AlertsTest sends a test message to every configured alert.
func (h ctlHandler) AlertsTest(ctx context.Context) (any, error) {
	hist := h.d.currentHistory()
	if hist == nil || !hist.alerts.Enabled() {
		return nil, errors.New("no alerts in the config; add an [[alerts]] table (see docs/config.md)")
	}
	return hist.alerts.Test(ctx, h.d.device()), nil
}

var (
	_ control.RunsHandler = ctlHandler{}
	_ mesh.RunsSource     = runsSource{}
)
