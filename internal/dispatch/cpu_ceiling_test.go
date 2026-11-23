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
	// TENTMAKER_JOBS follows the ceiling, not job.cpus: a job alone sizes
	// its parallelism to every CPU its unit allows.
	if want := int(d.Limit.CPUs); spec.TestWorkers != want {
		t.Fatalf("test workers = %d, want the ceiling %d (job.cpus is %g)", spec.TestWorkers, want, jobCPUs)
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
		if want := int(jobCPUs); spec.TestWorkers != want {
			t.Fatalf("%s: test workers after the drop = %d, want %d", name, spec.TestWorkers, want)
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
	if spec, _ := w.ex.Spec(names[0]); spec.CPUCeiling != 0 || spec.TestWorkers != int(jobCPUs) {
		t.Fatalf("with no pool cap: CPU ceiling %g, test workers %d; want 0 and job.cpus %g",
			spec.CPUCeiling, spec.TestWorkers, jobCPUs)
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
	if spec, _ := w.ex.Spec(all[len(all)-1]); spec.CPUCeiling != d.Limit.CPUs || spec.TestWorkers != int(d.Limit.CPUs) {
		t.Fatalf("once the cap applied: CPU ceiling %g, test workers %d; want %g and %d",
			spec.CPUCeiling, spec.TestWorkers, d.Limit.CPUs, int(d.Limit.CPUs))
	}
}

// An executor with no pool-wide CPU cap (macos-tart, windows-job) holds each
// job at job.cpus, so the worker gives it no ceiling and tells the job
// job.cpus workers, even though its SetPoolLimit succeeds.
func TestNoCPUCeilingWithoutAPoolWideCap(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	jobCPUs := r.cfg.Job.CPUs
	w := r.addWorker("alpha", 2)
	d := admit(2)
	d.Limit.CPUs = 2 * jobCPUs
	w.budget.set(d)
	w.ex.SetCapsPoolCPU(false)

	r.addJob(repoApp)
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v, want one", names)
	}
	if spec, _ := w.ex.Spec(names[0]); spec.CPUCeiling != 0 || spec.TestWorkers != int(jobCPUs) {
		t.Fatalf("CPU ceiling %g, test workers %d; want 0 and job.cpus %g", spec.CPUCeiling, spec.TestWorkers, jobCPUs)
	}
}

// Past job.cpus, TENTMAKER_JOBS leaves each worker 512 MiB of the job's
// memory, which does not burst with the CPUs; it never drops below job.cpus.
func TestTestWorkersFitTheJobsMemory(t *testing.T) {
	const gib = 1 << 30
	for _, tt := range []struct {
		mem           int64
		cpus, ceiling float64
		want          int
	}{{2 * gib, 2, 8, 4}, {8 * gib, 2, 8, 8}, {512 << 20, 2, 8, 2}, {3 * gib, 2, 0, 2}, {gib, 0.5, 4, 2}} {
		if got := testWorkers(core.Resources{MemoryBytes: tt.mem, CPUs: tt.cpus}, tt.ceiling); got != tt.want {
			t.Errorf("testWorkers(mem %d MiB, cpus %g, ceiling %g) = %d, want %d", tt.mem>>20, tt.cpus, tt.ceiling, got, tt.want)
		}
	}
}

func TestTestWorkersAreTheWholeCPUsOfTheCeiling(t *testing.T) {
	for _, tt := range []struct {
		cpus, ceiling float64
		want          int
	}{{2, 0, 2}, {2, 4, 4}, {2, 1, 2}, {2, 7.5, 7}, {1.5, 0, 1}, {0.5, 0, 1}, {0.5, 0.75, 1}} {
		if got := testWorkers(core.Resources{CPUs: tt.cpus}, tt.ceiling); got != tt.want {
			t.Errorf("testWorkers(cpus %g, ceiling %g) = %d, want %d", tt.cpus, tt.ceiling, got, tt.want)
		}
	}
}
