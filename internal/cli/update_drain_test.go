package cli

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// updateFixture is a set-up device whose service runs version 9.9.9 after
// a restart, with jobs running.
func updateFixture(t *testing.T, running int) *fixture {
	t.Helper()
	f := newFixture(t)
	f.setUp()
	f.isRoot = true
	f.up.newer = true
	f.svc.installed, f.svc.running = true, true
	f.serveControl()
	f.daemon.mu.Lock()
	f.daemon.version, f.daemon.running = "9.9.9", running
	f.daemon.mu.Unlock()
	return f
}

func (f *fixture) restarted() bool {
	for _, c := range f.svc.calls {
		if c == "restart" {
			return true
		}
	}
	return false
}

// update pauses new jobs and waits for running ones before the restart
// that would end them, then resumes (review r1 partB first-F5).
func TestUpdateWaitsForRunningJobs(t *testing.T) {
	f := updateFixture(t, 1)
	var sleeps atomic.Int32
	f.env.Sleep = func(time.Duration) {
		if sleeps.Add(1) == 3 {
			f.daemon.mu.Lock()
			f.daemon.running = 0
			f.daemon.mu.Unlock()
		}
	}
	appliedWith := -1
	f.up.onApply = func() {
		f.daemon.mu.Lock()
		appliedWith = f.daemon.running
		f.daemon.mu.Unlock()
	}
	if code := f.run("", "update", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, f.stdout, f.stderr)
	}
	if appliedWith != 0 {
		t.Fatalf("applied with %d job(s) running", appliedWith)
	}
	if len(f.daemon.paused) != 1 || !strings.Contains(f.daemon.paused[0], "updating") || f.daemon.resumes != 1 {
		t.Fatalf("paused %v, resumed %d times", f.daemon.paused, f.daemon.resumes)
	}
}

// Jobs that don't finish in time leave everything as it was.
func TestUpdateRefusesWhileJobsRun(t *testing.T) {
	defer func(d time.Duration) { UpdateDrainWait = d }(UpdateDrainWait)
	UpdateDrainWait = 200 * time.Millisecond
	f := updateFixture(t, 2)
	code := f.run("", "update")
	if code == ExitOK || !strings.Contains(f.stderr.String(), "2 job(s) still running") || !strings.Contains(f.stderr.String(), "--force") {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if len(f.up.applied) != 0 || f.restarted() {
		t.Fatalf("applied %v, service calls %v", f.up.applied, f.svc.calls)
	}
	if f.daemon.resumes != 1 || f.daemon.isPaused {
		t.Fatalf("left new jobs paused: resumes %d", f.daemon.resumes)
	}
}

// --force updates without waiting, but still pauses new jobs first.
func TestUpdateForceSkipsTheWait(t *testing.T) {
	f := updateFixture(t, 1)
	if code := f.run("", "update", "--force", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if len(f.up.applied) != 1 || !f.restarted() || len(f.daemon.paused) != 1 || f.daemon.resumes != 1 {
		t.Fatalf("applied %v, calls %v, paused %v, resumes %d", f.up.applied, f.svc.calls, f.daemon.paused, f.daemon.resumes)
	}
}

// A device someone paused stays paused after the update.
func TestUpdateKeepsAnEarlierPause(t *testing.T) {
	f := updateFixture(t, 0)
	f.daemon.mu.Lock()
	f.daemon.isPaused = true
	f.daemon.mu.Unlock()
	if code := f.run("", "update", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if len(f.daemon.paused) != 0 || f.daemon.resumes != 0 || !f.daemon.isPaused {
		t.Fatalf("paused %v, resumes %d, paused now %v", f.daemon.paused, f.daemon.resumes, f.daemon.isPaused)
	}
}

// A service manager that can't say whether the service runs might hide
// running jobs, so update changes nothing unless --force says ending them
// is fine (review r2 partB sol-F1).
func TestUpdateRefusesWhenServiceStatusFails(t *testing.T) {
	f := updateFixture(t, 1)
	f.svc.statusErr = errors.New("systemctl timed out")
	code := f.run("", "update")
	if code == ExitOK || !strings.Contains(f.stderr.String(), "can't tell whether the service is running") || !strings.Contains(f.stderr.String(), "--force") {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if len(f.up.applied) != 0 || f.restarted() || len(f.daemon.paused) != 0 {
		t.Fatalf("applied %v, service calls %v, paused %v", f.up.applied, f.svc.calls, f.daemon.paused)
	}
	if code := f.run("", "update", "--force", "--json"); code != ExitOK {
		t.Fatalf("--force: exit %d: %s %s", code, f.stdout, f.stderr)
	}
	if len(f.up.applied) != 1 || !f.restarted() {
		t.Fatalf("--force: applied %v, service calls %v", f.up.applied, f.svc.calls)
	}
}

// A running service whose control API doesn't answer may still have jobs,
// so update changes nothing unless --force (review r2 partB sol-F1).
func TestUpdateRefusesWhenRunningServiceDoesNotAnswer(t *testing.T) {
	f := updateFixture(t, 1)
	f.daemon.mu.Lock()
	f.daemon.statusErrs = 1
	f.daemon.mu.Unlock()
	code := f.run("", "update")
	if code == ExitOK || !strings.Contains(f.stderr.String(), "did not answer") || !strings.Contains(f.stderr.String(), "--force") {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if len(f.up.applied) != 0 || f.restarted() || len(f.daemon.paused) != 0 {
		t.Fatalf("applied %v, service calls %v, paused %v", f.up.applied, f.svc.calls, f.daemon.paused)
	}
	f.daemon.mu.Lock()
	f.daemon.statusErrs = 1
	f.daemon.mu.Unlock()
	if code := f.run("", "update", "--force", "--json"); code != ExitOK {
		t.Fatalf("--force: exit %d: %s %s", code, f.stdout, f.stderr)
	}
	if len(f.up.applied) != 1 || !f.restarted() {
		t.Fatalf("--force: applied %v, service calls %v", f.up.applied, f.svc.calls)
	}
}

// A service the manager reports stopped has no jobs, so a control API
// that doesn't answer is no reason to refuse.
func TestUpdateProceedsWhenServiceIsStopped(t *testing.T) {
	f := updateFixture(t, 0)
	f.svc.running = false
	f.daemon.mu.Lock()
	f.daemon.statusErrs = 1
	f.daemon.mu.Unlock()
	if code := f.run("", "update", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, f.stdout, f.stderr)
	}
	if len(f.up.applied) != 1 || !f.restarted() {
		t.Fatalf("applied %v, service calls %v", f.up.applied, f.svc.calls)
	}
}

// Not installed: the binary is replaced and there is nothing to drain.
func TestUpdateWithNoServiceSkipsTheDrain(t *testing.T) {
	f := updateFixture(t, 0)
	f.svc.installed, f.svc.running = false, false
	f.daemon.mu.Lock()
	f.daemon.statusErrs = 1
	f.daemon.mu.Unlock()
	if code := f.run("", "update", "--json"); code != ExitOK || !strings.Contains(f.stdout.String(), "not installed") {
		t.Fatalf("exit %d: %s %s", code, f.stdout, f.stderr)
	}
	if len(f.up.applied) != 1 || f.restarted() {
		t.Fatalf("applied %v, service calls %v", f.up.applied, f.svc.calls)
	}
}
