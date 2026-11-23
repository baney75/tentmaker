package dispatch

import (
	"errors"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

// A job's CPU ceiling is the pool's CPU limit when it starts, so a job alone
// on the device gets every CPU the pool allows; job.cpus stays its
// reservation, which counts slots and the pool's use. When the limit drops
// (on battery, a profile change), jobs that start after get the lower
// ceiling.
func TestJobCPUCeilingIsThePoolLimit(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	jobCPUs := r.cfg.Job.CPUs
	w := r.addWorker("alpha", 2)
	d := admit(2)
	d.Limit.CPUs = 2 * jobCPUs
	w.budget.set(d)

	r.addJob(repoApp)
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v, want one", names)
	}
	spec, _ := w.ex.Spec(names[0])
	if spec.CPUCeiling != d.Limit.CPUs {
		t.Fatalf("CPU ceiling = %g, want the pool limit %g", spec.CPUCeiling, d.Limit.CPUs)
	}
	if spec.Limits.CPUs != jobCPUs {
		t.Fatalf("reservation = %g CPUs, want job.cpus %g", spec.Limits.CPUs, jobCPUs)
	}

	// Slots still count job.cpus: the pool's use is one job's reservation,
	// not its ceiling, and the second slot stays free.
	r.tick()
	if lim, use, _ := w.ex.PoolLimit(); use.CPUs != jobCPUs || lim.CPUs != d.Limit.CPUs {
		t.Fatalf("pool limit %g with %g in use, want %g with %g", lim.CPUs, use.CPUs, d.Limit.CPUs, jobCPUs)
	}
	if st := w.w.Status(); st.Slots.Used != 1 || st.Slots.Free != 1 {
		t.Fatalf("slots %+v, want 1 used and 1 free", st.Slots)
	}

	// The limit drops to one job's worth: the next job's ceiling drops too,
	// and never below its reservation.
	low := admit(2)
	low.Limit.CPUs = jobCPUs
	w.budget.set(low)
	w.ex.Exit(names[0])
	r.run(4e9, 2e9)
	r.addJob(repoApp, func(j *core.Job) { j.Name = "second" })
	r.run(4e9, 2e9)
	all := started(w.ex)
	if len(all) < 2 {
		t.Fatalf("starts = %v, want one after the drop", all)
	}
	for _, name := range all[1:] {
		spec, _ = w.ex.Spec(name)
		if spec.CPUCeiling != jobCPUs {
			t.Fatalf("%s: CPU ceiling after the drop = %g, want %g", name, spec.CPUCeiling, jobCPUs)
		}
	}
}

func TestCPUCeilingNeverBelowTheReservation(t *testing.T) {
	job := core.Resources{CPUs: 2}
	for _, tt := range []struct{ ceiling, want float64 }{{0, 2}, {1, 2}, {2, 2}, {4, 4}, {7.5, 7.5}} {
		if got := core.CPUCeiling(job, tt.ceiling); got != tt.want {
			t.Errorf("CPUCeiling(2, %g) = %g, want %g", tt.ceiling, got, tt.want)
		}
	}
}

// The ceiling fails closed: when the pool's own cap didn't apply (systemd
// refused it, the slice is gone), nothing holds the jobs together, so each
// job keeps job.cpus and slots x job.cpus stays within the budget. Once the
// cap applies again, new jobs get the pool limit.
func TestJobCPUCeilingFallsBackWhenThePoolCapFails(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	jobCPUs := r.cfg.Job.CPUs
	w := r.addWorker("alpha", 2)
	d := admit(2)
	d.Limit.CPUs = 2 * jobCPUs
	w.budget.set(d)
	w.ex.SetPoolLimitErr(errors.New("systemctl set-property: unit not found"))

	r.addJob(repoApp)
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v, want one", names)
	}
	if spec, _ := w.ex.Spec(names[0]); spec.CPUCeiling != 0 {
		t.Fatalf("CPU ceiling with no pool cap = %g, want 0 (job.cpus)", spec.CPUCeiling)
	}

	w.ex.SetPoolLimitErr(nil)
	w.ex.Exit(names[0])
	r.run(4e9, 2e9)
	r.addJob(repoApp, func(j *core.Job) { j.Name = "second" })
	r.run(4e9, 2e9)
	all := started(w.ex)
	if len(all) < 2 {
		t.Fatalf("starts = %v, want one after the cap applied", all)
	}
	if spec, _ := w.ex.Spec(all[len(all)-1]); spec.CPUCeiling != d.Limit.CPUs {
		t.Fatalf("CPU ceiling once the cap applied = %g, want %g", spec.CPUCeiling, d.Limit.CPUs)
	}
}
