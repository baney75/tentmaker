// Package fake is an in-memory core.Executor for tests. It records every
// call, lets a test set runner states, fail starts, return canned check
// results and leave runners behind for reconcile.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/baney75/tentmaker/internal/core"
)

// ErrResetWhileRunning is what Reset returns when a runner is still running.
// The contract says Reset only runs when nothing does, so a test that sees it
// found a bug.
var ErrResetWhileRunning = errors.New("reset while a runner is running")

// Run is one runner the fake started (or was told about).
type Run struct {
	Spec    core.ExecSpec
	State   core.ExecState
	Stopped bool
}

// Call is one recorded method call.
type Call struct {
	Method string
	Name   string
}

// Executor is the fake. Create it with New.
type Executor struct {
	mu        sync.Mutex
	name      string
	platforms []core.Platform
	maxSlots  int

	runs       map[string]*Run
	startErrs  []error // consumed one per Start
	startErr   error   // every Start, after startErrs
	stateErrs  map[string]error
	results    map[string]core.CheckResult
	resultErrs map[string]error
	resetErr   error
	listErr    error
	stopErr    error
	cleanErr   error
	calls      []Call
	resets     int
	violations []string

	startGate chan struct{} // Start waits for it to close; nil: no wait

	poolLimit core.Resources
	poolInUse core.Resources
	poolCalls int

	cachesRepos bool
}

var _ core.Executor = (*Executor)(nil)
var _ core.RepoCacher = (*Executor)(nil)

// SetCachesRepos says whether the executor keeps repo caches (default no).
func (e *Executor) SetCachesRepos(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cachesRepos = on
}

// CachesRepos implements core.RepoCacher.
func (e *Executor) CachesRepos() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cachesRepos
}

// New returns an executor called name with maxSlots (0 = no cap) that runs
// the given platforms.
func New(name string, maxSlots int, platforms ...core.Platform) *Executor {
	return &Executor{
		name:       name,
		platforms:  append([]core.Platform(nil), platforms...),
		maxSlots:   maxSlots,
		runs:       map[string]*Run{},
		stateErrs:  map[string]error{},
		results:    map[string]core.CheckResult{},
		resultErrs: map[string]error{},
	}
}

// ---------------------------------------------------------------- controls

// FailStarts makes the next len(errs) Start calls fail with those errors.
func (e *Executor) FailStarts(errs ...error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.startErrs = append(e.startErrs, errs...)
}

// SetStartErr makes every Start fail with err (nil clears it).
func (e *Executor) SetStartErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.startErr = err
}

// SetState sets a runner's state, adding the runner if it is unknown.
func (e *Executor) SetState(name string, st core.ExecState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.runs[name]
	if r == nil {
		r = &Run{Spec: core.ExecSpec{Name: name}}
		e.runs[name] = r
	}
	r.State = st
}

// Exit marks a runner as exited (the job finished).
func (e *Executor) Exit(name string) { e.SetState(name, core.ExecExited) }

// SetStateErr makes State fail for name (nil clears it).
func (e *Executor) SetStateErr(name string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		delete(e.stateErrs, name)
		return
	}
	e.stateErrs[name] = err
}

// SetResult sets what CheckResult returns for name.
func (e *Executor) SetResult(name string, r core.CheckResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.results[name] = r
}

// SetResultErr makes CheckResult fail for name.
func (e *Executor) SetResultErr(name string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resultErrs[name] = err
}

// SetResetErr makes Reset fail with err (nil clears it).
func (e *Executor) SetResetErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetErr = err
}

// SetListErr makes List fail with err (nil clears it).
func (e *Executor) SetListErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.listErr = err
}

// HoldStarts makes every Start wait, as a slow sandbox boot does, until
// the returned release is called or the Start's context ends. release is
// safe to call more than once.
func (e *Executor) HoldStarts() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.startGate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			if e.startGate == gate {
				e.startGate = nil
			}
			e.mu.Unlock()
			close(gate)
		})
	}
}

// SetStopErr makes Stop fail with err, leaving the runner as it was (nil
// clears it).
func (e *Executor) SetStopErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopErr = err
}

// SetCleanErr makes Clean fail with err (nil clears it).
func (e *Executor) SetCleanErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cleanErr = err
}

// AddLeftover adds a running runner the worker never started, as if it were
// left over from before a restart.
func (e *Executor) AddLeftover(name string) { e.SetState(name, core.ExecRunning) }

// Runs returns a copy of every runner the fake knows (cleaned ones are gone).
func (e *Executor) Runs() map[string]Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]Run{}
	for k, v := range e.runs {
		out[k] = *v
	}
	return out
}

// Running names the runners in state running or unknown.
func (e *Executor) Running() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for k, v := range e.runs {
		if v.State == core.ExecRunning || v.State == core.ExecUnknown {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Spec returns the spec a runner was started with.
func (e *Executor) Spec(name string) (core.ExecSpec, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.runs[name]
	if r == nil {
		return core.ExecSpec{}, false
	}
	return r.Spec, true
}

// Calls lists recorded calls to method ("" = all).
func (e *Executor) Calls(method string) []Call {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Call
	for _, c := range e.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Resets counts successful Reset calls.
func (e *Executor) Resets() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resets
}

// Violations lists contract violations the fake saw (Reset while running).
func (e *Executor) Violations() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.violations...)
}

// PoolLimit returns the last SetPoolLimit arguments and the call count.
func (e *Executor) PoolLimit() (limit, inUse core.Resources, calls int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.poolLimit, e.poolInUse, e.poolCalls
}

// ---------------------------------------------------------------- core.Executor

func (e *Executor) record(method, name string) { e.calls = append(e.calls, Call{method, name}) }

// Name implements core.Executor.
func (e *Executor) Name() string { return e.name }

// Platforms implements core.Executor.
func (e *Executor) Platforms() []core.Platform { return append([]core.Platform(nil), e.platforms...) }

// MaxSlots implements core.Executor.
func (e *Executor) MaxSlots() int { return e.maxSlots }

// Preflight implements core.Executor.
func (e *Executor) Preflight(ctx context.Context) []core.Problem { return nil }

// Start implements core.Executor. It copies the secret, so a caller that
// wipes its buffer doesn't change what the test sees.
func (e *Executor) Start(ctx context.Context, spec core.ExecSpec) error {
	e.mu.Lock()
	gate := e.startGate
	e.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("Start", spec.Name)
	if !core.NamePattern.MatchString(spec.Name) {
		return fmt.Errorf("bad runner name %q", spec.Name)
	}
	if r, ok := e.runs[spec.Name]; ok && r.State != core.ExecMissing {
		return fmt.Errorf("runner %s already exists", spec.Name)
	}
	if len(e.startErrs) > 0 {
		err := e.startErrs[0]
		e.startErrs = e.startErrs[1:]
		if err != nil {
			return err
		}
	} else if e.startErr != nil {
		return e.startErr
	}
	if e.maxSlots > 0 {
		n := 0
		for _, r := range e.runs {
			if r.State == core.ExecRunning || r.State == core.ExecUnknown {
				n++
			}
		}
		if n >= e.maxSlots {
			e.violations = append(e.violations, "start beyond MaxSlots: "+spec.Name)
			return fmt.Errorf("all %d slots in use", e.maxSlots)
		}
	}
	spec.Grant.Secret = append([]byte(nil), spec.Grant.Secret...)
	e.runs[spec.Name] = &Run{Spec: spec, State: core.ExecRunning}
	return nil
}

// State implements core.Executor. An unknown name is missing.
func (e *Executor) State(ctx context.Context, name string) (core.ExecState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("State", name)
	if err := e.stateErrs[name]; err != nil {
		return core.ExecUnknown, err
	}
	r := e.runs[name]
	if r == nil {
		return core.ExecMissing, nil
	}
	return r.State, nil
}

// Stop implements core.Executor.
func (e *Executor) Stop(ctx context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("Stop", name)
	if e.stopErr != nil {
		return e.stopErr
	}
	if r := e.runs[name]; r != nil {
		r.Stopped = true
		r.State = core.ExecExited
	}
	return nil
}

// Clean implements core.Executor: the runner is forgotten.
func (e *Executor) Clean(ctx context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("Clean", name)
	if e.cleanErr != nil {
		return e.cleanErr
	}
	if r := e.runs[name]; r != nil && (r.State == core.ExecRunning || r.State == core.ExecUnknown) {
		e.violations = append(e.violations, "clean while running: "+name)
	}
	delete(e.runs, name)
	return nil
}

// List implements core.Executor.
func (e *Executor) List(ctx context.Context) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("List", "")
	if e.listErr != nil {
		return nil, e.listErr
	}
	var out []string
	for k := range e.runs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Reset implements core.Executor. It fails (and records a violation) when a
// runner is still running.
func (e *Executor) Reset(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("Reset", "")
	if e.resetErr != nil {
		return e.resetErr
	}
	for k, r := range e.runs {
		if r.State == core.ExecRunning || r.State == core.ExecUnknown {
			e.violations = append(e.violations, "reset while running: "+k)
			return ErrResetWhileRunning
		}
	}
	e.runs = map[string]*Run{}
	e.resets++
	return nil
}

// SetPoolLimit implements core.Executor.
func (e *Executor) SetPoolLimit(ctx context.Context, limit, inUse core.Resources) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("SetPoolLimit", "")
	e.poolLimit, e.poolInUse = limit, inUse
	e.poolCalls++
	return nil
}

// CheckResult implements core.Executor.
func (e *Executor) CheckResult(ctx context.Context, name string) (core.CheckResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("CheckResult", name)
	if err := e.resultErrs[name]; err != nil {
		return core.CheckResult{}, err
	}
	r, ok := e.results[name]
	if !ok {
		return core.CheckResult{}, fmt.Errorf("%s: no result.json", name)
	}
	return r, nil
}
