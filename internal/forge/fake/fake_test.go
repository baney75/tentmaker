package fake

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

var ctx = context.Background()

func TestJITRunnerTakesAnyMatchingJobIncludingForks(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	f := New("github", func() time.Time { return now })
	fork := f.AddJob(core.Job{Repo: "o/r", Labels: []string{"self-hosted", "tentmaker"}, FromFork: true, CreatedAt: now.Add(-time.Hour)})
	mine := f.AddJob(core.Job{Repo: "o/r", Labels: []string{"self-hosted", "tentmaker"}})
	g, err := f.Grant(ctx, "o/r", "tm-a-00000001", []string{"Self-Hosted", "TENTMAKER", "linux"})
	if err != nil || len(g.Secret) == 0 || g.RunnerID == 0 {
		t.Fatalf("grant = %+v, %v", g, err)
	}
	got, ok := f.TakeAny("tm-a-00000001")
	if !ok || got.ID != fork.ID {
		t.Fatalf("took %d, want the older fork job %d", got.ID, fork.ID)
	}
	st, _ := f.Runner(ctx, "o/r", g.RunnerID)
	if !st.Exists || !st.Busy || st.JobID != fork.ID {
		t.Fatalf("runner = %+v", st)
	}
	f.HideJob("tm-a-00000001", true)
	if st, _ := f.Runner(ctx, "o/r", g.RunnerID); st.JobID != 0 {
		t.Fatalf("hidden job visible")
	}
	if err := f.DeleteRunner(ctx, "o/r", g.RunnerID); !errors.Is(err, ErrBusy) {
		t.Fatalf("deleting a busy runner = %v", err)
	}
	q, _ := f.QueuedJobs(ctx, "o/r")
	if len(q) != 1 || q[0].ID != mine.ID {
		t.Fatalf("queued = %+v", q)
	}
	running, _ := f.RunningJobs(ctx, "o/r")
	if len(running) != 1 || running[0].ID != fork.ID {
		t.Fatalf("running = %+v", running)
	}
	f.Finish("tm-a-00000001")
	if st, _ := f.Runner(ctx, "o/r", g.RunnerID); st.Exists {
		t.Fatalf("ephemeral runner survived its job")
	}
	if err := f.DeleteRunner(ctx, "o/r", g.RunnerID); err != nil {
		t.Fatalf("deleting a missing runner = %v", err)
	}
	if _, err := f.JobByID(ctx, "o/r", 99); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing job = %v", err)
	}
	if err := f.CancelRun(ctx, "o/r", mine.RunID); err != nil || len(f.Cancelled("o/r")) != 1 {
		t.Fatalf("cancel = %v", err)
	}
	if j, _ := f.Job("o/r", mine.ID); j.Status != "completed" {
		t.Fatalf("cancelled job status = %s", j.Status)
	}
}

func TestErrorInjectionAndCounts(t *testing.T) {
	f := New("github", nil)
	boom := errors.New("boom")
	f.SetErr(MQueuedJobs, "o/a", boom)
	if _, err := f.QueuedJobs(ctx, "o/a"); err != boom {
		t.Fatalf("o/a = %v", err)
	}
	if _, err := f.QueuedJobs(ctx, "o/b"); err != nil {
		t.Fatalf("o/b = %v", err)
	}
	f.FailTimes(MCheckRepo, "", 2, boom)
	for i, want := range []error{boom, boom, nil} {
		if _, err := f.CheckRepo(ctx, "o/a"); err != want {
			t.Fatalf("call %d = %v", i, err)
		}
	}
	if f.Calls(MQueuedJobs) != 2 || f.CallsFor(MQueuedJobs, "o/a") != 1 || f.Calls(MCheckRepo) != 3 {
		t.Fatalf("counts wrong")
	}
	if info, _ := f.CheckRepo(ctx, "o/new"); info.Public {
		t.Fatalf("unknown repos should read as private")
	}
	if _, err := f.Grant(ctx, "o/a", "BAD NAME", nil); err == nil {
		t.Fatalf("bad runner name accepted")
	}
}

func TestStatusForgeSide(t *testing.T) {
	f := New("forgejo", nil)
	f.SetCommits("o/r", []core.Commit{{SHA: "abc", Ref: "main"}})
	f.SetFile("o/r", "abc", ".forgejo/workflows/ci.yml", []byte("x"))
	f.SetFile("o/r", "abc", ".forgejo/workflows/sub/deep.yml", []byte("y"))
	cs, _ := f.PendingCommits(ctx, "o/r")
	if len(cs) != 1 {
		t.Fatal(cs)
	}
	names, err := f.ListDir(ctx, "o/r", "abc", ".forgejo/workflows")
	if err != nil || len(names) != 1 || names[0] != "ci.yml" {
		t.Fatalf("ListDir = %v %v", names, err)
	}
	if _, err := f.ListDir(ctx, "o/r", "abc", ".github/workflows"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("empty dir = %v", err)
	}
	if b, err := f.FileAt(ctx, "o/r", "abc", ".forgejo/workflows/ci.yml"); err != nil || string(b) != "x" {
		t.Fatalf("FileAt = %q %v", b, err)
	}
	url, header, _ := f.Archive(ctx, "o/r", "abc")
	if url == "" || header == "" {
		t.Fatal("archive")
	}
	if err := f.PostStatus(ctx, "o/r", "abc", "tentmaker/test", "success", "ok", ""); err != nil {
		t.Fatal(err)
	}
	sts, _ := f.Statuses(ctx, "o/r", "abc")
	if sts["tentmaker/test"] != "success" || len(f.Posts()) != 1 {
		t.Fatalf("statuses = %v", sts)
	}
}

func TestConcurrentUse(t *testing.T) {
	f := New("github", nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 50; k++ {
				f.AddJob(core.Job{Repo: "o/r", Labels: []string{"tentmaker"}})
				_, _ = f.QueuedJobs(ctx, "o/r")
				_, _ = f.Grant(ctx, "o/r", "tm-x-"+string(rune('a'+i))+"-"+time.Now().Format("150405.000000000")[7:], nil)
				_ = f.AllRunners()
			}
		}(i)
	}
	wg.Wait()
}
