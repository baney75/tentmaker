package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// tickWithin runs one rig tick and fails if it does not return within d of
// real time (a Start that blocks the tick blocks every heartbeat).
func tickWithin(t *testing.T, r *rig, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.tick()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("a tick did not return within %v: it waits on the executor's Start", d)
	}
}

// waitStarted waits in real time until the worker has no job still starting.
func waitStarted(t *testing.T, w *wrig) {
	t.Helper()
	for i := 0; i < 500; i++ {
		busy := false
		for _, j := range w.w.Status().Running {
			if j.Starting {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("a start never finished")
}

// A slow sandbox boot (a macOS VM, a cold image pull) must not silence the
// worker: the broker sweeps a worker silent for 2 minutes. A starting job
// still holds its slot (review r1 partA sol-F4).
func TestSlowStartKeepsHeartbeat(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	w.async = true
	release := w.ex.HoldStarts()
	defer release()
	r.addJob(repoApp)

	tickWithin(t, r, 2*time.Second)
	for i := 0; i < 90; i++ { // 3 minutes of 2 s ticks while Start hangs
		r.clk.Add(2 * time.Second)
		tickWithin(t, r, 2*time.Second)
	}
	st := w.w.Status()
	if st.Slots.Used != 1 || st.Slots.Free != 1 || len(st.Running) != 1 || !st.Running[0].Starting {
		t.Fatalf("a starting job must hold its slot: slots %+v, running %+v", st.Slots, st.Running)
	}
	if d := r.device("alpha"); !d.Online {
		t.Fatalf("the broker swept a worker whose Start was slow: %+v", d)
	}
	if n := len(r.broker.Status().Runners); n != 1 {
		t.Fatalf("broker tracks %d runners, want the one still starting", n)
	}
	name := started(w.ex)
	if len(name) != 0 {
		t.Fatalf("executor reports %v started before Start returned", name)
	}

	release()
	waitStarted(t, w)
	r.run(4*time.Second, 2*time.Second)
	runs := w.ex.Running()
	if len(runs) != 1 {
		t.Fatalf("running = %v after the start finished", runs)
	}
	if g := r.grantFor(runs[0]); g.Started.IsZero() {
		t.Fatalf("broker never saw the runner running: %+v", g)
	}
}

// holdingSource takes the worker's Hold after the broker answers with an
// offer and before the worker starts it: the window between admission and
// the job record.
type holdingSource struct {
	*source
	w       *Worker
	jobs    int
	release func()
}

func (s *holdingSource) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	resp, err := s.source.Work(ctx, req)
	if err == nil && len(resp.Offers) > 0 && s.release == nil {
		s.jobs, s.release = s.w.Hold()
	}
	return resp, err
}

// A Hold that counted zero jobs means no job starts until release, even an
// offer that passed admission before the hold (review r2 partA sol-F1).
func TestHoldDuringOfferBlocksStart(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	src := &holdingSource{source: &source{inner: r.broker}, w: w.w}
	w.sources = []core.WorkSource{src}
	r.addJob(repoApp)
	r.tick()
	if src.release == nil {
		t.Fatalf("the broker made no offer")
	}
	defer src.release()
	if src.jobs != 0 {
		t.Fatalf("Hold counted %d jobs, want 0", src.jobs)
	}
	if n := len(w.w.Status().Running); n != 0 {
		t.Fatalf("a job started under a hold that counted zero jobs: %d running", n)
	}
	if n := len(started(w.ex)); n != 0 {
		t.Fatalf("executor started %d runners under the hold", n)
	}
	r.run(4*time.Second, 2*time.Second)
	if n := len(r.broker.Status().Runners); n != 0 {
		t.Fatalf("broker still tracks %d runners after the decline", n)
	}
	src.release()
	r.run(4*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("starts = %d after release, want the job offered again", n)
	}
}
