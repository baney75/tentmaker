package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/observe"
	"github.com/baney75/tentmaker/internal/runs"
)

// pageRuns is how much of the history the status page reads for its
// running jobs, which sort first. Failures are read apart (pageFailures).
const pageRuns = 200

// viewsSource is the daemon as internal/observe reads it: its Status (the
// JSON `tentmaker status --json` prints), its job counters (d.tally) and
// its run history. local serves this device's own data alone, as the mesh
// does: a caller this device admits may not be one a peer admits, so it
// never relays a peer's status or history.
type viewsSource struct {
	d     *Daemon
	local bool
}

// MetricsInput is this device's status and job counters.
func (v viewsSource) MetricsInput(ctx context.Context) (observe.MetricsInput, error) {
	raw, err := json.Marshal(v.d.Status(ctx))
	if err != nil {
		return observe.MetricsInput{}, err
	}
	self, ok := observe.ParseDevice(raw)
	if !ok {
		return observe.MetricsInput{}, errors.New("status has no device name")
	}
	now := v.d.o.Now()
	return observe.MetricsInput{Now: now, Self: self, Jobs: v.d.tally.Counts(now)}, nil
}

// PageInput is the pool view (this device alone when local), its running
// jobs and its failures.
func (v viewsSource) PageInput(ctx context.Context) (observe.PageInput, error) {
	cfg := v.d.Config()
	now := v.d.o.Now()
	in := observe.PageInput{Now: now}
	if cfg.Mesh.Enabled {
		in.Pool = cfg.Mesh.Pool
	}
	if v.local {
		raw, _ := json.Marshal(v.d.Status(ctx))
		in.Devices = []mesh.DeviceStatus{{Device: v.d.device(), Self: true, Broker: v.d.currentBroker() != nil,
			Version: v.d.o.Version, Status: raw}}
	} else {
		devs, err := v.d.Pool(ctx)
		if err != nil {
			return observe.PageInput{}, err
		}
		in.Devices = devs
	}
	// Running jobs, the last failures and every failure of the last day,
	// read at once: newer successes can't push a failure out of a read
	// that asks for failures alone.
	queries := []runs.Query{
		{Limit: pageRuns},
		{Failed: true, Limit: observe.MaxFailures},
		{Failed: true, Since: now.Add(-24 * time.Hour)},
	}
	answers := make([]runs.Answer, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i], errs[i] = v.runs(ctx, q)
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	note := func(s string) {
		if !seen[s] {
			seen[s] = true
			in.Errors = append(in.Errors, s)
		}
	}
	for i, a := range answers {
		if errs[i] != nil {
			note("run history: " + errs[i].Error())
		}
		for _, e := range a.Errors {
			note(e)
		}
		in.HistoryFrom = runs.Later(in.HistoryFrom, a.HistoryFrom)
	}
	in.Runs = answers[0].Records
	in.Failures = runs.Merge(answers[1].Records, answers[2].Records)
	return in, nil
}

func (v viewsSource) runs(ctx context.Context, q runs.Query) (runs.Answer, error) {
	if v.local {
		return v.d.localRuns(q), nil
	}
	return ctlHandler{v.d}.Runs(ctx, q)
}

// Views serves the metrics and the pool's status page; the control API
// mounts it behind its own caller checks.
func (d *Daemon) Views() http.Handler { return observe.Handler(viewsSource{d: d}) }

// LocalViews serves the metrics and a status page of this device alone,
// for the mesh peer API and the loopback metrics listener.
func (d *Daemon) LocalViews() http.Handler { return observe.Handler(viewsSource{d: d, local: true}) }

// Views implements control.ViewsHandler.
func (h ctlHandler) Views() http.Handler { return h.d.Views() }
