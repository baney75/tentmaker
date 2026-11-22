package mesh

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

type fakeRuns struct {
	device string
	recs   []runs.Record
	queued []string
	logs   map[string]string
	// repos this device can rerun; reruns records what it did.
	repos  map[string]bool
	mu     sync.Mutex
	reruns []string
}

func (f *fakeRuns) Runs(q runs.Query) runs.Answer {
	a := runs.Answer{Records: runs.Apply(f.recs, q)}
	if q.Repo != "" {
		a.Queued = f.queued
	}
	return a
}

func (f *fakeRuns) RunLog(_ context.Context, id string, max int64) (runs.LogAnswer, error) {
	l, ok := f.logs[id]
	if !ok {
		return runs.LogAnswer{}, core.ErrNotFound
	}
	return runs.LogAnswer{ID: id, Device: f.device, Size: int64(len(l)), Data: []byte(l)}, nil
}

func (f *fakeRuns) Rerun(_ context.Context, repo string, runID int64) error {
	if !f.repos[repo] {
		return core.ErrNotFound
	}
	if runID == 13 {
		return errors.New("run 13 is too old to rerun")
	}
	f.mu.Lock()
	f.reruns = append(f.reruns, repo)
	f.mu.Unlock()
	return nil
}

func rec(device, id string, end time.Time, concl string) runs.Record {
	return runs.Record{ID: id, Device: device, Repo: "o/app", SHA: "abcdef1234567", StartedAt: end.Add(-time.Minute),
		EndedAt: end, Conclusion: concl}
}

// The pool view merges every device's records, newest first, and keeps a
// device that doesn't answer out without failing the rest.
func TestPoolRunsMerge(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	a := &fakeRuns{device: "a", recs: []runs.Record{rec("a", "tm-a-1", t0, runs.Success), rec("a", "tm-a-2", t0.Add(2*time.Minute), runs.Failure)}}
	b := &fakeRuns{device: "b", recs: []runs.Record{rec("b", "tm-b-1", t0.Add(time.Minute), runs.Failure),
		{ID: "tm-b-2", Device: "b", Repo: "o/app", StartedAt: t0, Conclusion: runs.Running}}, queued: []string{"ci / lint (job 7)"}}
	c := &fakeRuns{device: "c", recs: []runs.Record{rec("c", "tm-c-1", t0, runs.Success)}}
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home", runs: a},
		nodeSpec{device: "b", pool: "home", runs: b, broker: &fakeBroker{}},
		nodeSpec{device: "c", pool: "home", runs: c},
	)
	ctx := context.Background()
	ms[0].node.Brokers(ctx) // discover
	got, err := ms[0].node.PoolRuns(ctx, runs.Query{Repo: "O/APP"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got.Records {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "tm-b-2,tm-a-2,tm-b-1,tm-a-1,tm-c-1" {
		t.Fatalf("order = %v", ids)
	}
	if len(got.Queued) != 1 || len(got.Errors) != 0 {
		t.Fatalf("answer = %+v", got)
	}

	got, _ = ms[0].node.PoolRuns(ctx, runs.Query{Failed: true, Limit: 1})
	if len(got.Records) != 1 || got.Records[0].ID != "tm-a-2" || got.Queued != nil {
		t.Fatalf("failed, limit 1 = %+v", got)
	}

	ms[2].fail.Store(true)
	got, _ = ms[0].node.PoolRuns(ctx, runs.Query{})
	if len(got.Records) != 4 || len(got.Errors) != 1 || !strings.HasPrefix(got.Errors[0], "c: ") {
		t.Fatalf("with c down = %+v", got)
	}
}

func TestPoolRunLogAndRerun(t *testing.T) {
	a := &fakeRuns{device: "a"}
	b := &fakeRuns{device: "b", logs: map[string]string{"tm-b-1": "line 1\nline 2\n"}}
	br := &fakeRuns{device: "c", repos: map[string]bool{"o/app": true}}
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home", runs: a},
		nodeSpec{device: "b", pool: "home", runs: b},
		nodeSpec{device: "c", pool: "home", runs: br, broker: &fakeBroker{}},
	)
	ctx := context.Background()
	ms[0].node.Brokers(ctx)
	l, err := ms[0].node.PoolRunLog(ctx, "tm-b-1", 1024)
	if err != nil || l.Device != "b" || string(l.Data) != "line 1\nline 2\n" || l.Size != 14 {
		t.Fatalf("log = %+v %v", l, err)
	}
	if _, err := ms[0].node.PoolRunLog(ctx, "tm-nobody", 1024); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing log err = %v", err)
	}
	if _, err := ms[0].node.PoolRunLog(ctx, "../etc/passwd", 1024); err == nil {
		t.Fatalf("a bad id was asked for")
	}

	dev, err := ms[0].node.PoolRerun(ctx, "o/app", 9)
	if err != nil || dev != "c" || len(br.reruns) != 1 {
		t.Fatalf("rerun = %q %v %v", dev, err, br.reruns)
	}
	if _, err := ms[0].node.PoolRerun(ctx, "o/other", 9); err == nil || !strings.Contains(err.Error(), "no broker") {
		t.Fatalf("rerun of an unknown repo = %v", err)
	}
	if _, err := ms[0].node.PoolRerun(ctx, "o/app", 13); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("refused rerun = %v", err)
	}
	// The broker reruns its own repos without the mesh.
	if dev, err := ms[2].node.PoolRerun(ctx, "o/app", 10); err != nil || dev != "c" {
		t.Fatalf("local rerun = %q %v", dev, err)
	}
}
