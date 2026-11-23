package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// SandboxSource is the Source a sandbox's job record carries. No broker
// has this name, so a sandbox is never reported to one.
const SandboxSource = "sandbox"

// SandboxSpec asks the worker for one sandbox.
type SandboxSpec struct {
	Name     string         // matches core.NamePattern
	Platform core.Platform  // OS required; an empty Arch means any
	Limits   core.Resources // zero: the configured job size
	TTL      time.Duration  // how long it lives; always > 0
	Owner    string         // who asked, for listing and access checks
}

// SandboxInfo is one sandbox this worker holds.
type SandboxInfo struct {
	Name     string         `json:"id"`
	Device   string         `json:"device"`
	Executor string         `json:"executor"`
	Platform core.Platform  `json:"platform"`
	Limits   core.Resources `json:"limits"`
	Started  time.Time      `json:"started"`
	Expires  time.Time      `json:"expires"`
	Owner    string         `json:"owner,omitempty"`
	State    string         `json:"state"` // starting, running, stopping
}

// NoRoomError is a sandbox the worker refused to place. Reason says why in
// plain words; nothing was started.
type NoRoomError struct{ Reason string }

func (e *NoRoomError) Error() string { return "no room for a sandbox: " + e.Reason }

// ErrNoSandbox means no sandbox by that name is on this worker.
var ErrNoSandbox = errors.New("no such sandbox")

// slotsOf is how many job slots r takes: a job-sized sandbox takes one, a
// bigger one as many as its memory or CPUs need, so the budget's slot count
// stays honest about memory.
func (w *Worker) slotsOf(r core.Resources) int {
	n := 1.0
	if w.limits.MemoryBytes > 0 {
		n = math.Max(n, float64(r.MemoryBytes)/float64(w.limits.MemoryBytes))
	}
	if w.limits.CPUs > 0 {
		n = math.Max(n, r.CPUs/w.limits.CPUs)
	}
	return int(math.Ceil(n - 1e-9))
}

// usedSlotsLocked counts every job as one slot and every sandbox as the
// slots its limits take.
func (w *Worker) usedSlotsLocked() int {
	n := 0
	for _, j := range w.st.Jobs {
		if j.Kind == core.KindSandbox {
			n += w.slotsOf(j.Limits)
		} else {
			n++
		}
	}
	return n
}

// StartSandbox places one sandbox on this device, if the budget, the slots
// and an executor that can hold sandboxes allow it, and waits until it is
// ready. It never starts one it can't account for: a refusal is a
// *NoRoomError and leaves nothing behind.
func (w *Worker) StartSandbox(ctx context.Context, spec SandboxSpec) (SandboxInfo, error) {
	if !core.NamePattern.MatchString(spec.Name) {
		return SandboxInfo{}, fmt.Errorf("bad sandbox name %q", spec.Name)
	}
	if spec.TTL <= 0 {
		return SandboxInfo{}, errors.New("a sandbox needs a time to live")
	}
	limits := spec.Limits
	if limits.MemoryBytes == 0 {
		limits.MemoryBytes = w.limits.MemoryBytes
	}
	if limits.CPUs == 0 {
		limits.CPUs = w.limits.CPUs
	}
	if limits.MemoryBytes <= 0 || !(limits.CPUs > 0) || math.IsInf(limits.CPUs, 0) {
		return SandboxInfo{}, errors.New("a sandbox needs a memory and CPU limit above zero")
	}
	now := w.now()
	w.mu.Lock()
	e, plat, why := w.admitSandboxLocked(spec.Platform, limits, now)
	if why != "" {
		w.mu.Unlock()
		return SandboxInfo{}, &NoRoomError{Reason: why}
	}
	for _, j := range w.st.Jobs {
		if j.Name == spec.Name {
			w.mu.Unlock()
			return SandboxInfo{}, fmt.Errorf("sandbox %s already exists", spec.Name)
		}
	}
	rec := &jobRec{Name: spec.Name, Source: SandboxSource, Kind: core.KindSandbox, Executor: e.name,
		Platform: plat, Started: now, Limits: limits, Expires: now.Add(spec.TTL), Owner: spec.Owner, starting: true}
	w.st.Jobs = append(w.st.Jobs, rec)
	// A sandbox runs whatever an agent sends, as the user every job runs
	// as, so no repo gets a cache again until the next reset.
	w.markDirtyLocked()
	w.mu.Unlock()
	if err := w.save(); err != nil {
		w.log.Error("state not saved before a sandbox start", "error", err)
	}

	execSpec := core.ExecSpec{Kind: core.KindSandbox, Name: spec.Name, Platform: plat, Limits: limits,
		MaxDuration: spec.TTL, TestWorkers: max(1, int(math.Floor(limits.CPUs)))}
	w.starts.Add(1)
	sctx, cancel := context.WithTimeout(ctx, startTimeout)
	err := e.ex.Start(sctx, execSpec)
	cancel()
	w.starts.Done()
	if err != nil {
		w.log.Warn("sandbox start failed", "executor", e.name, "sandbox", spec.Name, "error", err)
		w.mu.Lock()
		w.removeJobLocked(rec)
		w.errs.add(w.now(), e.name, "sandbox start: "+err.Error())
		w.mu.Unlock()
		w.stopAndClean(context.WithoutCancel(ctx), e, spec.Name)
		_ = w.save()
		return SandboxInfo{}, fmt.Errorf("start the sandbox on %s: %w", e.name, err)
	}
	w.mu.Lock()
	rec.starting = false
	stopping := rec.Stopping
	info := w.sandboxInfoLocked(rec)
	w.mu.Unlock()
	if stopping != "" {
		// Removed or shut down while it started.
		w.stopJob(context.WithoutCancel(ctx), rec, stopping)
		return SandboxInfo{}, fmt.Errorf("the sandbox was stopped while it started: %s", stopping)
	}
	w.log.Info("sandbox started", "executor", e.name, "sandbox", spec.Name, "platform", plat.String(),
		"memory", limits.MemoryBytes, "cpus", limits.CPUs, "expires", rec.Expires.Format(time.RFC3339))
	return info, nil
}

// admitSandboxLocked picks the executor and platform for a sandbox, or says
// why there is no room. Every check fails closed.
func (w *Worker) admitSandboxLocked(want core.Platform, limits core.Resources, now time.Time) (*execInfo, core.Platform, string) {
	d := w.decision
	switch {
	case w.shutting:
		return nil, core.Platform{}, "the daemon is shutting down"
	case w.st.Paused:
		why := "this device is paused"
		if w.st.PauseReason != "" {
			why += ": " + w.st.PauseReason
		}
		return nil, core.Platform{}, why
	case w.holds > 0:
		return nil, core.Platform{}, "an update is in progress"
	case w.cachedRepoLocked() != nil:
		// A cache is only as private as the least trusted job beside it,
		// and a sandbox is trusted for no repo (cacheDecisionLocked).
		return nil, core.Platform{}, "this device holds a repo's cache for its trusted CI jobs until its next reset; try again later"
	case w.lastTick.IsZero():
		return nil, core.Platform{}, "the budget has not been decided yet; try again in a few seconds"
	case !d.Admit:
		why := "the budget admits no new work"
		if len(d.Reasons) > 0 {
			why += ": " + strings.Join(d.Reasons, "; ")
		}
		return nil, core.Platform{}, why
	}
	need := w.slotsOf(limits)
	if used := w.usedSlotsLocked(); used+need > d.Slots {
		return nil, core.Platform{}, fmt.Sprintf("the %s budget has %d slots of %s and %g CPUs, %d in use, and this sandbox needs %d",
			d.Profile, d.Slots, config.Size(w.limits.MemoryBytes), w.limits.CPUs, used, need)
	}
	var used core.Resources
	for _, j := range w.st.Jobs {
		used = used.Add(j.Limits)
	}
	if d.Limit.MemoryBytes > 0 && !used.Add(limits).Fits(d.Limit) {
		return nil, core.Platform{}, fmt.Sprintf("the %s budget is %s and %g CPUs, %s and %g CPUs are in use, and this sandbox needs %s and %g CPUs",
			d.Profile, config.Size(d.Limit.MemoryBytes), d.Limit.CPUs, config.Size(used.MemoryBytes), used.CPUs,
			config.Size(limits.MemoryBytes), limits.CPUs)
	}
	var reasons []string
	for _, e := range w.execs {
		var plat core.Platform
		for _, p := range e.ex.Platforms() {
			if strings.EqualFold(p.OS, want.OS) && (want.Arch == "" || strings.EqualFold(ArchLabel(p.Arch), ArchLabel(want.Arch))) {
				plat = p
				break
			}
		}
		if plat.OS == "" {
			continue
		}
		if _, ok := e.ex.(core.Sandboxer); !ok {
			reasons = append(reasons, e.name+" can't hold sandboxes yet")
			continue
		}
		if !w.usableLocked(e, now) {
			why := e.name + " is not ready"
			if e.lastErr != "" {
				why += ": " + e.lastErr
			}
			reasons = append(reasons, why)
			continue
		}
		if w.capacityLocked(e) == 0 {
			reasons = append(reasons, fmt.Sprintf("%s is at its limit of %d", e.name, e.ex.MaxSlots()))
			continue
		}
		return e, plat, ""
	}
	if len(reasons) == 0 {
		return nil, core.Platform{}, "nothing on this device runs " + want.String()
	}
	return nil, core.Platform{}, strings.Join(reasons, "; ")
}

func (w *Worker) removeJobLocked(rec *jobRec) {
	for i, x := range w.st.Jobs {
		if x == rec {
			w.st.Jobs = append(w.st.Jobs[:i], w.st.Jobs[i+1:]...)
			return
		}
	}
}

func (w *Worker) sandboxLocked(name string) *jobRec {
	for _, j := range w.st.Jobs {
		if j.Name == name && j.Kind == core.KindSandbox {
			return j
		}
	}
	return nil
}

func (w *Worker) sandboxInfoLocked(j *jobRec) SandboxInfo {
	st := "running"
	switch {
	case j.starting:
		st = "starting"
	case j.Stopping != "":
		st = "stopping"
	}
	return SandboxInfo{Name: j.Name, Device: w.device, Executor: j.Executor, Platform: j.Platform, Limits: j.Limits,
		Started: j.Started, Expires: j.Expires, Owner: j.Owner, State: st}
}

// Sandbox returns one sandbox, or ErrNoSandbox.
func (w *Worker) Sandbox(name string) (SandboxInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j := w.sandboxLocked(name)
	if j == nil {
		return SandboxInfo{}, ErrNoSandbox
	}
	return w.sandboxInfoLocked(j), nil
}

// Sandboxes lists this worker's sandboxes, oldest first.
func (w *Worker) Sandboxes() []SandboxInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []SandboxInfo{}
	for _, j := range w.st.Jobs {
		if j.Kind == core.KindSandbox {
			out = append(out, w.sandboxInfoLocked(j))
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Started.Before(out[k].Started) })
	return out
}

// ExecSandbox runs one command in a running sandbox and returns its exit
// code. The command never outlives the sandbox's TTL.
func (w *Worker) ExecSandbox(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return -1, errors.New("no command to run")
	}
	w.mu.Lock()
	j := w.sandboxLocked(name)
	if j == nil {
		w.mu.Unlock()
		return -1, ErrNoSandbox
	}
	if j.starting || j.Stopping != "" {
		st := w.sandboxInfoLocked(j).State
		w.mu.Unlock()
		return -1, fmt.Errorf("sandbox %s is %s", name, st)
	}
	left := j.Expires.Sub(w.now())
	e := w.execByName(j.Executor)
	w.mu.Unlock()
	if left <= 0 {
		return -1, fmt.Errorf("sandbox %s has expired", name)
	}
	if e == nil {
		return -1, fmt.Errorf("sandbox %s's executor is gone", name)
	}
	sb, ok := e.ex.(core.Sandboxer)
	if !ok {
		return -1, fmt.Errorf("%s can't run commands in sandboxes", e.name)
	}
	if req.Timeout <= 0 || req.Timeout > left {
		req.Timeout = left
	}
	ectx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	return sb.Exec(ectx, name, req, stdin, stdout, stderr)
}

// StopSandbox stops and deletes a sandbox now. A stop that fails leaves it
// counted and retried, like a job's.
func (w *Worker) StopSandbox(ctx context.Context, name, why string) error {
	w.mu.Lock()
	j := w.sandboxLocked(name)
	w.mu.Unlock()
	if j == nil {
		return ErrNoSandbox
	}
	w.stopJob(ctx, j, why)
	w.mu.Lock()
	still := w.sandboxLocked(name)
	var err error
	if still != nil {
		if still.starting {
			err = nil // stopped as soon as its start returns
		} else {
			err = fmt.Errorf("sandbox %s did not stop: %s; Tentmaker keeps retrying", name, still.StopErr)
		}
	}
	w.mu.Unlock()
	if serr := w.save(); serr != nil {
		w.log.Error("state not saved", "error", serr)
	}
	return err
}

// stopSandboxes stops every sandbox, for a daemon that stops or restarts:
// a sandbox never outlives the daemon that served it.
func (w *Worker) stopSandboxes(ctx context.Context, why string) {
	w.mu.Lock()
	var sbx []*jobRec
	for _, j := range w.st.Jobs {
		if j.Kind == core.KindSandbox {
			sbx = append(sbx, j)
		}
	}
	w.mu.Unlock()
	for _, j := range sbx {
		w.log.Info("stopping a sandbox", "sandbox", j.Name, "why", why)
		w.stopJob(ctx, j, why)
	}
}
