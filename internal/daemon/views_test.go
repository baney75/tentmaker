package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/observe"
	"github.com/baney75/tentmaker/internal/runs"
)

// The control API serves the metrics and the status page from the daemon's
// own status and history, and neither carries the forge token, the control
// token or a job's JIT config.
func TestDaemonViews(t *testing.T) {
	r := newRig(t, nil)
	r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "build", Labels: []string{"self-hosted", "tentmaker", "linux"}})
	r.start()
	defer r.stop()
	r.eventually("a runner to start", func() bool { return len(r.ex.Running()) == 1 })
	spec, ok := r.ex.Spec(r.ex.Running()[0])
	if !ok || len(spec.Grant.Secret) == 0 {
		t.Fatal("no grant secret to look for")
	}
	ctl, err := control.ReadToken(control.TokenPath(r.paths.State))
	if err != nil {
		t.Fatal(err)
	}
	r.store.mu.Lock()
	forge := string(r.store.m["github-token"])
	r.store.mu.Unlock()

	get := func(path string) (string, http.Header) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.d.ControlAddress()+path, nil)
		req.Header.Set("Authorization", "Bearer "+ctl)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
		}
		return string(b), resp.Header
	}

	st := r.d.Status(context.Background())
	if st.Decision.Profile == "" {
		t.Fatal("status has no profile")
	}
	metrics, h := get("/metrics")
	if h.Get("Content-Type") != observe.MetricsContentType {
		t.Fatalf("metrics content type %q", h.Get("Content-Type"))
	}
	for _, want := range []string{
		`tentmaker_info{device="box",version="1.2.3"`,
		`tentmaker_jobs_running{device="box"} 1`,
		`tentmaker_budget_profile{device="box",profile="` + st.Decision.Profile + `"} 1`,
		fmt.Sprintf(`tentmaker_budget_cpus{device="box"} %g`, st.Decision.Limit.CPUs),
		`tentmaker_jobs_queued{device="box",forge="github",repo="baney75/app",mode="runner"}`,
		`tentmaker_slots{device="box",state="used"} 1`,
		`tentmaker_job_start_latency_seconds_count{device="box"}`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s\n%s", want, metrics)
		}
	}

	page, h := get("/")
	if !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("Content-Security-Policy") == "" {
		t.Fatalf("page headers %v", h)
	}
	for _, want := range []string{"<h3>box</h3>", ">" + st.Decision.Profile + "</span>", "baney75/app · build", "Running jobs"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	status, _ := get("/status")

	secrets := []struct{ what, value string }{
		{"forge credential", forge}, {"control credential", ctl}, {"JIT config", string(spec.Grant.Secret)},
	}
	for name, out := range map[string]string{"metrics": metrics, "page": page, "/status": status} {
		for _, s := range secrets {
			if len(s.value) < 8 {
				t.Fatalf("%s too short to look for", s.what)
			}
			if strings.Contains(out, s.value) {
				t.Errorf("%s carries the %s", name, s.what)
			}
		}
	}
}

// The views read Status through its JSON, the status --json contract; a
// renamed field would leave them blank.
func TestViewsReadStatusJSON(t *testing.T) {
	r := newRig(t, nil)
	r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "build", Labels: []string{"self-hosted", "tentmaker", "linux"}})
	r.start()
	defer r.stop()
	r.eventually("a runner to start", func() bool { return len(r.ex.Running()) == 1 })
	st := r.d.Status(context.Background())
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := observe.ParseDevice(raw)
	if !ok {
		t.Fatal("ParseDevice refused the daemon's status")
	}
	if d.Device != "box" || d.Version != "1.2.3" || d.Decision.Profile != st.Decision.Profile ||
		d.Decision.Limit != st.Decision.Limit || d.StartedAt.IsZero() ||
		d.Worker == nil || len(d.Worker.Running) != 1 || d.Broker == nil || len(d.Broker.Repos) != 1 {
		t.Fatalf("parsed %+v from %s", d, raw)
	}
}

// metricsValue reads one sample of the daemon's /metrics, by its exact
// name and labels as written.
func metricsValue(t *testing.T, r *rig, series string) float64 {
	t.Helper()
	in, err := viewsSource{d: r.d}.MetricsInput(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(observe.Metrics(in)), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			var f float64
			if _, err := fmt.Sscan(v, &f); err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return f
		}
	}
	t.Fatalf("no %s in the metrics", series)
	return 0
}

// The job counters are cumulative, not a count of the records the history
// keeps: jobs past history_max still add to them (review sol-F1).
func TestMetricsCountPastHistoryMax(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.HistoryMax = 3 })
	r.start()
	defer r.stop()
	h := r.d.currentHistory()
	const success = `tentmaker_jobs_finished_total{device="box",result="success"}`
	for i := 1; i <= 7; i++ {
		h.JobEnded(runs.Record{ID: fmt.Sprintf("tm-box-%d", i), Kind: core.KindCheck, Repo: "baney75/app", Device: "box",
			Conclusion: runs.Success, StartedAt: time.Now(), EndedAt: time.Now()}, nil, nil)
		if got := metricsValue(t, r, success); got != float64(i) {
			t.Fatalf("after %d successes the counter is %v (history keeps %d)", i, got, h.store.Len())
		}
	}
	if h.store.Len() != 3 {
		t.Fatalf("history keeps %d records, want 3", h.store.Len())
	}
	// A reload that rebuilds the history keeps the counters.
	r.cfg.HistoryMax = 4
	r.save()
	if err := r.d.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.d.currentHistory() == h {
		t.Fatal("the reload did not rebuild the history")
	}
	if got := metricsValue(t, r, success); got != 7 {
		t.Fatalf("after a reload the counter is %v, want 7", got)
	}
}

// A runner job whose broker reads its forge result is counted once, when
// that result comes or the broker's retry budget is spent, and a result
// that comes after that moves nothing (review first-F1, sol-F2): no series
// ever goes down.
func TestMetricsLateForgeResultMovesNothing(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	r := newRig(t, nil)
	r.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	r.start()
	defer r.stop()
	r.eventually("the broker", func() bool { return r.d.currentBroker() != nil })
	h := r.d.currentHistory()
	series := func(res string) string {
		return `tentmaker_jobs_finished_total{device="box",result="` + res + `"}`
	}
	all := []string{runs.Success, runs.Failure, runs.Cancelled, runs.Error, runs.Unknown}
	last := map[string]float64{}
	check := func(when string, want map[string]float64) {
		t.Helper()
		for _, res := range all {
			got := metricsValue(t, r, series(res))
			if got < last[res] {
				t.Fatalf("%s: %s went down from %v to %v", when, res, last[res], got)
			}
			if got != want[res] {
				t.Fatalf("%s: %s = %v, want %v", when, res, got, want[res])
			}
			last[res] = got
		}
	}
	ended := func(id, concl string) {
		h.JobEnded(runs.Record{ID: id, Kind: core.KindRunner, Forge: "github", Repo: "baney75/app", Device: "box",
			Conclusion: concl, LogSource: runs.LogRunnerDiag, StartedAt: now, EndedAt: now}, nil, nil)
	}

	// Tentmaker stopped it, so the diag log says cancelled; the forge says
	// failure in time. Counted once, as the failure.
	ended("tm-box-1", runs.Cancelled)
	check("waiting for the result", map[string]float64{})
	if got := metricsValue(t, r, `tentmaker_jobs_result_pending{device="box"}`); got != 1 {
		t.Fatalf("pending %v, want 1", got)
	}
	advance(3 * time.Minute) // past runs.ForgeGrace, inside the broker's budget
	check("past ForgeGrace", map[string]float64{})
	h.ForgeReport(runs.ForgeReport{ID: "tm-box-1", Device: "box", Repo: "baney75/app", Conclusion: "failure"})
	check("after the result", map[string]float64{runs.Failure: 1})

	// No result within the broker's whole budget: counted as unknown. The
	// result that comes later changes no series.
	ended("tm-box-2", runs.Unknown)
	advance(dispatch.ReportBudget)
	check("after the budget", map[string]float64{runs.Failure: 1, runs.Unknown: 1})
	h.ForgeReport(runs.ForgeReport{ID: "tm-box-2", Device: "box", Repo: "baney75/app", Conclusion: "success"})
	check("after the late result", map[string]float64{runs.Failure: 1, runs.Unknown: 1})
	if rec, _ := h.store.Get("tm-box-2"); rec.Conclusion != runs.Success {
		t.Fatalf("the history still takes the late result: %+v", rec)
	}

	// A repo no broker here reads results for counts at once.
	ended2 := runs.Record{ID: "tm-box-3", Kind: core.KindRunner, Forge: "github", Repo: "someone/else", Device: "box",
		Conclusion: runs.Success, LogSource: runs.LogRunnerDiag, StartedAt: now, EndedAt: now}
	h.JobEnded(ended2, nil, nil)
	check("a peer broker's job", map[string]float64{runs.Failure: 1, runs.Unknown: 1, runs.Success: 1})
}

// The mesh and the metrics listener serve this device's own status and
// history alone: a caller one device admits may not be one its peers admit
// (review first-F3).
func TestLocalViewsServeThisDeviceAlone(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	defer r.stop()
	h := r.d.currentHistory()
	h.JobEnded(runs.Record{ID: "tm-box-1", Kind: core.KindCheck, Repo: "baney75/app", Device: "box",
		Conclusion: runs.Failure, StartedAt: time.Now(), EndedAt: time.Now()}, nil, nil)
	// A failed forge report this broker kept for a job another device ran
	// stays off this device's page (review sol-F1).
	h.ForgeReport(runs.ForgeReport{ID: "tm-peer-1", Device: "peer", Repo: "baney75/app", Conclusion: "failure"})
	if _, ok := h.store.Get("tm-peer-1"); !ok {
		t.Fatal("the broker kept no record of the peer's job; the test proves nothing")
	}
	in, err := viewsSource{d: r.d, local: true}.PageInput(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Devices) != 1 || in.Devices[0].Device != "box" || !in.Devices[0].Self {
		t.Fatalf("local devices %+v", in.Devices)
	}
	if len(in.Failures) != 1 || in.Failures[0].ID != "tm-box-1" {
		t.Fatalf("local failures %+v", in.Failures)
	}
	page, err := observe.Page(in)
	if err != nil || !strings.Contains(string(page), "<h3>box</h3>") {
		t.Fatalf("local page lacks this device")
	}
}

// metrics_listen serves /metrics alone to a process on this device and
// refuses a CI job (review second-F4).
func TestMetricsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	r := newRig(t, func(c *config.Config) { c.MetricsListen = addr })
	var runner atomic.Bool
	r.metricsIdent = func(net.Conn) (control.Caller, error) {
		if runner.Load() {
			return control.Caller{Runner: true, Who: "uid 900"}, nil
		}
		return control.Caller{Who: "uid 1000"}, nil
	}
	r.start()
	defer r.stop()
	if st := r.d.Status(context.Background()); len(st.Problems) != 0 {
		t.Fatalf("problems %+v", st.Problems)
	}
	r.eventually("the metrics listener", func() bool { return r.d.MetricsAddress() == addr })
	get := func(path string) (int, string) {
		t.Helper()
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := c.Get("http://" + r.d.MetricsAddress() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/metrics"); code != 200 || !strings.Contains(body, `tentmaker_info{device="box"`) {
		t.Fatalf("GET /metrics: %d\n%s", code, body)
	}
	for _, p := range []string{"/", "/status", "/v1/status"} {
		if code, _ := get(p); code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, code)
		}
	}
	runner.Store(true)
	if code, _ := get("/metrics"); code != http.StatusForbidden {
		t.Fatalf("a CI job got %d", code)
	}
}
