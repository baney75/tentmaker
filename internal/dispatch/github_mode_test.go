package dispatch

import (
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// TestRunnerModeLikeGitHub runs the broker's runner-mode tests again
// against a fake forge that behaves like GitHub: Runner never says which job
// a busy runner has, and only the jobs name the runner that took them. The
// default fake reports the job, which once hid that every real GitHub job
// was stopped or quarantined its device (review r1 partA first-F1, sol-F1).
func TestRunnerModeLikeGitHub(t *testing.T) {
	tests := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"HappyPath", TestRunnerModeHappyPath},
		{"TwoWorkersOneJobGetOneRunner", TestTwoWorkersOneJobGetOneRunner},
		{"IdleRunnerCountsAgainstDemand", TestIdleRunnerCountsAgainstDemand},
		{"TwoBrokersRaceWithClaimer", TestTwoBrokersRaceWithClaimer},
		{"ForkJobStolenByOurRunner", TestForkJobStolenByOurRunner},
		{"UnidentifiedBusyRunnerStoppedAfter30s", TestUnidentifiedBusyRunnerStoppedAfter30s},
		{"IdleRunnerRemovedAfterTTL", TestIdleRunnerRemovedAfterTTL},
		{"QuarantineAndRecovery", TestQuarantineAndRecovery},
		{"BudgetDownshiftDrains", TestBudgetDownshiftDrains},
		{"EvictStopsEverything", TestEvictStopsEverything},
		{"StartFailureBackoffThenClose", TestStartFailureBackoffThenClose},
		{"ExecutorMaxSlotsRespected", TestExecutorMaxSlotsRespected},
		{"DaemonRestartMidJob", TestDaemonRestartMidJob},
		{"ForgeErrorOnOneRepoDoesNotStopOthers", TestForgeErrorOnOneRepoDoesNotStopOthers},
		{"PollBacksOffWhenIdle", TestPollBacksOffWhenIdle},
		{"RefusedJobsAndRepos", TestRefusedJobsAndRepos},
		{"PauseAndResume", TestPauseAndResume},
		{"UnknownStateCountsAsRunning", TestUnknownStateCountsAsRunning},
		{"SilentWorkerLosesItsRunners", TestSilentWorkerLosesItsRunners},
		{"ShutdownLeavesBusyRunnersAndDropsIdleOnes", TestShutdownLeavesBusyRunnersAndDropsIdleOnes},
		{"FailingSourceDoesNotStopTheNext", TestFailingSourceDoesNotStopTheNext},
		{"ConcurrentWorkNeverOverServes", TestConcurrentWorkNeverOverServes},
	}
	rigLikeGitHub = true
	defer func() { rigLikeGitHub = false }()
	for _, tc := range tests {
		t.Run(tc.name, tc.fn)
	}
}

// startedRunner grants and starts one runner for one job on a GitHub-like
// forge and returns the worker, the job and the runner's name.
func startedRunner(t *testing.T) (*rig, *wrig, core.Job, string) {
	t.Helper()
	r := newRig(t, runnerRepo(repoApp))
	r.forge.LikeGitHub(true)
	w := r.addWorker("alpha", 1)
	j := r.addJob(repoApp)
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v, want one", names)
	}
	r.run(2*time.Second, 2*time.Second)
	if g := r.grantFor(names[0]); g.Started.IsZero() {
		t.Fatalf("broker never saw the runner running: %+v", g)
	}
	return r, w, j, names[0]
}

// A job shorter than 30 s, seen busy once, settles with no stop and no
// quarantine.
func TestShortJobLikeGitHubSettles(t *testing.T) {
	r, w, j, name := startedRunner(t)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.run(6*time.Second, 2*time.Second)
	if g := r.grantFor(name); g.TookJob != j.ID {
		t.Fatalf("runner's job not identified: %+v", g)
	}
	r.forge.Finish(name)
	w.ex.Exit(name)
	r.run(10*time.Second, 2*time.Second)
	if called(w.ex, "Stop", name) {
		t.Fatalf("a short job's runner was stopped")
	}
	if d := r.device("alpha"); d.Quarantined {
		t.Fatalf("device quarantined after a short job: %+v", d)
	}
	if n := len(r.broker.Status().Runners); n != 0 {
		t.Fatalf("broker still tracks %d runners", n)
	}
}

// A job that starts and ends between two monitor passes is never seen busy.
// The runner is gone; the job it was granted for names it, so it settles.
func TestUnseenJobLikeGitHubSettles(t *testing.T) {
	r, w, j, name := startedRunner(t)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.forge.Finish(name)
	w.ex.Exit(name)
	r.run(40*time.Second, 2*time.Second)
	if d := r.device("alpha"); d.Quarantined {
		t.Fatalf("device quarantined after an unseen job its grant names: %+v", d)
	}
	if n := len(r.broker.Status().Runners); n != 0 {
		t.Fatalf("broker still tracks %d runners", n)
	}
}

// A runner that ran some other job, never seen, still fails closed.
func TestUnseenOtherJobLikeGitHubQuarantines(t *testing.T) {
	r, w, _, name := startedRunner(t)
	other := r.addJob(repoApp, func(j *core.Job) { j.Name = "other" })
	if err := r.forge.Take(name, other.ID); err != nil {
		t.Fatal(err)
	}
	r.forge.Finish(name)
	w.ex.Exit(name)
	r.run(40*time.Second, 2*time.Second)
	if d := r.device("alpha"); !d.Quarantined {
		t.Fatalf("device not quarantined after an unidentified job: %+v", d)
	}
}
