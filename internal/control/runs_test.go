package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

func (f *fakeHandler) Runs(ctx context.Context, q runs.Query) (runs.Answer, error) {
	return runs.Answer{Records: []runs.Record{{ID: "tm-a-1", Repo: q.Repo, Conclusion: runs.Failure}}, Queued: []string{"ci / lint"}}, nil
}

func (f *fakeHandler) RunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error) {
	if id != "tm-a-1" {
		return runs.LogAnswer{}, core.ErrNotFound
	}
	return runs.LogAnswer{ID: id, Device: "box", Size: 5, Data: []byte("hello")}, nil
}

func (f *fakeHandler) Rerun(ctx context.Context, req RerunRequest) (RerunAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reruns = append(f.reruns, req)
	return RerunAnswer{Repo: req.Repo, RunID: req.RunID, Broker: "box"}, nil
}

func (f *fakeHandler) AlertsTest(ctx context.Context) (any, error) {
	return []map[string]any{{"alert": 0, "ok": true}}, nil
}

func TestRunsRoutes(t *testing.T) {
	h, c, _ := start(t)
	ctx := context.Background()
	var a runs.Answer
	if err := c.Get(ctx, "/v1/runs?repo=o/r&failed=1", &a); err != nil || len(a.Records) != 1 || a.Records[0].Repo != "o/r" || len(a.Queued) != 1 {
		t.Fatalf("runs = %+v %v", a, err)
	}
	var e *Error
	if err := c.Get(ctx, "/v1/runs?limit=x", &a); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatalf("bad query: %v", err)
	}

	var l runs.LogAnswer
	if err := c.Get(ctx, "/v1/runs/log?id=tm-a-1", &l); err != nil || string(l.Data) != "hello" {
		t.Fatalf("log = %+v %v", l, err)
	}
	if err := c.Get(ctx, "/v1/runs/log?id=tm-zz", &l); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("missing log: %v", err)
	}
	if err := c.Get(ctx, "/v1/runs/log?id=../x", &l); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatalf("bad id: %v", err)
	}
	// A local reader without the token sees the list and the redacted log,
	// but can't rerun.
	noTok := &Client{Addr: c.Addr}
	if err := noTok.Get(ctx, "/v1/runs", &a); err != nil {
		t.Fatalf("runs without the token: %v", err)
	}
	if err := noTok.Get(ctx, "/v1/runs/log?id=tm-a-1", &l); err != nil {
		t.Fatalf("log without the token: %v", err)
	}
	if err := noTok.PostFor(ctx, "/v1/rerun", RerunRequest{Repo: "o/r", RunID: 9}, nil); err == nil {
		t.Fatalf("rerun without the token worked")
	}

	var ra RerunAnswer
	if err := c.PostFor(ctx, "/v1/rerun", RerunRequest{Repo: "o/r", RunID: 9}, &ra); err != nil || ra.Broker != "box" {
		t.Fatalf("rerun = %+v %v", ra, err)
	}
	if err := c.PostFor(ctx, "/v1/rerun", map[string]any{"repo": "o/r"}, &ra); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatalf("rerun without a run id: %v", err)
	}
	var res []map[string]any
	if err := c.PostFor(ctx, "/v1/alerts/test", nil, &res); err != nil || len(res) != 1 {
		t.Fatalf("alerts test = %v %v", res, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.reruns) != 1 {
		t.Fatalf("reruns = %+v", h.reruns)
	}
}
