package dispatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

// cheapForge is the fake forge plus core.CheapPoller.
type cheapForge struct {
	*fakeforge.Forge
	cheap *atomic.Bool
}

func (c cheapForge) CheapPolls() bool { return c.cheap.Load() }

func (r *rig) cheapBroker() *atomic.Bool {
	cheap := &atomic.Bool{}
	cheap.Store(true)
	cf := cheapForge{r.forge, cheap}
	r.broker = NewBroker(BrokerOptions{
		Config:       r.cfg,
		Forges:       map[string]core.Forge{"github": cf},
		StatusForges: map[string]core.StatusForge{"github": cf},
		Planner:      r.planner.plan,
		StateDir:     r.bdir,
		Log:          r.log,
		Now:          r.clk.Now,
	})
	return cheap
}

// pollGaps ticks every second for d and returns the gaps between polls.
func (r *rig) pollGaps(repo string, d time.Duration) []time.Duration {
	var at []time.Duration
	seen := r.forge.CallsFor(fakeforge.MQueuedJobs, repo)
	start := r.clk.Now()
	for r.clk.Now().Sub(start) < d {
		r.run(time.Second, time.Second)
		if n := r.forge.CallsFor(fakeforge.MQueuedJobs, repo); n != seen {
			seen = n
			at = append(at, r.clk.Now().Sub(start))
		}
	}
	var gaps []time.Duration
	for i := 1; i < len(at); i++ {
		gaps = append(gaps, at[i]-at[i-1])
	}
	return gaps
}

// A forge whose unchanged polls are free is polled every 2 s, idle or not;
// once it says its limit runs low, or a poll fails, the usual backoff is back.
func TestCheapForgePollsEveryTwoSeconds(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	cheap := r.cheapBroker()
	r.tick()
	for i, g := range r.pollGaps(repoApp, 40*time.Second) {
		if g != 2*time.Second {
			t.Fatalf("idle gap %d = %v, want 2s", i, g)
		}
	}
	// The limit runs low: back to 5 s growing x1.5.
	cheap.Store(false)
	gaps := r.pollGaps(repoApp, 60*time.Second)
	if len(gaps) < 3 || gaps[0] < 5*time.Second || gaps[2] <= gaps[0] {
		t.Fatalf("gaps with the limit low = %v, want a growing backoff from 5s", gaps)
	}
	// Errors back off even while polls are cheap.
	cheap.Store(true)
	r.run(40*time.Second, time.Second)
	r.forge.SetErr(fakeforge.MQueuedJobs, repoApp, context.DeadlineExceeded)
	gaps = r.pollGaps(repoApp, 60*time.Second)
	if len(gaps) < 2 || gaps[0] < 5*time.Second || gaps[1] <= gaps[0] {
		t.Fatalf("gaps while polls fail = %v, want a growing backoff", gaps)
	}
	r.forge.SetErr(fakeforge.MQueuedJobs, repoApp, nil)
}

// Many repos stretch the fast interval to stay under the forge's
// secondary rate limit.
func TestCheapPollIntervalGrowsWithRepos(t *testing.T) {
	var repos []config.Repo
	for _, n := range []string{"baney75/a", "baney75/b", "baney75/c", "baney75/d", "baney75/e", "baney75/f"} {
		repos = append(repos, runnerRepo(n))
	}
	r := newRig(t, repos...)
	r.cheapBroker()
	r.tick()
	for i, g := range r.pollGaps("baney75/a", 30*time.Second) {
		if g != 3*time.Second {
			t.Fatalf("gap %d = %v, want 3s for 6 repos", i, g)
		}
	}
}

// Check jobs carry the repo cache; runner jobs only from a private repo
// with fork PR workflows off, and never a fork's job.
func TestOffersMarkCacheSafeJobsOnly(t *testing.T) {
	t.Run("checks", func(t *testing.T) {
		r := newRig(t, checksRepo(repoSite))
		w := r.addWorker("alpha", 8)
		r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
		r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
		r.tick()
		names := started(w.ex)
		if len(names) != 1 {
			t.Fatalf("starts = %v", names)
		}
		spec, _ := w.ex.Spec(names[0])
		if spec.Cache != (core.CacheRepo{Forge: "github", Repo: repoSite, Job: "test"}) {
			t.Fatalf("check job cache = %+v", spec.Cache)
		}
	})
	for _, tc := range []struct {
		name string
		info core.RepoInfo
		fork bool
		want bool
	}{
		{"private no forks", core.RepoInfo{ForkPolicy: "off"}, false, true},
		{"fork PRs need approval", core.RepoInfo{ForkPolicy: "approve_all"}, false, false},
		{"fork policy unknown", core.RepoInfo{ForkPolicy: "unknown"}, false, false},
		{"public", core.RepoInfo{Public: true, ForkPolicy: "off"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := config.Repo{Name: repoApp, Mode: "runner", AllowPublic: true}
			r := newRig(t, repo)
			info := tc.info
			info.Repo, info.DefaultBranch = repoApp, "main"
			r.forge.SetRepo(info)
			w := r.addWorker("alpha", 8)
			r.addJob(repoApp)
			r.run(10*time.Second, time.Second)
			names := started(w.ex)
			if len(names) == 0 {
				t.Skip("repo refused outright; nothing to check")
			}
			spec, _ := w.ex.Spec(names[0])
			if got := !spec.Cache.IsZero(); got != tc.want {
				t.Fatalf("cache = %+v, want cached %v", spec.Cache, tc.want)
			}
		})
	}
}

// The worker never trusts an offer for a fork's job, and an older broker
// that sends no Cache flag gets no cache.
func TestWorkerCacheNeedsTheBrokersWord(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  core.WorkOffer
		want bool
	}{
		{"safe", core.WorkOffer{Cache: true, Job: core.Job{Forge: "github", Repo: repoApp}}, true},
		{"old broker", core.WorkOffer{Job: core.Job{Forge: "github", Repo: repoApp}}, false},
		{"fork job", core.WorkOffer{Cache: true, Job: core.Job{Forge: "github", Repo: repoApp, FromFork: true}}, false},
		{"no repo", core.WorkOffer{Cache: true, Job: core.Job{Forge: "github"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := !offerCache(tc.off).IsZero(); got != tc.want {
				t.Fatalf("cache %v, want %v", got, tc.want)
			}
		})
	}
}

// Kick gets a worker to tick at once instead of at the next second, and
// kicks during a tick fold into one.
func TestKickTicksTheWorkerNow(t *testing.T) {
	if workerTick != time.Second {
		t.Fatalf("worker tick %v, want 1s", workerTick)
	}
	r := newRig(t, runnerRepo(repoApp))
	wr := r.addWorker("alpha", 1)
	var ticks atomic.Int32
	wr.sources = []core.WorkSource{countingSource{&ticks}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { wr.w.Run(ctx); close(done) }()
	waitFor(t, func() bool { return ticks.Load() >= 1 })
	before := ticks.Load()
	for i := 0; i < 5; i++ {
		wr.w.Kick()
	}
	waitFor(t, func() bool { return ticks.Load() > before })
	cancel()
	<-done
	if n := ticks.Load() - before; n > 2 {
		t.Fatalf("5 kicks made %d ticks, want them folded", n)
	}
}

// countingSource counts the worker's exchanges.
type countingSource struct{ n *atomic.Int32 }

func (c countingSource) Name() string { return "counter" }
func (c countingSource) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	c.n.Add(1)
	return core.WorkResponse{}, nil
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out; workerTick is 1s, so this only passes via Kick")
		}
		time.Sleep(time.Millisecond)
	}
}
