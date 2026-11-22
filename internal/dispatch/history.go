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
		concl, step := runs.ParseRunnerLog(string(c.log))
		rec.Conclusion, rec.FailedStep = concl, step
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
