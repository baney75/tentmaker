package dispatch

import (
	"testing"
	"time"
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
