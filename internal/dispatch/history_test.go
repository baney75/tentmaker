package dispatch

import (
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
