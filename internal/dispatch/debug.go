package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// Debugging a failed job (docs/DESIGN.md, "Debugging a failed job").
//
// A repo with [repos.debug] keep_failed set has a failed job's sandbox kept
// on the device that ran it instead of cleaned up. The job is reported
// ended to its broker at once (its runner is deleted, its relay and Actions
// cache grant are revoked), but its record stays in state.json with
// KeptUntil set, so it holds its slot and memory, blocks the node reset
// that would wipe it, and keeps the device from taking the repo's jobs.
// When the grace ends (or the owner releases it, or a hard guard evicts),
// the worker cleans it up exactly as it cleans any job.

// Debug limits.
const (
	// DebugTimeout is a debug command's default time limit, and
	// MaxDebugTimeout the most a caller may ask for.
	DebugTimeout    = time.Minute
	MaxDebugTimeout = 10 * time.Minute
	// DebugOutput is how much output a debug command keeps by default, and
	// MaxDebugOutput the most a caller may ask for.
	DebugOutput    = 64 << 10
	MaxDebugOutput = 1 << 20
	// ShellTimeout bounds one interactive debug shell; the sandbox's own
	// grace ends it sooner.
	ShellTimeout = time.Hour
)

// ErrNotKept means no failed job's sandbox with that name is kept on this
// device.
var ErrNotKept = errors.New("no kept sandbox")

// kept reports whether the job ended and its sandbox is kept for debugging.
func (j *jobRec) kept() bool { return !j.KeptUntil.IsZero() }

// KeptJob is one failed job's sandbox kept on this device.
type KeptJob struct {
	Name     string    `json:"name"`
	Repo     string    `json:"repo"`
	Job      string    `json:"job"`
	Executor string    `json:"executor"`
	Until    time.Time `json:"until"`
}

// endedLocked reports j ended to the source that granted it and drops what
// it held only while it ran: its relays and Actions cache grant.
func (w *Worker) endedLocked(j *jobRec, res *core.CheckResult) {
	w.dropRelaysLocked(j.Name)
	w.revokeActions(j.Name)
	rep := w.reportLocked(j.Source)
	rep.Ended = append(rep.Ended, j.Name)
	if res != nil {
		rep.Results = append(rep.Results, *res)
	}
	w.st.Dirty = true
}

// keep keeps the sandbox of a job that failed on its own, when its repo
// asks for that on this device and its executor can. It reports true when
// it kept it: the job is reported ended and stays counted until release.
func (w *Worker) keep(ctx context.Context, e *execInfo, j *jobRec, why string, res *core.CheckResult, rec *runs.Record) bool {
	if e == nil || why != "" || rec.Conclusion != runs.Failure {
		return false
	}
	d := w.o.Config.KeepFailedFor(j.Forge, j.Repo)
	if d <= 0 {
		return false
	}
	dbg, ok := e.ex.(core.Debugger)
	if !ok {
		w.log.Info("failed job's sandbox not kept: this executor can't keep one", "runner", j.Name, "executor", e.name)
		return false
	}
	w.mu.Lock()
	shutting := w.shutting
	w.mu.Unlock()
	if shutting {
		return false
	}
	kctx, cancel := context.WithTimeout(ctx, execTimeout)
	err := dbg.Keep(kctx, j.Name)
	cancel()
	if err != nil {
		w.log.Warn("failed job's sandbox not kept; cleaning it up", "runner", j.Name, "error", err)
		return false
	}
	until := w.now().Add(d)
	rec.KeptUntil = until
	w.mu.Lock()
	j.KeptUntil = until
	j.Actions = nil
	w.endedLocked(j, res)
	w.holdActionsLocked()
	w.mu.Unlock()
	w.log.Info("failed job's sandbox kept for debugging", "runner", j.Name, "repo", j.Repo, "until", until.Format(time.RFC3339))
	if err := w.save(); err != nil {
		w.log.Error("state not saved", "error", err)
	}
	return true
}

// release cleans up a kept sandbox, the way finish cleans up any job.
func (w *Worker) release(ctx context.Context, e *execInfo, j *jobRec, why string) {
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
	for i, x := range w.st.Jobs {
		if x == j {
			w.st.Jobs = append(w.st.Jobs[:i], w.st.Jobs[i+1:]...)
			break
		}
	}
	w.holdActionsLocked()
	w.st.Dirty = true
	w.mu.Unlock()
	w.log.Info("kept sandbox cleaned up", "runner", j.Name, "repo", j.Repo, "why", why)
}

// keptReposLocked names the repos with a kept sandbox, for WorkRequest.Kept.
func (w *Worker) keptReposLocked() []core.CacheRepo {
	var out []core.CacheRepo
	for _, j := range w.st.Jobs {
		if j.kept() && !keptIn(out, j.Forge, j.Repo) {
			out = append(out, core.CacheRepo{Forge: j.Forge, Repo: j.Repo})
		}
	}
	return out
}

func keptIn(list []core.CacheRepo, forge, repo string) bool {
	for _, k := range list {
		if k.Forge == forge && strings.EqualFold(k.Repo, repo) {
			return true
		}
	}
	return false
}

// KeptJobs lists the sandboxes kept on this device.
func (w *Worker) KeptJobs() []KeptJob {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.keptJobsLocked()
}

func (w *Worker) keptJobsLocked() []KeptJob {
	out := []KeptJob{}
	for _, j := range w.st.Jobs {
		if j.kept() {
			out = append(out, KeptJob{Name: j.Name, Repo: j.Repo, Job: j.Job, Executor: j.Executor, Until: j.KeptUntil})
		}
	}
	return out
}

// keptJob finds a kept sandbox and its executor's Debugger.
func (w *Worker) keptJob(name string) (*jobRec, core.Debugger, error) {
	w.mu.Lock()
	var j *jobRec
	for _, x := range w.st.Jobs {
		if x.Name == name && x.kept() {
			j = x
		}
	}
	var spec core.DebugSpec
	if j != nil {
		spec.Limits = j.Limits
	}
	w.mu.Unlock()
	if j == nil {
		return nil, nil, fmt.Errorf("%w for job %s on this device", ErrNotKept, name)
	}
	e := w.execByName(j.Executor)
	if e == nil {
		return nil, nil, fmt.Errorf("%w: its executor %s is gone", ErrNotKept, j.Executor)
	}
	dbg, ok := e.ex.(core.Debugger)
	if !ok {
		return nil, nil, fmt.Errorf("the %s executor can't debug a job", e.name)
	}
	return j, dbg, nil
}

// DebugExec runs command in job name's kept sandbox. timeout and maxOut
// of 0 take the defaults; larger than the maximums is an error.
func (w *Worker) DebugExec(ctx context.Context, name, command string, timeout time.Duration, maxOut int64) (core.DebugResult, error) {
	if strings.TrimSpace(command) == "" {
		return core.DebugResult{}, errors.New("no command")
	}
	if timeout == 0 {
		timeout = DebugTimeout
	}
	if maxOut == 0 {
		maxOut = DebugOutput
	}
	if timeout < 0 || timeout > MaxDebugTimeout || maxOut < 0 || maxOut > MaxDebugOutput {
		return core.DebugResult{}, fmt.Errorf("timeout must be up to %s and output up to %d bytes", MaxDebugTimeout, MaxDebugOutput)
	}
	j, dbg, err := w.keptJob(name)
	if err != nil {
		return core.DebugResult{}, err
	}
	spec := core.DebugSpec{Limits: j.Limits, Timeout: timeout, MaxOutput: maxOut}
	ctx, cancel := context.WithTimeout(ctx, timeout+execTimeout)
	defer cancel()
	res, err := dbg.DebugExec(ctx, name, command, spec)
	if err != nil {
		return res, err
	}
	// Redact first: a secret cut in half would no longer match.
	res.Output = runs.Redact(res.Output)
	if int64(len(res.Output)) > maxOut {
		res.Output, res.Truncated = res.Output[len(res.Output)-int(maxOut):], true
	}
	w.log.Info("debug command run", "runner", name, "exit_code", res.ExitCode, "timed_out", res.TimedOut)
	return res, nil
}

// DebugShell runs an interactive shell in job name's kept sandbox until it
// exits, ctx ends, or the sandbox's grace ends.
func (w *Worker) DebugShell(ctx context.Context, name string, stdin io.Reader, stdout io.Writer) error {
	j, dbg, err := w.keptJob(name)
	if err != nil {
		return err
	}
	w.mu.Lock()
	left := j.KeptUntil.Sub(w.now())
	w.mu.Unlock()
	if left <= 0 {
		return fmt.Errorf("%w: job %s's grace is over", ErrNotKept, name)
	}
	ctx, cancel := context.WithTimeout(ctx, min(left, ShellTimeout))
	defer cancel()
	w.log.Info("debug shell opened", "runner", name)
	err = dbg.DebugShell(ctx, name, core.DebugSpec{Limits: j.Limits, Timeout: min(left, ShellTimeout)}, stdin, stdout)
	w.log.Info("debug shell closed", "runner", name)
	return err
}

// ReleaseKept ends job name's grace now; the next tick cleans it up.
func (w *Worker) ReleaseKept(name string) error {
	w.mu.Lock()
	found := false
	for _, j := range w.st.Jobs {
		if j.Name == name && j.kept() {
			j.KeptUntil, found = w.now(), true
		}
	}
	w.mu.Unlock()
	if !found {
		return fmt.Errorf("%w for job %s on this device", ErrNotKept, name)
	}
	w.Kick()
	return nil
}
