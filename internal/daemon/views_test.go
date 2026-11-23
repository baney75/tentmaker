package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/observe"
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
