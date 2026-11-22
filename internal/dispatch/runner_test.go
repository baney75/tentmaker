package dispatch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

// The whole runner-mode path in one process: queued job, grant, start,
// the runner takes the job, the broker identifies it, the job ends, the
// worker reports, the broker forgets, the node resets.
func TestRunnerModeHappyPath(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	j := r.addJob(repoApp)

	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v, want one", names)
	}
	name := names[0]
	if !strings.HasPrefix(name, "tm-alpha-") || len(name) != len("tm-alpha-")+8 || !core.NamePattern.MatchString(name) {
		t.Fatalf("runner name %q", name)
	}
	spec, _ := w.ex.Spec(name)
	grants := r.forge.Grants()
	if len(grants) != 1 || !bytes.Equal(spec.Grant.Secret, grants[0].Secret) || len(spec.Grant.Secret) == 0 {
		t.Fatalf("executor did not get the forge's grant secret")
	}
	if spec.Kind != core.KindRunner || spec.Platform != linuxX64 || spec.TestWorkers != 2 ||
		spec.Limits != (core.Resources{MemoryBytes: 2 << 30, CPUs: 2}) || spec.MaxDuration != 3*time.Hour {
		t.Fatalf("spec = %+v", spec)
	}
	for _, l := range []string{"self-hosted", "tentmaker", "tm-alpha", "linux", "X64"} {
		if !hasLabel(grants[0].Labels, l) {
			t.Errorf("grant labels %v miss %s", grants[0].Labels, l)
		}
	}

	r.run(2*time.Second, 2*time.Second) // the worker reports it running
	if g := r.grantFor(name); g.Started.IsZero() {
		t.Fatalf("broker never saw the runner running: %+v", g)
	}
	if got, ok := r.forge.TakeAny(name); !ok || got.ID != j.ID {
		t.Fatalf("runner took %v %v", got, ok)
	}
	r.run(8*time.Second, 2*time.Second)
	if g := r.grantFor(name); !g.Busy || g.TookJob != j.ID {
		t.Fatalf("runner not identified: %+v", g)
	}

	r.forge.Finish(name)
	w.ex.Exit(name)
	r.run(2*time.Second, 2*time.Second)
	if n := len(r.broker.Status().Runners); n != 0 {
		t.Fatalf("broker still tracks %d runners", n)
	}
	if n := len(w.w.Status().Running); n != 0 {
		t.Fatalf("worker still runs %d jobs", n)
	}
	r.run(90*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("starts = %d after the job finished", n)
	}
	if d := r.device("alpha"); d.Quarantined || !d.Online {
		t.Fatalf("device = %+v", d)
	}
	if w.ex.Resets() < 2 {
		t.Fatalf("resets = %d; the node should reset after the job", w.ex.Resets())
	}
	if v := w.ex.Violations(); len(v) > 0 {
		t.Fatalf("executor contract violations: %v", v)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.bdir, "broker.json")); err != nil {
		t.Fatal(err)
	}
	r.assertNoSecrets()
}

func TestTwoWorkersOneJobGetOneRunner(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	a := r.addWorker("alpha", 2)
	b := r.addWorker("beta", 2)
	r.addJob(repoApp)
	r.tick()
	r.run(60*time.Second, 2*time.Second)
	if n := len(started(a.ex)) + len(started(b.ex)); n != 1 {
		t.Fatalf("starts = %d, want 1", n)
	}
	if n := len(r.forge.AllRunners()); n != 1 {
		t.Fatalf("forge runners = %d", n)
	}
}

func TestIdleRunnerCountsAgainstDemand(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 5)
	r.addJob(repoApp)
	r.addJob(repoApp)
	r.tick()
	r.run(30*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("starts = %d, want 2 (one per job)", n)
	}
}

func TestTwoBrokersRaceWithClaimer(t *testing.T) {
	for _, withClaimer := range []bool{true, false} {
		r := newRig(t, runnerRepo(repoApp))
		var c *claimer
		var cl core.Claimer
		if withClaimer {
			c = &claimer{}
			cl = c
		}
		ba := r.newBroker(testConfig("alpha", runnerRepo(repoApp)), t.TempDir(), cl)
		bb := r.newBroker(testConfig("beta", runnerRepo(repoApp)), t.TempDir(), cl)
		r.broker = ba
		wa := r.addWorker("alpha", 2)
		wb := r.addWorker("beta", 2)
		wa.sources = []core.WorkSource{ba}
		wb.sources = []core.WorkSource{bb}
		r.addJob(repoApp)
		for i := 0; i < 5; i++ {
			for _, b := range []*Broker{ba, bb} {
				if err := b.Tick(bg); err != nil {
					t.Fatal(err)
				}
			}
			for _, w := range r.workers {
				if err := w.w.Tick(bg); err != nil {
					t.Fatal(err)
				}
				w.w.starts.Wait()
			}
			r.clk.Add(2 * time.Second)
		}
		grants := len(r.forge.Grants())
		if withClaimer {
			if grants != 1 || c.denied == 0 {
				t.Fatalf("with a claimer: grants = %d, denied = %d", grants, c.denied)
			}
		} else if grants != 2 {
			t.Fatalf("without a claimer the race should give 2 grants, got %d", grants)
		}
	}
}

// A JIT runner is not bound to its job: GitHub can hand it a fork's job.
func TestForkJobStolenByOurRunner(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	fork := r.addJob(repoApp, func(j *core.Job) { j.FromFork = true; j.CreatedAt = j.CreatedAt.Add(-time.Minute); j.RunID = 777 })
	r.addJob(repoApp)
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v; the fork job must not get a runner", names)
	}
	if !strings.Contains(r.logs.String(), "job from a fork") {
		t.Fatalf("fork refusal not logged")
	}
	name := names[0]
	r.run(2*time.Second, 2*time.Second)
	if err := r.forge.Take(name, fork.ID); err != nil {
		t.Fatal(err)
	}
	r.run(10*time.Second, 2*time.Second)

	if got := r.forge.Cancelled(repoApp); len(got) != 1 || got[0] != 777 {
		t.Fatalf("cancelled runs = %v, want [777]", got)
	}
	if !called(w.ex, "Stop", name) || !called(w.ex, "Clean", name) {
		t.Fatalf("fork runner not stopped and cleaned")
	}
	if r.forge.CallsFor(fakeforge.MDeleteRunner, repoApp) == 0 {
		t.Fatalf("no attempt to delete the fork runner")
	}
	if !r.device("alpha").Quarantined {
		t.Fatalf("device not quarantined after a fork job")
	}
	st := w.w.Status()
	if len(st.Sources) != 1 || !st.Sources[0].Quarantined {
		t.Fatalf("worker sources = %+v", st.Sources)
	}

	// Quarantined: nothing new from this broker until the node resets.
	before := len(started(w.ex))
	r.run(30*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != before {
		t.Fatalf("started %d jobs while quarantined", n-before)
	}
	r.run(40*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n <= before {
		t.Fatalf("no work after the reset")
	}
	if r.device("alpha").Quarantined {
		t.Fatalf("quarantine not lifted after the reset")
	}
	if v := w.ex.Violations(); len(v) > 0 {
		t.Fatalf("violations: %v", v)
	}
}

func TestUnidentifiedBusyRunnerStoppedAfter30s(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 1)
	j := r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	r.run(2*time.Second, 2*time.Second)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.forge.HideJob(name, true)
	var busyAt time.Time
	for i := 0; i < 10 && busyAt.IsZero(); i++ {
		r.run(time.Second, time.Second)
		if r.grantFor(name).Busy {
			busyAt = r.clk.Now()
		}
	}
	if busyAt.IsZero() {
		t.Fatal("broker never saw the runner busy")
	}
	for r.clk.Now().Sub(busyAt) < 25*time.Second {
		r.run(time.Second, time.Second)
	}
	if called(w.ex, "Stop", name) {
		t.Fatalf("stopped before 30 s")
	}
	r.run(15*time.Second, time.Second)
	if !called(w.ex, "Stop", name) {
		t.Fatalf("unidentified runner still running after 40 s busy")
	}
	if !r.device("alpha").Quarantined {
		t.Fatalf("device not quarantined")
	}
}

func TestIdleRunnerRemovedAfterTTL(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 1)
	j := r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	// Another pool's runner wins the race for the job.
	r.forge.SetJobStatus(repoApp, j.ID, "in_progress")
	r.run(80*time.Second, 2*time.Second)
	if called(w.ex, "Stop", name) {
		t.Fatalf("idle runner stopped before idle_ttl")
	}
	r.run(20*time.Second, 2*time.Second)
	if !called(w.ex, "Stop", name) {
		t.Fatalf("idle runner not stopped after idle_ttl")
	}
	if _, ok := r.forge.RunnerByName(name); ok {
		t.Fatalf("idle runner not deleted at the forge")
	}
	r.run(4*time.Second, 2*time.Second)
	if n := len(r.broker.Status().Runners); n != 0 {
		t.Fatalf("broker still tracks %d runners", n)
	}
	if r.device("alpha").Quarantined {
		t.Fatalf("an idle runner removed on purpose must not quarantine")
	}
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("starts = %d; the job was taken elsewhere", n)
	}
}

// A runner that ran a job between two checks and ended: the broker can't say
// which job, so the worker takes nothing from it until it resets.
func TestQuarantineAndRecovery(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 1)
	r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	r.run(2*time.Second, 2*time.Second)
	r.forge.HideJob(name, true) // the forge never says which job it ran
	if _, ok := r.forge.TakeAny(name); !ok {
		t.Fatal("take")
	}
	r.forge.Finish(name)
	w.ex.Exit(name)
	r.run(6*time.Second, 2*time.Second)
	if r.device("alpha").Quarantined {
		t.Fatalf("quarantined before the 30 s an ended runner's job has to show up")
	}
	r.run(30*time.Second, 2*time.Second)
	if !r.device("alpha").Quarantined {
		t.Fatalf("device not quarantined")
	}
	if !w.w.Status().Sources[0].Quarantined {
		t.Fatalf("worker did not take the quarantine")
	}
	r.addJob(repoApp)
	// The worker resets once a minute; until then it takes nothing.
	for i := 0; i < 45 && w.w.Status().Sources[0].Quarantined; i++ {
		if n := len(started(w.ex)); n != 1 {
			t.Fatalf("started %d jobs while quarantined", n-1)
		}
		r.run(2*time.Second, 2*time.Second)
	}
	r.run(4*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("starts = %d; work should resume after the reset", n)
	}
	if r.device("alpha").Quarantined || w.w.Status().Sources[0].Quarantined {
		t.Fatalf("quarantine not lifted")
	}
}

func TestBudgetDownshiftDrains(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	src := &source{inner: r.broker}
	w.sources = []core.WorkSource{src}
	r.addJob(repoApp)
	r.addJob(repoApp)
	r.tick()
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("starts = %d", n)
	}
	w.budget.set(core.Decision{Profile: "off", Reasons: []string{"on battery at 31%, below 40%"}})
	r.addJob(repoApp)
	r.run(20*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("started work while draining")
	}
	if n := len(w.ex.Calls("Stop")); n != 0 {
		t.Fatalf("drain stopped %d running jobs", n)
	}
	if req := src.last(); req.Free != 0 || len(req.Running) != 2 {
		t.Fatalf("last request free=%d running=%v", req.Free, req.Running)
	}
	if w.budget.running() != 2 {
		t.Fatalf("budget saw %d running", w.budget.running())
	}
	_, inUse, calls := w.ex.PoolLimit()
	if calls == 0 || inUse != (core.Resources{MemoryBytes: 4 << 30, CPUs: 4}) {
		t.Fatalf("pool limit in use = %+v after %d calls", inUse, calls)
	}
	w.budget.set(admit(3))
	r.run(2*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 3 {
		t.Fatalf("starts = %d after the budget came back", n)
	}
}

func TestEvictStopsEverything(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	j := r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	r.run(2*time.Second, 2*time.Second)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.run(8*time.Second, 2*time.Second)
	w.budget.set(core.Decision{Profile: "off", Evict: true, Reasons: []string{"free disk below 5GiB"}})
	r.run(2*time.Second, 2*time.Second)
	if !called(w.ex, "Stop", name) || !called(w.ex, "Clean", name) {
		t.Fatalf("evict did not stop and clean")
	}
	if n := len(w.w.Status().Running); n != 0 {
		t.Fatalf("worker still runs %d", n)
	}
	r.run(2*time.Second, 2*time.Second)
	if n := len(r.broker.Status().Runners); n != 0 {
		t.Fatalf("broker still tracks %d runners", n)
	}
}

func TestStartFailureBackoffThenClose(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 1)
	w.ex.SetStartErr(errBoom)
	r.addJob(repoApp)
	var at []time.Duration
	seen := 0
	r.tick()
	for r.clk.Now().Sub(t0) < 3*time.Hour {
		if n := len(started(w.ex)); n != seen {
			seen = n
			at = append(at, r.clk.Now().Sub(t0))
		}
		r.run(5*time.Second, 5*time.Second)
	}
	if len(at) != bootCloseAfter {
		t.Fatalf("start attempts at %v; want %d then closed", at, bootCloseAfter)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i, wnt := range want {
		gap := at[i+1] - at[i]
		if gap < wnt || gap > wnt+20*time.Second {
			t.Errorf("gap %d = %v, want about %v", i, gap, wnt)
		}
	}
	st := w.w.Status()
	if !st.Executors[0].Closed || st.Executors[0].BootFailures != bootCloseAfter {
		t.Fatalf("executor status = %+v", st.Executors[0])
	}
	if n := len(r.forge.AllRunners()); n != 0 {
		t.Fatalf("%d runners leaked at the forge", n)
	}
	if r.device("alpha").Quarantined {
		t.Fatalf("a start failure (runner never took a job) must not quarantine")
	}
}

func TestExecutorMaxSlotsRespected(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorkerWith("alpha", 4, 1)
	for i := 0; i < 3; i++ {
		r.addJob(repoApp)
	}
	r.tick()
	r.run(10*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("starts = %d with MaxSlots 1", n)
	}
	if v := w.ex.Violations(); len(v) > 0 {
		t.Fatal(v)
	}
}

func TestDaemonRestartMidJob(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 3)
	j1 := r.addJob(repoApp)
	r.addJob(repoApp)
	r.tick()
	names := started(w.ex)
	if len(names) != 2 {
		t.Fatalf("starts = %v", names)
	}
	r.run(2*time.Second, 2*time.Second)
	busy, lost := names[0], names[1]
	if err := r.forge.Take(busy, j1.ID); err != nil {
		t.Fatal(err)
	}
	r.run(8*time.Second, 2*time.Second)
	if r.grantFor(busy).TookJob != j1.ID {
		t.Fatalf("busy runner not identified")
	}

	// The daemon goes down. Meanwhile the second runner vanishes and an
	// unknown runner appears in the executor.
	w.ex.SetState(lost, core.ExecMissing)
	w.ex.AddLeftover("tm-alpha-0badf00d")
	r.broker = r.newBroker(r.cfg, r.bdir, nil)
	w.sources = []core.WorkSource{r.broker}
	w.w = r.newWorker(w)
	if st := w.w.Status(); len(st.Running) != 2 {
		t.Fatalf("state.json not reloaded: %+v", st.Running)
	}
	r.tick()
	if !called(w.ex, "Stop", "tm-alpha-0badf00d") || !called(w.ex, "Clean", "tm-alpha-0badf00d") {
		t.Fatalf("leftover not stopped and cleaned")
	}
	if called(w.ex, "Stop", busy) {
		t.Fatalf("restart stopped the busy runner")
	}
	st := w.w.Status()
	if len(st.Running) != 1 || st.Running[0].Name != busy {
		t.Fatalf("running after restart = %+v", st.Running)
	}
	if g := r.grantFor(busy); g.TookJob != j1.ID {
		t.Fatalf("restarted broker lost the busy runner: %+v", g)
	}
	r.run(4*time.Second, 2*time.Second)
	if called(w.ex, "Stop", busy) {
		t.Fatalf("busy runner stopped after the restart")
	}
	r.forge.Finish(busy)
	w.ex.Exit(busy)
	r.run(10*time.Second, 2*time.Second)
	for _, g := range r.broker.Status().Runners {
		if g.Name == busy || g.Name == lost {
			t.Fatalf("broker still tracks %s", g.Name)
		}
	}
	if r.device("alpha").Quarantined {
		t.Fatalf("restart quarantined the device")
	}
}

func TestForgeErrorOnOneRepoDoesNotStopOthers(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp), runnerRepo(repoTools))
	w := r.addWorker("alpha", 4)
	r.forge.SetErr(fakeforge.MQueuedJobs, repoApp, errBoom)
	r.addJob(repoApp)
	r.addJob(repoTools)
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v", names)
	}
	if spec, _ := w.ex.Spec(names[0]); spec.Grant.Repo != repoTools {
		t.Fatalf("started a job for %s", spec.Grant.Repo)
	}
	for _, rs := range r.broker.Status().Repos {
		if rs.Name == repoApp && (rs.Healthy || rs.LastError == "") {
			t.Fatalf("app status = %+v", rs)
		}
		if rs.Name == repoTools && !rs.Healthy {
			t.Fatalf("tools status = %+v", rs)
		}
	}
	r.run(10*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("starts = %d while app errors", n)
	}
	r.forge.SetErr(fakeforge.MQueuedJobs, repoApp, nil)
	r.run(32*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("app's job not served after its forge recovered")
	}
}

func TestPollBacksOffWhenIdle(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	var at []time.Duration
	seen := 0
	r.tick()
	for r.clk.Now().Sub(t0) <= 160*time.Second {
		if n := r.forge.CallsFor(fakeforge.MQueuedJobs, repoApp); n != seen {
			seen = n
			at = append(at, r.clk.Now().Sub(t0))
		}
		r.run(time.Second, time.Second)
	}
	want := []float64{5, 7.5, 11.25, 16.875, 25.3125, 30, 30}
	if len(at) < len(want)+1 {
		t.Fatalf("polls at %v", at)
	}
	for i, wnt := range want {
		gap := (at[i+1] - at[i]).Seconds()
		if gap < wnt || gap > wnt+1 {
			t.Errorf("gap %d = %vs, want %vs", i, gap, wnt)
		}
	}
	// Work resets the interval to 5 s.
	r.addJob(repoApp)
	for n := r.forge.CallsFor(fakeforge.MQueuedJobs, repoApp); r.forge.CallsFor(fakeforge.MQueuedJobs, repoApp) == n; {
		r.run(time.Second, time.Second)
	}
	first := r.clk.Now()
	for n := r.forge.CallsFor(fakeforge.MQueuedJobs, repoApp); r.forge.CallsFor(fakeforge.MQueuedJobs, repoApp) == n; {
		r.run(time.Second, time.Second)
	}
	if gap := r.clk.Now().Sub(first); gap != 5*time.Second {
		t.Fatalf("interval with work queued = %v", gap)
	}
}

func TestRefusedJobsAndRepos(t *testing.T) {
	pub := "baney75/public"
	pubOK := "baney75/public-ok"
	pubWeak := "baney75/public-weak"
	broken := "baney75/broken"
	evil := "evil/repo"
	r := newRig(t, runnerRepo(repoApp), runnerRepo(pub),
		config.Repo{Name: pubOK, AllowPublic: true}, config.Repo{Name: pubWeak, AllowPublic: true},
		runnerRepo(broken), runnerRepo(evil))
	r.cfg.DenyRepos = []string{"evil/*"}
	r.broker = r.newBroker(r.cfg, r.bdir, nil)
	w := r.addWorker("alpha", 10)
	r.forge.SetRepo(core.RepoInfo{Repo: pub, Public: true, ForkPolicy: "approve_all"})
	r.forge.SetRepo(core.RepoInfo{Repo: pubOK, Public: true, ForkPolicy: "approve_all"})
	r.forge.SetRepo(core.RepoInfo{Repo: pubWeak, Public: true, ForkPolicy: "approve_first_time"})
	r.forge.SetErr(fakeforge.MCheckRepo, broken, errBoom)

	good := r.addJob(repoApp)
	r.addJob(repoApp, func(j *core.Job) { j.FromFork = true })
	r.addJob(repoApp, func(j *core.Job) { j.CreatedAt = r.clk.Now().Add(-25 * time.Hour) })
	r.addJob(repoApp, func(j *core.Job) { j.Status = "waiting" })
	r.addJob(repoApp, func(j *core.Job) { j.Labels = []string{"self-hosted", "linux"} })
	r.addJob(repoApp, func(j *core.Job) { j.Labels = []string{"self-hosted", "tentmaker", "gpu"} })
	r.addJob(pub)
	okJob := r.addJob(pubOK)
	r.addJob(pubWeak)
	r.addJob(broken)
	r.addJob(evil)

	r.tick()
	r.run(10*time.Second, 2*time.Second)
	got := map[int64]bool{}
	for _, g := range r.broker.Status().Runners {
		got[g.JobID] = true
	}
	if len(got) != 2 || !got[good.ID] || !got[okJob.ID] {
		t.Fatalf("granted jobs %v, want only %d and %d", got, good.ID, okJob.ID)
	}
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("starts = %d", n)
	}
	if n := r.forge.CallsFor(fakeforge.MQueuedJobs, evil); n != 0 {
		t.Fatalf("denied repo polled %d times", n)
	}
	if n := r.forge.CallsFor(fakeforge.MCheckRepo, repoApp); n != 1 {
		t.Fatalf("CheckRepo called %d times; it is cached for an hour", n)
	}
	r.run(2*time.Minute, 10*time.Second)
	if n := r.forge.CallsFor(fakeforge.MCheckRepo, broken); n < 2 {
		t.Fatalf("an unreadable repo should be read again after a minute (calls %d)", n)
	}
	r.run(time.Hour, 30*time.Second)
	if n := r.forge.CallsFor(fakeforge.MCheckRepo, repoApp); n != 2 {
		t.Fatalf("CheckRepo calls after an hour = %d, want 2", n)
	}
	for _, rs := range r.broker.Status().Repos {
		switch rs.Name {
		case evil:
			if !rs.Denied {
				t.Errorf("evil/repo not shown as denied")
			}
		case pub, pubWeak, broken:
			if rs.Refused == "" {
				t.Errorf("%s not shown as refused: %+v", rs.Name, rs)
			}
		}
	}
}

func TestPauseAndResume(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	w.w.Pause("presenting")
	r.addJob(repoApp)
	r.tick()
	r.run(6*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 0 {
		t.Fatalf("started while paused")
	}
	if st := w.w.Status(); !st.Paused || st.PauseReason != "presenting" {
		t.Fatalf("status = %+v", st)
	}
	w.w.Resume()
	r.run(2*time.Second, 2*time.Second)
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("not started after resume")
	}
}

func TestUnknownStateCountsAsRunning(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 1)
	r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	w.ex.SetStateErr(name, errBoom)
	r.run(70*time.Second, 2*time.Second)
	if called(w.ex, "Clean", name) || called(w.ex, "Stop", name) {
		t.Fatalf("a runner in unknown state was cut")
	}
	if w.budget.running() != 1 || len(w.w.Status().Running) != 1 {
		t.Fatalf("unknown runner not counted as running")
	}
	if len(w.ex.Calls("Reset")) != 1 {
		t.Fatalf("reset ran while a runner's state was unknown")
	}
	if g := r.grantFor(name); g.Name == "" || g.Ended {
		t.Fatalf("broker was told it ended: %+v", g)
	}
}

func TestSilentWorkerLosesItsRunners(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 1)
	j := r.addJob(repoApp)
	r.tick()
	name := started(w.ex)[0]
	r.run(2*time.Second, 2*time.Second)
	r.forge.SetJobStatus(repoApp, j.ID, "in_progress") // keep the broker from granting it again
	w.silent = true
	r.run(2*time.Minute+10*time.Second, 5*time.Second)
	if _, ok := r.forge.RunnerByName(name); ok {
		t.Fatalf("silent worker's runner not deleted")
	}
	if d := r.device("alpha"); d.Online {
		t.Fatalf("device still online: %+v", d)
	}
	if n := len(r.broker.Status().Runners); n != 0 {
		t.Fatalf("broker still tracks %d", n)
	}
	// It comes back still running the runner the broker dropped: stop it.
	w.silent = false
	r.run(4*time.Second, 2*time.Second)
	if !called(w.ex, "Stop", name) {
		t.Fatalf("returning worker's orphan runner not stopped")
	}
	if !r.device("alpha").Online {
		t.Fatalf("device not back online")
	}
}

func TestShutdownLeavesBusyRunnersAndDropsIdleOnes(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	j1 := r.addJob(repoApp)
	r.addJob(repoApp)
	r.tick()
	names := started(w.ex)
	busy, idle := names[0], names[1]
	r.run(2*time.Second, 2*time.Second)
	if err := r.forge.Take(busy, j1.ID); err != nil {
		t.Fatal(err)
	}
	r.run(8*time.Second, 2*time.Second)

	r.broker.Shutdown(bg)
	if _, ok := r.forge.RunnerByName(idle); ok {
		t.Fatalf("idle runner not deleted at shutdown")
	}
	if _, ok := r.forge.RunnerByName(busy); !ok {
		t.Fatalf("busy runner deleted at shutdown")
	}
	w.w.Shutdown(bg)
	if !called(w.ex, "Stop", idle) {
		t.Fatalf("idle runner not stopped")
	}
	if called(w.ex, "Stop", busy) {
		t.Fatalf("busy runner stopped at shutdown")
	}
	if got := w.ex.Running(); len(got) != 1 || got[0] != busy {
		t.Fatalf("still running = %v", got)
	}
	b, err := os.ReadFile(filepath.Join(w.dir, "state.json"))
	if err != nil || !strings.Contains(string(b), busy) {
		t.Fatalf("state.json lost the busy job: %v", err)
	}
	// A shut-down broker grants nothing.
	r.addJob(repoApp)
	resp, err := r.broker.Work(bg, core.WorkRequest{Device: "beta", Free: 2, Labels: WorkerLabels("beta", nil),
		Platforms: []core.Platform{linuxX64}})
	if err != nil || len(resp.Offers) != 0 {
		t.Fatalf("shut-down broker offered %d (%v)", len(resp.Offers), err)
	}
}

func TestFailingSourceDoesNotStopTheNext(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	w := r.addWorker("alpha", 2)
	other := NewBroker(BrokerOptions{Config: testConfig("down"), Now: r.clk.Now})
	bad := &source{inner: other}
	bad.setFail(errBoom)
	w.sources = []core.WorkSource{bad, r.broker}
	r.addJob(repoApp)
	r.tick()
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("starts = %d", n)
	}
	var down SourceStatus
	for _, s := range w.w.Status().Sources {
		if s.Name == "down" {
			down = s
		}
	}
	if down.Healthy || down.Failures != 1 || down.LastError == "" {
		t.Fatalf("down source status = %+v", down)
	}
}

// Many devices asking at once for one job, while the broker ticks and
// status is read: exactly one grant (the reservation is taken under the
// lock before any forge call).
func TestConcurrentWorkNeverOverServes(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	r.addJob(repoApp)
	if err := r.broker.Tick(bg); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	offers := make(chan int, 32)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.broker.Tick(bg)
				_ = r.broker.Status()
			}
		}
	}()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dev := fmt.Sprintf("dev%02d", i)
			resp, err := r.broker.Work(bg, core.WorkRequest{Device: dev, Free: 1, Labels: WorkerLabels(dev, nil),
				Platforms: []core.Platform{linuxX64}})
			if err != nil {
				t.Error(err)
			}
			offers <- len(resp.Offers)
		}(i)
	}
	wg.Wait()
	close(stop)
	close(offers)
	total := 0
	for n := range offers {
		total += n
	}
	if total != 1 || len(r.forge.Grants()) != 1 {
		t.Fatalf("offers = %d, grants = %d; want exactly 1", total, len(r.forge.Grants()))
	}
}
