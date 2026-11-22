package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// A job that ends is kept in run history with its redacted log; the
// control API lists it, serves its log and reruns its run through the
// local broker's forge.
func TestDaemonRunHistory(t *testing.T) {
	r := newRig(t, func(c *config.Config) {
		c.Mesh.Enabled = false
		c.Alerts = []config.Alert{{URLSecret: "no-such-secret", On: "failure"}}
	})
	r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "build", RunID: 77, Labels: []string{"self-hosted", "tentmaker", "linux"}})
	r.start()
	defer r.stop()
	r.eventually("a runner to start", func() bool { return len(r.ex.Running()) == 1 })
	name := r.ex.Running()[0]
	ctx := context.Background()
	c := r.client()

	var a runs.Answer
	if err := c.Get(ctx, "/v1/runs", &a); err != nil || len(a.Records) != 1 || a.Records[0].Conclusion != runs.Running {
		t.Fatalf("running = %+v %v", a, err)
	}
	r.ex.SetLog(name, []byte("token ghp_abcdefghijklmnopqrstuvwxyz0123456789\nStep result: Failed\nJob result after all job steps finish: Failed\n"))
	var l runs.LogAnswer
	if err := c.Get(ctx, "/v1/runs/log?id="+name, &l); err != nil || !l.Running || strings.Contains(string(l.Data), "ghp_") {
		t.Fatalf("live log = %+v %v", l, err)
	}

	r.ex.Exit(name)
	r.eventually("the job to be kept", func() bool {
		var a runs.Answer
		return c.Get(ctx, "/v1/runs?failed=1&repo=baney75/app", &a) == nil && len(a.Records) == 1 && a.Records[0].Ended()
	})
	if err := c.Get(ctx, "/v1/runs/log?id="+name, &l); err != nil || l.Running || !strings.Contains(string(l.Data), "***") ||
		strings.Contains(string(l.Data), "ghp_") {
		t.Fatalf("kept log = %+v %v", l, err)
	}
	var e *control.Error
	if err := c.Get(ctx, "/v1/runs/log?id=tm-nobody-1", &l); !errors.As(err, &e) || e.Status != 404 {
		t.Fatalf("missing log: %v", err)
	}

	var ra control.RerunAnswer
	if err := c.PostFor(ctx, "/v1/rerun", control.RerunRequest{Repo: "baney75/app", RunID: 77}, &ra); err != nil || ra.Broker != "box" {
		t.Fatalf("rerun = %+v %v", ra, err)
	}
	if got := r.forge.Reruns("baney75/app"); len(got) != 1 || got[0] != 77 {
		t.Fatalf("forge reruns = %v", got)
	}
	if err := c.PostFor(ctx, "/v1/rerun", control.RerunRequest{Repo: "someone/else", RunID: 1}, &ra); err == nil {
		t.Fatalf("rerun of a repo with no forge worked")
	}

	var res []map[string]any
	if err := c.PostFor(ctx, "/v1/alerts/test", nil, &res); err != nil || len(res) != 1 || res[0]["ok"] != false ||
		!strings.Contains(res[0]["error"].(string), "not readable") {
		t.Fatalf("alerts test = %v %v", res, err)
	}
}

// plainExec hides the fake's JobInspector, as the darwin executor has
// none: no diag log, no usage.
type plainExec struct{ core.Executor }

// A runner job on an executor that can't show its log takes its
// conclusion, failed step and log from the forge through the broker: the
// record is a failure with the test output (redacted), the failure alert
// fires, and wait_for_run's look sees the failure and the tail.
func TestDaemonForgeResult(t *testing.T) {
	r := newRig(t, func(c *config.Config) {
		c.Mesh.Enabled = false
		c.Alerts = []config.Alert{{URLSecret: "no-such-secret", On: "failure"}}
	})
	r.executors = func(config.Config, func(core.Platform) (string, error), *slog.Logger) ([]core.Executor, []core.Problem) {
		return []core.Executor{plainExec{r.ex}}, nil
	}
	j := r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "test", RunID: 78, HeadSHA: "abcdef1234567",
		Labels: []string{"self-hosted", "tentmaker", "linux"}})
	forgeLog := "##[group]Run go test ./...\n--- FAIL: TestParse (0.00s)\n    parse_test.go:12: got ghp_abcdefghijklmnopqrstuvwxyz0123456789\n" +
		"FAIL\tgithub.com/baney75/app\t0.01s\n##[error]Process completed with exit code 1.\n"
	r.forge.SetReport("baney75/app", j.ID, core.JobReport{Conclusion: "failure", FailedStep: "Run go test ./...",
		Log: []byte(forgeLog)}, nil)
	r.start()
	defer r.stop()
	r.eventually("a runner to start", func() bool { return len(r.ex.Running()) == 1 })
	name := r.ex.Running()[0]
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.eventually("the broker to see the job", func() bool {
		for _, g := range r.d.currentBroker().Status().Runners {
			if g.Name == name && g.TookJob == j.ID {
				return true
			}
		}
		return false
	})
	r.forge.Finish(name)
	r.ex.Exit(name)

	ctx := context.Background()
	c := r.client()
	look := func(ctx context.Context) (runs.Snapshot, error) {
		var a runs.Answer
		err := c.Get(ctx, "/v1/runs?id="+name, &a)
		return runs.Snapshot{Records: a.Records}, err
	}
	res, err := runs.Wait(ctx, look, runs.WaitOptions{Timeout: 20 * time.Second, Poll: 100 * time.Millisecond, Settle: -1})
	if err != nil || !res.Done || res.Conclusion != runs.Failure || len(res.Records) != 1 {
		t.Fatalf("wait = %+v %v", res, err)
	}
	got := res.Records[0]
	if got.FailedStep != "Run go test ./..." || got.LogSource != runs.LogForge {
		t.Fatalf("record = %+v", got)
	}
	var l runs.LogAnswer
	if err := c.Get(ctx, "/v1/runs/log?id="+name, &l); err != nil {
		t.Fatal(err)
	}
	tail := strings.Join(runs.TailLines(string(l.Data), 20), "\n")
	if !strings.Contains(tail, "--- FAIL: TestParse") || strings.Contains(tail, "ghp_") || !strings.Contains(tail, runs.Mask) {
		t.Fatalf("log tail = %q", tail)
	}
	r.eventually("the failure alert", func() bool { return r.d.currentHistory().alerts.Counts().Failed >= 1 })
}

// A forge result whose log could not be read keeps the runner's diag log
// and says so; a forge result before the worker's record still lands.
func TestHistoryForgeFallback(t *testing.T) {
	h := &history{store: runs.Open(t.TempDir(), 10), log: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		device: "box", early: map[string]runs.ForgeReport{}}
	diag := "[x] Processing step: DisplayName='go test'\n[x] Step result: Failed\n"
	h.JobEnded(runs.Record{ID: "tm-box-1", Kind: core.KindRunner, Repo: "o/r", Device: "box", Conclusion: runs.Unknown,
		LogSource: runs.LogRunnerDiag}, []byte(diag))
	h.ForgeReport(runs.ForgeReport{ID: "tm-box-1", Device: "box", Repo: "o/r", Conclusion: "failure"})
	rec, _ := h.store.Get("tm-box-1")
	data, err := h.store.Log("tm-box-1")
	if rec.Conclusion != runs.Failure || rec.LogSource != runs.LogRunnerDiag || err != nil || string(data) != diag {
		t.Fatalf("record = %+v, log = %q %v", rec, data, err)
	}

	h.ForgeReport(runs.ForgeReport{ID: "tm-box-2", Device: "box", Repo: "o/r", Conclusion: "success", Log: []byte("ok\n")})
	h.JobEnded(runs.Record{ID: "tm-box-2", Kind: core.KindRunner, Repo: "o/r", Device: "box", Conclusion: runs.Unknown,
		LogSource: runs.LogRunnerDiag}, nil)
	rec, _ = h.store.Get("tm-box-2")
	if data, _ := h.store.Log("tm-box-2"); rec.Conclusion != runs.Success || rec.LogSource != runs.LogForge || string(data) != "ok\n" {
		t.Fatalf("early record = %+v, log = %q", rec, data)
	}
}
