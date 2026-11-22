package dispatch

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// RunRecorder keeps run history. The worker calls JobEnded once per job
// that ran, off its lock, with the record and the raw tail of the job's
// log (nil when the executor can't read one); the recorder redacts it. It
// must not block for long: the daemon's stores it and queues alerts.
type RunRecorder interface {
	JobEnded(rec runs.Record, log []byte)
}

// ReportRecorder takes a broker's forge result for a runner job that
// ended: the record's conclusion, failed step and log come from the forge,
// not the runner's diag logs. It must not block for long.
type ReportRecorder interface {
	ForgeReport(rep runs.ForgeReport)
}

// reportReq is an ended runner job whose forge result is still to read.
type reportReq struct {
	rep   runs.ForgeReport
	tries int
	next  time.Time
}

const (
	reportEvery = 10 * time.Second // between reads of one job
	reportTries = 12               // about two minutes, runs.ForgeGrace
	logTries    = 3                // reads of a completed job whose log isn't up yet
	maxReports  = 256
)

// queueReportLocked queues the forge read for a runner job that ended
// identified on a forge that can report it.
func (b *Broker) queueReportLocked(rec *grantRec) {
	if b.o.Reports == nil || rec.Kind != core.KindRunner || !rec.Identified || rec.Fork || rec.TookJob == 0 ||
		len(b.reports) >= maxReports {
		return
	}
	if _, ok := b.o.Forges[rec.Forge].(core.JobReporter); !ok {
		return
	}
	rep := runs.ForgeReport{ID: rec.Name, Device: rec.Device, Forge: rec.Forge, Repo: rec.Repo, JobID: rec.TookJob,
		RunID: rec.TookRun, EndedAt: rec.EndedAt}
	if rec.Job.ID == rec.TookJob {
		rep.Job, rep.RunURL = rec.Job.Name, rec.Job.HTMLURL
	}
	b.reports = append(b.reports, &reportReq{rep: rep, next: b.now()})
}

// sendReports reads the forge result of each queued job that is due and
// hands it to Reports. A job not completed yet, or a read that fails, is
// tried again until reportTries; then the record keeps what the runner's
// diag logs said.
func (b *Broker) sendReports(ctx context.Context, now time.Time) {
	b.mu.Lock()
	var due []*reportReq
	keep := b.reports[:0]
	for _, q := range b.reports {
		if now.Before(q.next) {
			keep = append(keep, q)
		} else {
			due = append(due, q)
		}
	}
	b.reports = keep
	b.mu.Unlock()
	for _, q := range due {
		jr, ok := b.o.Forges[q.rep.Forge].(core.JobReporter)
		if !ok {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		res, err := jr.JobReport(fctx, q.rep.Repo, q.rep.JobID, runs.LogTailBytes)
		cancel()
		q.tries++
		if err == nil && res.Conclusion != "" && (res.LogErr == nil || q.tries >= logTries) {
			q.rep.Conclusion, q.rep.FailedStep, q.rep.Log = res.Conclusion, res.FailedStep, res.Log
			q.rep.SHA, q.rep.Workflow, q.rep.StartedAt = res.HeadSHA, res.Workflow, res.StartedAt
			if res.LogErr != nil {
				b.log.Info("job log not read from the forge; the record keeps the runner's diag log",
					"runner", q.rep.ID, "job_id", q.rep.JobID, "error", res.LogErr)
				q.rep.Log = nil
			}
			b.o.Reports.ForgeReport(q.rep)
			continue
		}
		if err != nil {
			b.noteErr(q.rep.Repo, "read job result", err)
		}
		if q.tries >= reportTries {
			b.log.Info("job result not read from the forge; the record keeps the runner's diag log",
				"runner", q.rep.ID, "job_id", q.rep.JobID)
			continue
		}
		q.next = now.Add(reportEvery)
		b.mu.Lock()
		b.reports = append(b.reports, q)
		b.mu.Unlock()
	}
}

// usageEvery is how often a running job's cgroup is read. The cgroup goes
// when the job's unit stops, so the last reading is what history keeps.
const usageEvery = 5 * time.Second

// freeSlotLocked is the lowest slot number no job on the device holds.
func (w *Worker) freeSlotLocked() int {
	used := map[int]bool{}
	for _, j := range w.st.Jobs {
		used[j.Slot] = true
	}
	n := 1
	for used[n] {
		n++
	}
	return n
}

// sampleUsage reads a running job's use at most every usageEvery.
func (w *Worker) sampleUsage(ctx context.Context, e *execInfo, j *jobRec, now time.Time) {
	in, ok := e.ex.(core.JobInspector)
	if !ok {
		return
	}
	w.mu.Lock()
	due := now.Sub(j.sampled) >= usageEvery
	if due {
		j.sampled = now
	}
	w.mu.Unlock()
	if !due {
		return
	}
	uctx, cancel := context.WithTimeout(ctx, execTimeout)
	u, err := in.JobUsage(uctx, j.Name)
	cancel()
	if err != nil {
		return
	}
	w.mu.Lock()
	mergeUsage(&j.Usage, u)
	w.mu.Unlock()
}

// mergeUsage keeps the larger of each reading: the peak and the CPU time
// only grow while a job runs, and a reading after the unit stopped is 0.
func mergeUsage(dst *core.Usage, u core.Usage) {
	dst.PeakMemoryBytes = max(dst.PeakMemoryBytes, u.PeakMemoryBytes)
	dst.CPUSeconds = max(dst.CPUSeconds, u.CPUSeconds)
	dst.CacheBytes = max(dst.CacheBytes, u.CacheBytes)
}

type collected struct {
	log   []byte
	hasLg bool
	usage core.Usage
	ended time.Time
}

// collect reads a finished job's log and last use before Clean.
func (w *Worker) collect(ctx context.Context, e *execInfo, j *jobRec) collected {
	c := collected{ended: w.now()}
	w.mu.Lock()
	c.usage = j.Usage
	w.mu.Unlock()
	if w.o.Runs == nil || e == nil {
		return c
	}
	in, ok := e.ex.(core.JobInspector)
	if !ok {
		return c
	}
	uctx, cancel := context.WithTimeout(ctx, execTimeout)
	if u, err := in.JobUsage(uctx, j.Name); err == nil {
		mergeUsage(&c.usage, u)
	}
	cancel()
	lctx, cancel := context.WithTimeout(ctx, execTimeout)
	if l, err := in.JobLog(lctx, j.Name, runs.LogTailBytes); err == nil {
		c.log, c.hasLg = l.Data, true
	}
	cancel()
	return c
}

// baseRecord is the record of a job as the worker knows it.
func (w *Worker) baseRecord(j *jobRec) runs.Record {
	return runs.Record{
		ID: j.Name, Kind: j.Kind, Forge: j.Forge, Repo: j.Repo, Workflow: j.Workflow, Job: j.Job,
		JobID: j.JobID, RunID: j.RunID, RunURL: j.RunURL, Ref: j.Ref, SHA: j.SHA, Device: w.device,
		Executor: j.Executor, Platform: j.Platform.String(), Slot: j.Slot, QueuedAt: j.Queued, StartedAt: j.Started,
		PeakMemoryBytes: j.Usage.PeakMemoryBytes, CPUSeconds: j.Usage.CPUSeconds, CacheBytes: j.Usage.CacheBytes,
	}
}

// recordEnd hands the ended job's record to the recorder.
func (w *Worker) recordEnd(j *jobRec, res *core.CheckResult, why string, c collected) {
	if w.o.Runs == nil {
		return
	}
	w.mu.Lock()
	rec := w.baseRecord(j)
	stopping := j.Stopping
	w.mu.Unlock()
	rec.EndedAt = c.ended
	rec.PeakMemoryBytes, rec.CPUSeconds, rec.CacheBytes = c.usage.PeakMemoryBytes, c.usage.CPUSeconds, c.usage.CacheBytes
	log := c.log
	switch {
	case res != nil:
		rec.Conclusion = runs.CheckConclusion(res.State)
		rec.FailedStep = runs.CheckStep(res.Description)
		if rec.SHA == "" {
			rec.SHA = res.SHA
		}
		if res.State != "success" {
			rec.Note = res.Description
		}
		if !c.hasLg && res.LogTail != "" {
			log = []byte(res.LogTail)
		}
	default:
		// The broker's forge result replaces these when it comes.
		concl, step := runs.ParseRunnerLog(string(c.log))
		rec.Conclusion, rec.FailedStep, rec.LogSource = concl, step, runs.LogRunnerDiag
		if rec.Conclusion == "" {
			rec.Conclusion = runs.Unknown
		}
	}
	if why == "" {
		why = stopping
	}
	if why != "" && why != "stopped" {
		rec.Note = why
	}
	if why != "" && rec.Conclusion != runs.Failure {
		// Tentmaker ended it: a stop, an eviction, max_duration.
		rec.Conclusion = runs.Cancelled
		if res != nil && res.State == "error" {
			rec.Conclusion = runs.Error
		}
	}
	w.o.Runs.JobEnded(rec, log)
}

// RunningRecords lists the jobs on this device as running records.
func (w *Worker) RunningRecords() []runs.Record {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]runs.Record, 0, len(w.st.Jobs))
	for _, j := range w.st.Jobs {
		r := w.baseRecord(j)
		r.Conclusion = runs.Running
		out = append(out, r)
	}
	return out
}

// JobLog reads a job's live log while it is on this device; ok is false
// when no such job runs here. The data is raw; the caller redacts it.
func (w *Worker) JobLog(ctx context.Context, name string, max int64) (core.JobLog, bool, error) {
	w.mu.Lock()
	var j *jobRec
	for _, x := range w.st.Jobs {
		if x.Name == name {
			j = x
		}
	}
	w.mu.Unlock()
	if j == nil {
		return core.JobLog{}, false, nil
	}
	e := w.execByName(j.Executor)
	if e == nil {
		return core.JobLog{}, true, core.ErrNotFound
	}
	in, ok := e.ex.(core.JobInspector)
	if !ok {
		return core.JobLog{}, true, core.ErrNotFound
	}
	lctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	l, err := in.JobLog(lctx, name, max)
	return l, true, err
}

// Queued names the Tentmaker jobs this broker knows for repo at sha (a
// prefix of 7 or more) or in forge run runID that have not ended:
// runner-mode jobs the forge still lists as queued, and checks-mode jobs
// with no final status yet (checks jobs have no run id). Naming neither a
// sha nor a run id matches nothing. A job may also show as a running
// record for a moment; that only makes a wait longer, never shorter.
func (b *Broker) Queued(repo, sha string, runID int64) []string {
	if sha == "" && runID == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, r := range b.repos {
		if !strings.EqualFold(r.cfg.Name, repo) {
			continue
		}
		for _, j := range r.queued {
			if !hasLabel(j.Labels, LabelTentmaker) || (sha != "" && !runs.SameSHA(j.HeadSHA, sha)) || (runID != 0 && j.RunID != runID) {
				continue
			}
			out = append(out, fmt.Sprintf("%s / %s (job %d)", j.Workflow, j.Name, j.ID))
		}
		for _, h := range r.heads {
			if runID != 0 || !runs.SameSHA(h.commit.SHA, sha) {
				continue
			}
			for _, cj := range h.jobs {
				ctxName := CheckContext(cj)
				if _, ok := h.statuses[ctxName]; !ok {
					out = append(out, ctxName)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
