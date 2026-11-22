package dispatch

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func hasProblem(ps []core.Problem, sub string) bool {
	for _, p := range ps {
		if strings.Contains(p.What, sub) {
			return true
		}
	}
	return false
}

// A job whose Stop fails may still run: it stays counted against the slots
// and the memory budget, its executor takes nothing new, and the stop is
// retried until the executor confirms it (review r1 partA sol-F2).
func TestFailedStopKeepsJobCounted(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	j := r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	r.run(2*time.Second, 2*time.Second)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.run(6*time.Second, 2*time.Second)

	w.ex.SetStopErr(errors.New("the sandbox did not answer"))
	w.budget.set(core.Decision{Profile: "off", Evict: true, Reasons: []string{"free disk below 5GiB"}})
	r.run(2*time.Second, 2*time.Second)
	if !called(w.ex, "Stop", name) {
		t.Fatalf("evict did not try to stop the job")
	}
	st := w.w.Status()
	if len(st.Running) != 1 || st.Slots.Used != 1 {
		t.Fatalf("a job whose stop failed was dropped: running %+v, slots %+v", st.Running, st.Slots)
	}
	if called(w.ex, "Clean", name) {
		t.Fatalf("cleaned a job whose stop failed")
	}
	if !hasProblem(w.w.Problems(), name) {
		t.Fatalf("problems %+v do not name the runner", w.w.Problems())
	}

	// Admission is back, but not on this executor while the stop is open.
	w.budget.set(admit(2))
	r.addJob(repoApp)
	r.run(10*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("started %d jobs on an executor with a failed stop", n-1)
	}
	if st := w.w.Status(); st.Executors[0].Ready {
		t.Fatalf("executor reported ready with a failed stop: %+v", st.Executors[0])
	}

	w.ex.SetStopErr(nil)
	r.run(10*time.Second, 2*time.Second)
	if !called(w.ex, "Clean", name) {
		t.Fatalf("not cleaned once the stop worked")
	}
	if len(w.w.Problems()) != 0 {
		t.Fatalf("problems after the stop worked: %+v", w.w.Problems())
	}
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("starts = %d; the executor should take work again", n)
	}
	if v := w.ex.Violations(); len(v) > 0 {
		t.Fatalf("violations: %v", v)
	}
}

// A job whose Clean fails leaves a runner behind; its executor takes
// nothing new until a Clean or a reset succeeds.
func TestFailedCleanBlocksExecutor(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	j := r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	r.run(2*time.Second, 2*time.Second)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.run(6*time.Second, 2*time.Second)

	w.ex.SetCleanErr(errors.New("disk busy"))
	w.ex.SetResetErr(errors.New("disk busy"))
	r.forge.Finish(name)
	w.ex.Exit(name)
	r.addJob(repoApp)
	r.run(20*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("started %d jobs on an executor with a runner it could not clean", n-1)
	}
	if !hasProblem(w.w.Problems(), name) {
		t.Fatalf("problems %+v do not name the runner", w.w.Problems())
	}

	w.ex.SetCleanErr(nil)
	r.run(10*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("starts = %d; the executor should take work once clean works", n)
	}
	if len(w.w.Problems()) != 0 {
		t.Fatalf("problems after clean worked: %+v", w.w.Problems())
	}
}
