package dispatch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
)

func sbxSpec(name string, mods ...func(*SandboxSpec)) SandboxSpec {
	s := SandboxSpec{Name: name, Platform: core.Platform{OS: "linux"}, TTL: 30 * time.Minute, Owner: "1000"}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func noRoom(t *testing.T, err error, want string) {
	t.Helper()
	var nr *NoRoomError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want a NoRoomError", err)
	}
	if !strings.Contains(nr.Reason, want) {
		t.Fatalf("reason %q, want it to say %q", nr.Reason, want)
	}
}

func TestSandboxCountsAgainstTheBudget(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	wr := r.addWorker("w1", 3)
	r.tick()

	info, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "running" || info.Platform != linuxX64 || info.Limits.MemoryBytes != 2<<30 || info.Limits.CPUs != 2 {
		t.Fatalf("info %+v", info)
	}
	if !info.Expires.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("expires %v", info.Expires)
	}
	runs := wr.ex.Runs()
	if runs["sbx-000000000001"].Spec.Kind != core.KindSandbox || runs["sbx-000000000001"].Spec.MaxDuration != 30*time.Minute {
		t.Fatalf("executor got %+v", runs["sbx-000000000001"].Spec)
	}
	// Twice a job's memory takes two slots; 1 + 2 fits in 3.
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000002", func(s *SandboxSpec) { s.Limits.MemoryBytes = 4 << 30 })); err != nil {
		t.Fatal(err)
	}
	if got := wr.w.Status().Slots.Used; got != 3 {
		t.Fatalf("slots used %d, want 3", got)
	}
	_, err = wr.w.StartSandbox(bg, sbxSpec("sbx-000000000003"))
	noRoom(t, err, "3 in use")
	if called(wr.ex, "Start", "sbx-000000000003") {
		t.Fatal("a refused sandbox was started")
	}
	// CI jobs see no free slot while the sandboxes hold them.
	r.addJob(repoApp)
	r.run(time.Minute, 10*time.Second)
	for _, n := range started(wr.ex) {
		if !strings.HasPrefix(n, "sbx-") {
			t.Fatalf("a CI job started with every slot held by sandboxes: %s", n)
		}
	}
	// Removing one frees its slots for the job.
	if err := wr.w.StopSandbox(bg, "sbx-000000000002", "removed"); err != nil {
		t.Fatal(err)
	}
	if _, err := wr.w.Sandbox("sbx-000000000002"); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("removed sandbox still there: %v", err)
	}
	if !called(wr.ex, "Clean", "sbx-000000000002") {
		t.Fatal("a removed sandbox was not cleaned")
	}
	r.run(time.Minute, 10*time.Second)
	jobs := 0
	for _, n := range started(wr.ex) {
		if !strings.HasPrefix(n, "sbx-") {
			jobs++
		}
	}
	if jobs != 1 {
		t.Fatalf("%d CI jobs started after a sandbox freed its slots, want 1", jobs)
	}
	if v := wr.ex.Violations(); len(v) > 0 {
		t.Fatal(v)
	}
}

func TestSandboxRefusalsFailClosed(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 2)
	_, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001"))
	noRoom(t, err, "not been decided")
	r.tick()

	_, err = wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001", func(s *SandboxSpec) { s.Platform = core.Platform{OS: "macos"} }))
	noRoom(t, err, "nothing on this device runs macos")
	_, err = wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001", func(s *SandboxSpec) { s.Platform.Arch = "arm64" }))
	noRoom(t, err, "nothing on this device runs linux/arm64")
	_, err = wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001", func(s *SandboxSpec) { s.Limits.CPUs = 100 }))
	noRoom(t, err, "needs 50")

	wr.budget.set(core.Decision{Profile: "off", Slots: 0, Reasons: []string{"on battery"}})
	r.tick()
	_, err = wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001"))
	noRoom(t, err, "on battery")

	wr.budget.set(admit(2))
	r.tick()
	wr.ex.FailStarts(errBoom)
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if len(wr.w.Sandboxes()) != 0 || !called(wr.ex, "Clean", "sbx-000000000001") {
		t.Fatalf("a failed start left %v behind", wr.w.Sandboxes())
	}
	if _, err := wr.w.StartSandbox(bg, sbxSpec("bad name")); err == nil {
		t.Fatal("a bad name was accepted")
	}
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001", func(s *SandboxSpec) { s.TTL = 0 })); err == nil {
		t.Fatal("no TTL was accepted")
	}
}

// plainExecutor hides the fake's Exec, like an executor that can't hold
// sandboxes yet.
type plainExecutor struct{ core.Executor }

func TestSandboxNeedsASandboxer(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 2)
	wr.w = NewWorker(WorkerOptions{Config: r.cfg, Device: "w1", Executors: []core.Executor{plainExecutor{wr.ex}},
		Budget: wr.budget, Sources: func() []core.WorkSource { return nil }, StateDir: wr.dir, Now: r.clk.Now})
	r.tick()
	_, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001"))
	noRoom(t, err, "can't hold sandboxes")
}

func TestSandboxExpires(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 2)
	r.tick()
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001", func(s *SandboxSpec) { s.TTL = 5 * time.Minute })); err != nil {
		t.Fatal(err)
	}
	r.run(4*time.Minute, 30*time.Second)
	if _, err := wr.w.Sandbox("sbx-000000000001"); err != nil {
		t.Fatalf("expired early: %v", err)
	}
	r.run(2*time.Minute, 30*time.Second)
	if _, err := wr.w.Sandbox("sbx-000000000001"); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("still there after its TTL: %v", err)
	}
	if !called(wr.ex, "Stop", "sbx-000000000001") || !called(wr.ex, "Clean", "sbx-000000000001") {
		t.Fatal("an expired sandbox was not stopped and cleaned")
	}
	if !strings.Contains(r.logs.String(), "time to live ran out") {
		t.Fatal("the expiry was not logged")
	}
}

func TestSandboxExec(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 2)
	r.tick()
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001")); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code, err := wr.w.ExecSandbox(bg, "sbx-000000000001", core.SandboxExec{Argv: []string{"echo", "hi"}}, nil, &out, &errs)
	if err != nil || code != 0 || out.String() != "hi\n" {
		t.Fatalf("code %d err %v out %q", code, err, out.String())
	}
	code, _ = wr.w.ExecSandbox(bg, "sbx-000000000001", core.SandboxExec{Argv: []string{"sh", "-c", "exit 7"}}, nil, &out, &errs)
	if code != 7 {
		t.Fatalf("exit code %d, want 7", code)
	}
	// A command never outlives the sandbox.
	var got time.Duration
	wr.ex.SetExec(func(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		got = req.Timeout
		return 0, nil
	})
	r.clk.Add(10 * time.Minute)
	wr.w.ExecSandbox(bg, "sbx-000000000001", core.SandboxExec{Argv: []string{"true"}, Timeout: time.Hour}, nil, &out, &errs)
	if got != 20*time.Minute {
		t.Fatalf("timeout %v, want the 20m left", got)
	}
	if _, err := wr.w.ExecSandbox(bg, "sbx-000000000009", core.SandboxExec{Argv: []string{"true"}}, nil, &out, &errs); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("exec in a missing sandbox: %v", err)
	}
}

// A sandbox is no CI job: run history never lists it, running or ended.
func TestSandboxesStayOutOfRunHistory(t *testing.T) {
	r := newRig(t)
	rec := &recorder{}
	wr := &wrig{name: "w1", budget: &budget{d: admit(2)}, dir: t.TempDir(), runs: rec}
	wr.ex = fakeexec.New("fake-w1", 0, linuxX64)
	wr.w = r.newWorker(wr)
	r.workers = append(r.workers, wr)
	r.tick()
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001")); err != nil {
		t.Fatal(err)
	}
	if got := wr.w.RunningRecords(); len(got) != 0 {
		t.Fatalf("running records list a sandbox: %+v", got)
	}
	if err := wr.w.StopSandbox(bg, "sbx-000000000001", "removed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := rec.all(); len(got) != 0 {
		t.Fatalf("a removed sandbox was recorded as a CI job: %+v", got)
	}
}

func TestSandboxesEndWithTheDaemon(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 2)
	r.tick()
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001")); err != nil {
		t.Fatal(err)
	}
	wr.w.Shutdown(bg)
	if !called(wr.ex, "Stop", "sbx-000000000001") || len(wr.w.Sandboxes()) != 0 {
		t.Fatalf("shutdown left %v", wr.w.Sandboxes())
	}
}

func TestSandboxesEndAfterACrash(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 2)
	r.tick()
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001")); err != nil {
		t.Fatal(err)
	}
	// The daemon dies without a shutdown; the next one finds the sandbox
	// in its state and ends it.
	wr.w = r.newWorker(wr)
	r.tick()
	if _, err := wr.w.Sandbox("sbx-000000000001"); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("the sandbox outlived its daemon: %v", err)
	}
	if !called(wr.ex, "Stop", "sbx-000000000001") || !called(wr.ex, "Clean", "sbx-000000000001") {
		t.Fatal("not stopped and cleaned after the restart")
	}
}

// A sandbox runs as the user every job runs as, so it can reach a cache
// beside it through /proc. While the device holds a repo's cache, a job
// that holds it now or one that held it since the last reset, no sandbox
// starts.
func TestSandboxRefusedWhileACacheIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(w *Worker)
	}{
		{"a cached job runs", func(w *Worker) {
			j := heldBy(repoApp)
			j.Name, j.Executor, j.Kind = "tm-cached", "fake-w1", core.KindRunner
			w.st.Jobs = append(w.st.Jobs, j)
		}},
		{"a cached job ran since the reset", func(w *Worker) {
			w.st.Held = &core.CacheRepo{Forge: "github", Repo: repoApp}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			wr := r.addWorker("w1", 4)
			wr.ex.SetCachesRepos(true)
			r.tick()
			wr.w.mu.Lock()
			tc.hold(wr.w)
			wr.w.mu.Unlock()
			_, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001"))
			noRoom(t, err, "cache")
			if called(wr.ex, "Start", "sbx-000000000001") {
				t.Fatal("a sandbox started beside a held cache")
			}
		})
	}
}

// Anything a sandbox left behind can read or poison a later job's cache,
// so after a sandbox no repo gets one until the node is reset, even the
// repo whose trusted jobs alone ran before it.
func TestNoCacheAfterASandboxUntilAReset(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 4)
	wr.ex.SetCachesRepos(true)
	r.tick()
	e := wr.w.execByName("fake-w1")
	offer := cacheOffer(repoApp, true, false)
	cache := func() bool {
		wr.w.mu.Lock()
		defer wr.w.mu.Unlock()
		c, ok := wr.w.cacheDecisionLocked(e, offer)
		return ok && !c.IsZero()
	}
	wr.w.mu.Lock()
	wr.w.ranLocked(trustKey(core.CacheRepo{Forge: "github", Repo: repoApp}))
	wr.w.mu.Unlock()
	if !cache() {
		t.Fatal("no cache after only the repo's own trusted jobs ran")
	}
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001")); err != nil {
		t.Fatal(err)
	}
	if err := wr.w.StopSandbox(bg, "sbx-000000000001", "removed"); err != nil {
		t.Fatal(err)
	}
	if cache() {
		t.Fatal("a trusted job got its repo's cache after a sandbox ran, before a reset")
	}
	r.run(2*time.Minute, 10*time.Second)
	if wr.ex.Resets() == 0 {
		t.Fatal("no reset after the sandbox")
	}
	if !cache() {
		t.Fatal("no cache after the reset")
	}
}

// A reset stops every job namespace and removes every job folder it finds,
// so no sandbox is admitted while one runs: it would lose its own.
func TestNoSandboxStartsDuringAReset(t *testing.T) {
	r := newRig(t)
	wr := r.addWorker("w1", 4)
	r.tick()
	wr.w.mu.Lock()
	wr.w.markDirtyLocked()
	wr.w.mu.Unlock()
	resets := wr.ex.Resets()
	var err error
	ran := false
	wr.ex.SetResetHook(func() {
		ran = true
		_, err = wr.w.StartSandbox(bg, sbxSpec("sbx-000000000001"))
	})
	r.clk.Add(2 * time.Minute)
	r.tick()
	wr.ex.SetResetHook(nil)
	if !ran {
		t.Fatal("no reset ran")
	}
	noRoom(t, err, "resetting")
	if v := wr.ex.Violations(); len(v) > 0 {
		t.Fatal(v)
	}
	if wr.ex.Resets() != resets+1 {
		t.Fatalf("resets %d, want %d", wr.ex.Resets(), resets+1)
	}
	// Once the reset is done, sandboxes start again.
	if _, err := wr.w.StartSandbox(bg, sbxSpec("sbx-000000000002")); err != nil {
		t.Fatal(err)
	}
}
