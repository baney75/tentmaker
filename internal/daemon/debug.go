package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/runs"
)

// FlakyWindow is how far back flaky and why read the pool's history.
const FlakyWindow = 14 * 24 * time.Hour

// localRunTests is job id's record and the test results this device kept
// for it; a record with no tests has empty results.
func (d *Daemon) localRunTests(id string) (runs.TestsAnswer, error) {
	if !runs.ValidID(id) {
		return runs.TestsAnswer{}, errors.New("bad job id")
	}
	h := d.currentHistory()
	if h == nil {
		return runs.TestsAnswer{}, core.ErrNotFound
	}
	rec, ok := h.store.Get(id)
	if !ok {
		return runs.TestsAnswer{}, core.ErrNotFound
	}
	t, err := h.store.Tests(id)
	switch {
	case errors.Is(err, core.ErrNotFound):
		t = runs.TestResults{ID: id, Device: rec.Device}
	case err != nil:
		return runs.TestsAnswer{}, err
	}
	if t.Cases == nil {
		t.Cases = []runs.TestCase{}
	}
	return runs.TestsAnswer{Record: rec, Results: t}, nil
}

func (s runsSource) RunTests(ctx context.Context, id string) (runs.TestsAnswer, error) {
	return s.d.localRunTests(id)
}

// Tests is job id's test results from the device that ran it.
func (h ctlHandler) Tests(ctx context.Context, id string) (runs.TestsAnswer, error) {
	if n := h.d.currentNode(); n != nil {
		return n.PoolRunTests(ctx, id)
	}
	return h.d.localRunTests(id)
}

// Flaky finds the flaky tests in the pool's records that q matches, since
// FlakyWindow ago unless q says otherwise.
func (h ctlHandler) Flaky(ctx context.Context, q runs.Query) (control.FlakyAnswer, error) {
	if q.Since.IsZero() {
		q.Since = h.d.o.Now().Add(-FlakyWindow)
	}
	q.Limit, q.Failed = runs.MaxLimit, false
	a, err := h.Runs(ctx, q)
	if err != nil {
		return control.FlakyAnswer{}, err
	}
	return control.FlakyAnswer{Since: q.Since, Tests: runs.Flaky(a.Records), Errors: a.Errors}, nil
}

// Why explains a failed job: the one named, or the newest failed job of a
// repo's commit or run. It reads the job's tests and log tail from the
// device that ran it and the repo's history for flakiness.
func (h ctlHandler) Why(ctx context.Context, req control.WhyRequest) (runs.WhyAnswer, error) {
	if err := req.Check(); err != nil {
		return runs.WhyAnswer{}, err
	}
	q := runs.Query{ID: req.ID, Repo: req.Repo, SHA: req.SHA, RunID: req.RunID, Limit: runs.MaxLimit}
	a, err := h.Runs(ctx, q)
	if err != nil {
		return runs.WhyAnswer{}, err
	}
	var rec *runs.Record
	for i := range a.Records {
		r := &a.Records[i]
		if req.ID != "" || r.Failed() {
			rec = r
			break
		}
	}
	if rec == nil {
		what := "job " + req.ID
		if req.ID == "" {
			what = "no failed job of " + req.Repo
			if req.SHA != "" {
				what += " at " + req.SHA
			} else {
				what += fmt.Sprintf(" in run %d", req.RunID)
			}
			if len(a.Records) > 0 {
				return runs.WhyAnswer{}, notFound{fmt.Errorf("%s: its %d job(s) did not fail", what, len(a.Records))}
			}
			return runs.WhyAnswer{}, notFound{errors.New(what + " in the pool's history")}
		}
		return runs.WhyAnswer{}, notFound{errors.New(what + " is not in the pool's history")}
	}
	var tests *runs.TestResults
	if t, err := h.Tests(ctx, rec.ID); err == nil && (len(t.Results.Cases) > 0 || t.Results.Dropped > 0) {
		tests = &t.Results
	}
	var log []byte
	if l, err := h.RunLog(ctx, rec.ID, runs.LogTailBytes); err == nil {
		log = l.Data
	}
	hist, err := h.Runs(ctx, runs.Query{Repo: rec.Repo, Since: h.d.o.Now().Add(-FlakyWindow), Limit: runs.MaxLimit})
	if err != nil {
		hist = runs.Answer{}
	}
	return runs.Why(*rec, tests, log, hist.Records), nil
}

// notFound is err, answered as 404.
type notFound struct{ err error }

func (e notFound) Error() string        { return e.err.Error() }
func (e notFound) Is(target error) bool { return target == core.ErrNotFound }

// debugWorker is this device's worker, for a job whose sandbox is kept
// here. A job that ran on another device is refused with where to go.
func (h ctlHandler) debugWorker(ctx context.Context, id string) (*dispatch.Worker, error) {
	if !runs.ValidID(id) {
		return nil, errors.New("bad job id")
	}
	h.d.mu.RLock()
	w := h.d.worker
	h.d.mu.RUnlock()
	if w != nil {
		for _, k := range w.KeptJobs() {
			if k.Name == id {
				return w, nil
			}
		}
	}
	self := h.d.device()
	if a, err := h.Runs(ctx, runs.Query{ID: id, Limit: 1}); err == nil && len(a.Records) == 1 {
		if dev := a.Records[0].Device; dev != "" && !strings.EqualFold(dev, self) {
			return nil, fmt.Errorf("job %s ran on device %s; run tentmaker debug there", id, dev)
		}
	}
	if w == nil {
		return nil, notFound{fmt.Errorf("%v for job %s: this device runs no jobs", dispatch.ErrNotKept, id)}
	}
	return w, nil
}

// debugErr answers a sandbox that isn't kept as 404.
func debugErr(err error) error {
	if errors.Is(err, dispatch.ErrNotKept) {
		return notFound{err}
	}
	return err
}

// DebugExec runs one command in job id's kept sandbox on this device.
func (h ctlHandler) DebugExec(ctx context.Context, req control.DebugExecRequest) (control.DebugExecAnswer, error) {
	w, err := h.debugWorker(ctx, req.ID)
	if err != nil {
		return control.DebugExecAnswer{}, err
	}
	res, err := w.DebugExec(ctx, req.ID, req.Command, time.Duration(req.TimeoutS)*time.Second, req.MaxBytes)
	if err != nil {
		return control.DebugExecAnswer{}, debugErr(err)
	}
	return control.DebugExecAnswer{ID: req.ID, ExitCode: res.ExitCode, Output: string(res.Output),
		Truncated: res.Truncated, TimedOut: res.TimedOut}, nil
}

// DebugShell runs an interactive shell in job id's kept sandbox here.
func (h ctlHandler) DebugShell(ctx context.Context, id string, stdin io.Reader, stdout io.Writer) error {
	w, err := h.debugWorker(ctx, id)
	if err != nil {
		return err
	}
	return debugErr(w.DebugShell(ctx, id, stdin, stdout))
}

// DebugRelease ends job id's grace now; the worker cleans it up.
func (h ctlHandler) DebugRelease(ctx context.Context, id string) error {
	w, err := h.debugWorker(ctx, id)
	if err != nil {
		return err
	}
	return debugErr(w.ReleaseKept(id))
}

var (
	_ control.TestsHandler = ctlHandler{}
	_ control.DebugHandler = ctlHandler{}
)
