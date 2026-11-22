package dispatch

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
	"github.com/baney75/tentmaker/internal/runs"
)

type recorder struct {
	mu   sync.Mutex
	recs []runs.Record
	logs [][]byte
}

func (r *recorder) JobEnded(rec runs.Record, log []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	r.logs = append(r.logs, log)
}

func (r *recorder) all() ([]runs.Record, [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runs.Record(nil), r.recs...), append([][]byte(nil), r.logs...)
}

// A runner job that ends leaves one record with its conclusion and failed
// step from the runner's log, the use sampled while it ran, and the log
// read before Clean took the job's folder.
func TestWorkerRecordsEndedJob(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	rec := &recorder{}
	wr := &wrig{name: "alpha", budget: &budget{d: admit(2)}, dir: t.TempDir(), sources: []core.WorkSource{r.broker}, runs: rec}
	wr.ex = fakeexec.New("fake-alpha", 0, linuxX64)
	wr.w = r.newWorker(wr)
	r.workers = append(r.workers, wr)

	j := r.addJob(repoApp)
	r.tick()
	name := started(wr.ex)[0]
	wr.ex.SetUsage(name, core.Usage{PeakMemoryBytes: 3 << 30, CPUSeconds: 41.5})
	r.run(6*time.Second, 2*time.Second)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.run(6*time.Second, 2*time.Second)

	running := wr.w.RunningRecords()
	if len(running) != 1 || running[0].Conclusion != runs.Running || running[0].Slot != 1 || running[0].Device != "alpha" {
		t.Fatalf("running = %+v", running)
	}
	wr.ex.SetLog(name, []byte("live line\n"))
	if l, ok, err := wr.w.JobLog(bg, name, 1024); !ok || err != nil || string(l.Data) != "live line\n" {
		t.Fatalf("live log = %q %v %v", l.Data, ok, err)
	}
	if _, ok, _ := wr.w.JobLog(bg, "nobody", 1024); ok {
		t.Fatalf("found a job that is not here")
	}

	wr.ex.SetLog(name, []byte("[x] Processing step: DisplayName='go test'\n[x] Step result: Failed\n[x] Job result after all job steps finish: Failed\n"))
	wr.ex.SetUsage(name, core.Usage{}) // the cgroup is gone at the end
	r.forge.Finish(name)
	wr.ex.Exit(name)
	r.run(2*time.Second, 2*time.Second)

	recs, logs := rec.all()
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	got := recs[0]
	if got.ID != name || got.Repo != repoApp || got.Conclusion != runs.Failure || got.FailedStep != "go test" {
		t.Fatalf("record = %+v", got)
	}
	if got.PeakMemoryBytes != 3<<30 || got.CPUSeconds != 41.5 {
		t.Fatalf("usage lost: %+v", got)
	}
	if got.StartedAt.IsZero() || got.EndedAt.Before(got.StartedAt) || got.Slot != 1 {
		t.Fatalf("times or slot wrong: %+v", got)
	}
	if !strings.Contains(string(logs[0]), "go test") {
		t.Fatalf("log = %q", logs[0])
	}
	if n := len(wr.w.RunningRecords()); n != 0 {
		t.Fatalf("still %d running", n)
	}
}

// A job Tentmaker stops is cancelled, with why in the note.
func TestWorkerRecordsStoppedJob(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	rec := &recorder{}
	wr := &wrig{name: "alpha", budget: &budget{d: admit(2)}, dir: t.TempDir(), sources: []core.WorkSource{r.broker}, runs: rec}
	wr.ex = fakeexec.New("fake-alpha", 0, linuxX64)
	wr.w = r.newWorker(wr)
	r.workers = append(r.workers, wr)

	r.addJob(repoApp)
	r.tick()
	r.run(4*time.Second, 2*time.Second)
	wr.budget.set(core.Decision{Profile: "off", Evict: true, Reasons: []string{"free disk below 5GiB"}})
	r.run(6*time.Second, 2*time.Second)
	recs, _ := rec.all()
	if len(recs) != 1 || recs[0].Conclusion != runs.Cancelled || recs[0].Note == "" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestFreeSlot(t *testing.T) {
	w := &Worker{}
	w.st.Jobs = []*jobRec{{Slot: 1}, {Slot: 3}}
	if n := w.freeSlotLocked(); n != 2 {
		t.Fatalf("slot = %d", n)
	}
}

// The broker names queued runner jobs by sha and pending checks by context.
func TestBrokerQueued(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	r.addJob(repoApp, func(j *core.Job) { j.HeadSHA = sha1; j.RunID = 5 })
	r.addJob(repoApp, func(j *core.Job) { j.HeadSHA = sha2; j.Name = "other"; j.RunID = 6 })
	r.addJob(repoApp, func(j *core.Job) { j.HeadSHA = sha1; j.Labels = []string{"ubuntu-latest"} })
	r.tick() // no worker: the jobs stay queued
	got := r.broker.Queued(strings.ToUpper(repoApp), sha1[:8], 0)
	if len(got) != 1 || !strings.Contains(got[0], "test") {
		t.Fatalf("queued = %v", got)
	}
	if got := r.broker.Queued(repoApp, "", 6); len(got) != 1 || !strings.Contains(got[0], "other") {
		t.Fatalf("queued by run = %v", got)
	}
	if got := r.broker.Queued(repoApp, "", 0); len(got) != 0 {
		t.Fatalf("an unscoped ask = %v", got)
	}
	if got := r.broker.Queued("someone/else", sha1, 0); len(got) != 0 {
		t.Fatalf("other repo = %v", got)
	}

	c := newRig(t, checksRepo(repoSite))
	c.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\nlint\n"))
	c.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
	c.forge.SetStatus(repoSite, sha1, "tentmaker/lint", "success")
	c.tick()
	if got := c.broker.Queued(repoSite, sha1, 0); len(got) != 1 || got[0] != "tentmaker/test" {
		t.Fatalf("checks queued = %v", got)
	}
}

type reports struct {
	mu  sync.Mutex
	got []runs.ForgeReport
}

func (r *reports) ForgeReport(rep runs.ForgeReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, rep)
}

func (r *reports) all() []runs.ForgeReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runs.ForgeReport(nil), r.got...)
}

// endJob runs one job on wr to its end at the forge and on the device.
func endJob(t *testing.T, r *rig, wr *wrig) (core.Job, string) {
	t.Helper()
	n := len(started(wr.ex))
	j := r.addJob(repoApp)
	for i := 0; i < 10 && len(started(wr.ex)) == n; i++ {
		r.run(2*time.Second, 2*time.Second)
	}
	names := started(wr.ex)
	if len(names) == n {
		t.Fatal("no runner started")
	}
	name := names[len(names)-1]
	r.run(6*time.Second, 2*time.Second)
	if err := r.forge.Take(name, j.ID); err != nil {
		t.Fatal(err)
	}
	r.run(6*time.Second, 2*time.Second)
	return j, name
}

// The broker reads each ended runner job's result from the forge and hands
// it over: a log not up yet is tried again, then sent without one (the
// record keeps the diag log); a result it can't read is given up on.
func TestBrokerForgeReports(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	rp := &reports{}
	r.broker.o.Reports = rp
	wr := r.addWorker("alpha", 1, linuxX64)

	j, name := endJob(t, r, wr)
	r.forge.SetReport(repoApp, j.ID, core.JobReport{HeadSHA: "abc1234def", Workflow: "CI", Conclusion: "failure", FailedStep: "go test", LogErr: errors.New("404")}, nil)
	r.forge.Finish(name)
	wr.ex.Exit(name)
	r.run(10*time.Second, 2*time.Second)
	if got := rp.all(); len(got) != 0 {
		t.Fatalf("sent before the log retries: %+v", got)
	}
	r.run(30*time.Second, 2*time.Second)
	got := rp.all()
	if len(got) != 1 || got[0].ID != name || got[0].JobID != j.ID || got[0].Device != "alpha" ||
		got[0].Conclusion != "failure" || got[0].FailedStep != "go test" || got[0].Log != nil ||
		got[0].SHA != "abc1234def" || got[0].Workflow != "CI" {
		t.Fatalf("reports = %+v", got)
	}

	j, name = endJob(t, r, wr)
	r.forge.SetReport(repoApp, j.ID, core.JobReport{}, errors.New("github: 502"))
	r.forge.Finish(name)
	wr.ex.Exit(name)
	before := r.forge.Calls("JobReport")
	r.run(5*time.Minute, 2*time.Second)
	if got := rp.all(); len(got) != 1 {
		t.Fatalf("an unreadable result was sent: %+v", got)
	}
	if n := r.forge.Calls("JobReport") - before; n != reportTries {
		t.Fatalf("JobReport tried %d times, want %d", n, reportTries)
	}
}
