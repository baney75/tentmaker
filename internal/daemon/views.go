package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/baney75/tentmaker/internal/observe"
	"github.com/baney75/tentmaker/internal/runs"
)

// pageRuns is how much of the pool's history the status page reads: running
// jobs come first, then the newest ended ones, where it finds the last
// failures.
const pageRuns = 200

// viewsSource is the daemon as internal/observe reads it. Both views come
// from the daemon's Status (the JSON `tentmaker status --json` prints),
// its pool view and its run history: no counter of their own.
type viewsSource struct{ d *Daemon }

// MetricsInput is this device's status and the jobs it ran since the
// daemon started.
func (v viewsSource) MetricsInput(ctx context.Context) (observe.MetricsInput, error) {
	raw, err := json.Marshal(v.d.Status(ctx))
	if err != nil {
		return observe.MetricsInput{}, err
	}
	self, ok := observe.ParseDevice(raw)
	if !ok {
		return observe.MetricsInput{}, errors.New("status has no device name")
	}
	in := observe.MetricsInput{Now: v.d.o.Now(), Self: self}
	if h := v.d.currentHistory(); h != nil {
		in.Runs = h.store.List(runs.Query{Since: self.StartedAt})
	}
	return in, nil
}

// PageInput is the pool view and the pool's recent runs.
func (v viewsSource) PageInput(ctx context.Context) (observe.PageInput, error) {
	cfg := v.d.Config()
	in := observe.PageInput{Now: v.d.o.Now(), Self: v.d.device()}
	if cfg.Mesh.Enabled {
		in.Pool = cfg.Mesh.Pool
	}
	devs, err := v.d.Pool(ctx)
	if err != nil {
		return observe.PageInput{}, err
	}
	in.Devices = devs
	h := ctlHandler{v.d}
	a, err := h.Runs(ctx, runs.Query{Limit: pageRuns})
	if err != nil {
		in.Errors = append(in.Errors, "run history: "+err.Error())
	}
	in.Runs, in.Errors = a.Records, append(in.Errors, a.Errors...)
	return in, nil
}

// Views serves the metrics and the status page; the control API and the
// mesh mount it behind their own caller checks.
func (d *Daemon) Views() http.Handler { return observe.Handler(viewsSource{d}) }

// Views implements control.ViewsHandler.
func (h ctlHandler) Views() http.Handler { return h.d.Views() }
