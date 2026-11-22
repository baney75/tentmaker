package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

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
