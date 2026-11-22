package dispatch

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
	"github.com/baney75/tentmaker/internal/runs"
)

const failedDiag = "[x] Processing step: DisplayName='go test'\n[x] Step result: Failed\n[x] Job result after all job steps finish: Failed\n"

func keepRepo(name, d string) config.Repo {
	r := runnerRepo(name)
	if d != "" {
		v, _ := time.ParseDuration(d)
		r.Debug.KeepFailed = config.Duration(v)
	}
	return r
}

// debugRig is a rig with one worker that records its runs.
func debugRig(t *testing.T, repo config.Repo) (*rig, *wrig, *recorder) {
	r := newRig(t, repo)
	rec := &recorder{}
	wr := &wrig{name: "alpha", budget: &budget{d: admit(2)}, dir: t.TempDir(), sources: []core.WorkSource{r.broker}, runs: rec}
	wr.ex = fakeexec.New("fake-alpha", 0, linuxX64)
	wr.w = r.newWorker(wr)
	r.workers = append(r.workers, wr)
	return r, wr, rec
}

// failJob runs one job on wr and ends it failed, with tests set.
func failJob(t *testing.T, r *rig, wr *wrig, log string, tests *runs.TestResults) string {
	t.Helper()
	_, name := endJob(t, r, wr)
	wr.ex.SetLog(name, []byte(log))
	if tests != nil {
		wr.ex.SetTests(name, *tests)
	}
	r.forge.Finish(name)
	wr.ex.Exit(name)
	r.run(2*time.Second, 2*time.Second)
	return name
}

// The JUnit cases the executor found reach the run recorder with the log.
func TestWorkerRecordsTests(t *testing.T) {
	r, wr, rec := debugRig(t, runnerRepo(repoApp))
	tr := runs.TestResults{Files: []string{"runner/_work/app/report.xml"},
		Cases: []runs.TestCase{{Suite: "pkg", Name: "TestParse", Status: runs.TestFailed, Message: "want 1", Source: runs.SourceJUnit}}}
	name := failJob(t, r, wr, failedDiag, &tr)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.tests) != 1 || rec.tests[0] == nil || len(rec.tests[0].Cases) != 1 || rec.tests[0].Cases[0].Name != "TestParse" {
		t.Fatalf("tests = %+v", rec.tests)
	}
	if called(wr.ex, "Keep", name) || !called(wr.ex, "Clean", name) {
		t.Fatalf("a repo without keep_failed kept its sandbox")
	}
}

// A failed job in a repo with keep_failed keeps its sandbox: it is reported
// ended, holds its slot, keeps the repo's jobs off the device, can be
// debugged, and is cleaned up when its grace ends.
func TestWorkerKeepsFailedSandbox(t *testing.T) {
	r, wr, rec := debugRig(t, keepRepo(repoApp, "10m"))
	name := failJob(t, r, wr, failedDiag, nil)
	if !called(wr.ex, "Keep", name) || called(wr.ex, "Clean", name) || !wr.ex.Kept(name) {
		t.Fatalf("calls: keep %v clean %v", called(wr.ex, "Keep", name), called(wr.ex, "Clean", name))
	}
	recs, _ := rec.all()
	if len(recs) != 1 || recs[0].KeptUntil.IsZero() || recs[0].Conclusion != runs.Failure {
		t.Fatalf("records = %+v", recs)
	}
	kept := wr.w.KeptJobs()
	if len(kept) != 1 || kept[0].Name != name || kept[0].Repo != repoApp {
		t.Fatalf("kept = %+v", kept)
	}
	if n := len(wr.w.RunningRecords()); n != 0 {
		t.Fatalf("a kept job shows as running: %d", n)
	}
	if st := wr.w.Status(); len(st.Kept) != 1 || len(st.Running) != 0 {
		t.Fatalf("status kept %d running %d", len(st.Kept), len(st.Running))
	}

	// The repo's next job doesn't come here while the sandbox is kept.
	n := len(started(wr.ex))
	r.addJob(repoApp)
	r.run(10*time.Second, 2*time.Second)
	if len(started(wr.ex)) != n {
		t.Fatalf("a job of %s started on a device with its sandbox kept", repoApp)
	}

	// debug_exec: a capped, redacted answer.
	wr.ex.SetDebugExec(func(_, cmd string) (core.DebugResult, error) {
		return core.DebugResult{ExitCode: 2, Output: []byte(strings.Repeat("x", 100) + "\nGITHUB_TOKEN=ghp_" + strings.Repeat("C", 36) + "\n")}, nil
	})
	res, err := wr.w.DebugExec(bg, name, "cat out.txt", 0, 64)
	if err != nil || res.ExitCode != 2 || !res.Truncated || strings.Contains(string(res.Output), "ghp_") {
		t.Fatalf("debug exec = %+v %q %v", res, res.Output, err)
	}
	if calls := wr.ex.DebugCalls(); len(calls) != 1 || calls[0].Command != "cat out.txt" || calls[0].Spec.Timeout != DebugTimeout {
		t.Fatalf("debug calls = %+v", calls)
	}
	if _, err := wr.w.DebugExec(bg, name, "true", 2*MaxDebugTimeout, 0); err == nil {
		t.Fatal("a timeout past the maximum was taken")
	}
	if _, err := wr.w.DebugExec(bg, "tm-alpha-nobody", "true", 0, 0); !errors.Is(err, ErrNotKept) {
		t.Fatalf("debug of a job not kept = %v", err)
	}
	var out strings.Builder
	if err := wr.w.DebugShell(bg, name, strings.NewReader("ls\n"), &out); err != nil || !strings.Contains(out.String(), "ls") {
		t.Fatalf("shell = %q %v", out.String(), err)
	}

	// The grace ends: cleaned up like any job, and the repo's job starts.
	r.run(11*time.Minute, 30*time.Second)
	if !called(wr.ex, "Clean", name) || len(wr.w.KeptJobs()) != 0 {
		t.Fatalf("kept sandbox not cleaned up after its grace")
	}
	if len(started(wr.ex)) == n {
		t.Fatalf("the repo's job did not start after the sandbox was released")
	}
	if _, err := wr.w.DebugExec(bg, name, "true", 0, 0); !errors.Is(err, ErrNotKept) {
		t.Fatalf("debug after release = %v", err)
	}
}

// The owner can end the grace early.
func TestWorkerReleaseKept(t *testing.T) {
	r, wr, _ := debugRig(t, keepRepo(repoApp, "1h"))
	name := failJob(t, r, wr, failedDiag, nil)
	if err := wr.w.ReleaseKept("tm-nobody"); !errors.Is(err, ErrNotKept) {
		t.Fatalf("release of nothing = %v", err)
	}
	if err := wr.w.ReleaseKept(name); err != nil {
		t.Fatal(err)
	}
	r.run(2*time.Second, 2*time.Second)
	if !called(wr.ex, "Clean", name) || len(wr.w.KeptJobs()) != 0 {
		t.Fatal("released sandbox not cleaned up")
	}
}

// Only a job that failed on its own is kept, and only where the executor
// can keep it.
func TestWorkerKeepsOnlyFailures(t *testing.T) {
	r, wr, _ := debugRig(t, keepRepo(repoApp, "10m"))
	name := failJob(t, r, wr, "[x] Job result after all job steps finish: Succeeded\n", nil)
	if called(wr.ex, "Keep", name) || !called(wr.ex, "Clean", name) {
		t.Fatal("a passing job's sandbox was kept")
	}

	r, wr, _ = debugRig(t, keepRepo(repoApp, "10m"))
	wr.ex.SetKeepErr(errors.New("can't"))
	name = failJob(t, r, wr, failedDiag, nil)
	if !called(wr.ex, "Clean", name) || len(wr.w.KeptJobs()) != 0 {
		t.Fatal("a sandbox the executor could not keep was not cleaned up")
	}
}

// A hard guard (evict) cleans up a kept sandbox at once.
func TestWorkerEvictReleasesKept(t *testing.T) {
	r, wr, _ := debugRig(t, keepRepo(repoApp, "1h"))
	name := failJob(t, r, wr, failedDiag, nil)
	wr.budget.set(core.Decision{Profile: "off", Evict: true, Reasons: []string{"free disk below 5GiB"}})
	r.run(6*time.Second, 2*time.Second)
	if !called(wr.ex, "Clean", name) || len(wr.w.KeptJobs()) != 0 {
		t.Fatal("evict left a kept sandbox")
	}
}

// The broker offers no work for a repo a device keeps a sandbox of.
func TestBrokerSkipsKeptRepo(t *testing.T) {
	req := core.WorkRequest{Kept: []core.CacheRepo{{Forge: "github", Repo: "O/App"}}}
	if !keptRepo(req, "github", "o/app") || keptRepo(req, "github", "o/other") || keptRepo(req, "gitea", "o/app") {
		t.Fatal("keptRepo")
	}
}
