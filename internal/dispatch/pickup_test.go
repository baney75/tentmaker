package dispatch

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

// pacedForge is the fake forge plus core.CheapPoller and core.PollPacer,
// as the GitHub client is.
type pacedForge struct {
	*fakeforge.Forge
	cheap *atomic.Bool
	delay *atomic.Int64 // PollDelay, nanoseconds
}

func (p pacedForge) CheapPolls() bool         { return p.cheap.Load() }
func (p pacedForge) PollDelay() time.Duration { return time.Duration(p.delay.Load()) }

func (r *rig) pacedBroker() pacedForge {
	pf := pacedForge{r.forge, &atomic.Bool{}, &atomic.Int64{}}
	pf.cheap.Store(true)
	r.broker = NewBroker(BrokerOptions{
		Config:       r.cfg,
		Forges:       map[string]core.Forge{"github": pf},
		StatusForges: map[string]core.StatusForge{"github": pf},
		Planner:      r.planner.plan,
		StateDir:     r.bdir,
		Log:          r.log,
		Now:          r.clk.Now,
	})
	return pf
}

// pendingAt reports whether the broker has posted pending for sha.
func (r *rig) pendingFor(sha string) bool {
	for _, p := range r.forge.Posts() {
		if p.SHA == sha && p.State == "pending" {
			return true
		}
	}
	return false
}

// pushAfterPoll waits, a second a tick as Run does, for the broker's next
// checks poll, then sets commits right after it (the worst moment to push)
// and returns how long, in fake time, the push took to get a pending status.
func (r *rig) pushAfterPoll(t *testing.T, commits []core.Commit, sha string) time.Duration {
	t.Helper()
	polls := r.forge.CallsFor(fakeforge.MPendingCommits, repoSite)
	for r.forge.CallsFor(fakeforge.MPendingCommits, repoSite) == polls {
		r.run(time.Second, time.Second)
	}
	r.forge.SetCommits(repoSite, commits)
	start := r.clk.Now()
	for !r.pendingFor(sha) {
		if r.clk.Now().Sub(start) > time.Minute {
			t.Fatalf("%s not picked up in a minute", short(sha))
		}
		r.run(time.Second, time.Second)
	}
	return r.clk.Now().Sub(start)
}

// A push is picked up by the next poll: within 2 s on an idle repo, and
// within 1 s while the repo is active (a job of it runs, or it had a new
// head in the last 5 minutes). Before, every push waited up to 2 s.
func TestPickupDelayAfterPush(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	r.cheapBroker()
	r.addWorker("alpha", 8)
	for _, sha := range []string{sha1, sha2, sha3} {
		r.forge.SetFile(repoSite, sha, ciWorkflow, []byte("test\n"))
	}
	r.tick()
	// Ten idle minutes: nothing to test.
	r.run(10*time.Minute, time.Second)
	main1 := core.Commit{SHA: sha1, Ref: "main", Author: "baney75"}
	if d := r.pushAfterPoll(t, []core.Commit{main1}, sha1); d > 2*time.Second {
		t.Fatalf("idle repo: picked up %v after the push, want at most 2s", d)
	}
	// sha1's job runs: the repo is hot.
	pr := core.Commit{SHA: sha2, Ref: "feature", PR: 7, Author: "baney75"}
	if d := r.pushAfterPoll(t, []core.Commit{pr, main1}, sha2); d > time.Second {
		t.Fatalf("active repo: picked up %v after the push, want at most 1s", d)
	}
	gaps := r.pollGapsChecks(30 * time.Second)
	for i, g := range gaps {
		if g != time.Second {
			t.Fatalf("hot gap %d = %v, want 1s", i, g)
		}
	}
}

// pollGapsChecks is pollGaps for the checks repo.
func (r *rig) pollGapsChecks(d time.Duration) []time.Duration {
	var at []time.Duration
	seen := r.forge.CallsFor(fakeforge.MPendingCommits, repoSite)
	start := r.clk.Now()
	for r.clk.Now().Sub(start) < d {
		r.run(time.Second, time.Second)
		if n := r.forge.CallsFor(fakeforge.MPendingCommits, repoSite); n != seen {
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

// An idle checks repo reads statuses only for heads with planned jobs, and
// a head whose jobs all have a status at most every statusRecheck. Before,
// every poll read every head's statuses: 60 reads a minute here.
func TestChecksIdleAPICallsPerMinute(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	r.cheapBroker()
	r.forge.SetFile(repoSite, sha2, ciWorkflow, []byte("test\n"))
	r.forge.SetStatus(repoSite, sha2, "tentmaker/test", "success")
	r.forge.SetCommits(repoSite, []core.Commit{
		{SHA: sha1, Ref: "main", Author: "baney75"}, // no workflow at sha1
		{SHA: sha2, Ref: "feature", PR: 7, Author: "baney75"},
	})
	r.tick()
	r.run(10*time.Minute, time.Second) // long idle: the repo is cold again
	polls := r.forge.Calls(fakeforge.MPendingCommits)
	reads := r.forge.Calls(fakeforge.MStatuses)
	r.run(time.Minute, time.Second)
	polls = r.forge.Calls(fakeforge.MPendingCommits) - polls
	reads = r.forge.Calls(fakeforge.MStatuses) - reads
	if polls != 30 {
		t.Fatalf("%d polls in an idle minute, want 30 (every 2s)", polls)
	}
	if reads > 4 {
		t.Fatalf("%d status reads in an idle minute, want at most 4 (one head, every %v)", reads, statusRecheck)
	}
	// A status another broker posts is seen within statusRecheck.
	r.forge.SetStatus(repoSite, sha2, "tentmaker/test", "failure")
	r.run(statusRecheck+2*time.Second, time.Second)
	for _, rs := range r.broker.repos {
		for _, h := range rs.heads {
			if h.commit.SHA == sha2 && h.statuses["tentmaker/test"] != "failure" {
				t.Fatalf("statuses %v not read again within %v", h.statuses, statusRecheck)
			}
		}
	}
}

// A forge low on its limit is polled as its PollDelay says, never stopped:
// one that is used up is not asked again before its reset, and fast polls
// come back once the reset passes.
func TestPacedPollingNeverStarves(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	pf := r.pacedBroker()
	r.tick()
	pf.cheap.Store(false)
	pf.delay.Store(int64(45 * time.Second))
	r.run(time.Minute, time.Second) // the old interval runs out
	for i, g := range r.pollGapsChecks(5 * time.Minute) {
		if g != 45*time.Second {
			t.Fatalf("low limit gap %d = %v, want PollDelay's 45s", i, g)
		}
	}
	// Used up for 20 minutes: polls stop until then, and only until then.
	pf.delay.Store(int64(20 * time.Minute))
	r.run(time.Minute, time.Second)
	before := r.forge.Calls(fakeforge.MPendingCommits)
	r.run(15*time.Minute, time.Second)
	if n := r.forge.Calls(fakeforge.MPendingCommits) - before; n != 0 {
		t.Fatalf("%d polls while the limit was used up", n)
	}
	pf.delay.Store(0)
	pf.cheap.Store(true)
	r.run(6*time.Minute, time.Second)
	for i, g := range r.pollGapsChecks(20 * time.Second) {
		if g != 2*time.Second {
			t.Fatalf("after the reset gap %d = %v, want 2s", i, g)
		}
	}
	// A refused poll (the forge says rate limited) waits for PollDelay too,
	// not the usual 5 s backoff.
	pf.delay.Store(int64(3 * time.Minute))
	r.forge.SetErr(fakeforge.MPendingCommits, repoSite, core.ErrRateLimited)
	r.run(3*time.Second, time.Second)
	before = r.forge.Calls(fakeforge.MPendingCommits)
	r.run(2*time.Minute, time.Second)
	if n := r.forge.Calls(fakeforge.MPendingCommits) - before; n != 0 {
		t.Fatalf("%d polls in the 3 minutes a refused poll should wait", n)
	}
}

// A poll refused just before the limit resets is retried after the usual
// backoff, not after the whole wait the paced poll before it used. Before,
// the refusal reused that wait as its backoff: in bench run 45184009840
// (issue #39) a repo paced 25 minutes to GitHub's reset polled 0.6 s early,
// was refused, and slept 25 minutes more with a commit waiting.
func TestRefusedPollAfterLongPaceRetriesSoon(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	pf := r.pacedBroker()
	r.tick()
	pf.cheap.Store(false)
	pf.delay.Store(int64(25 * time.Minute))
	waitPoll := func(limit time.Duration) time.Duration {
		t.Helper()
		polls := r.forge.CallsFor(fakeforge.MPendingCommits, repoSite)
		start := r.clk.Now()
		for r.forge.CallsFor(fakeforge.MPendingCommits, repoSite) == polls {
			if r.clk.Now().Sub(start) > limit {
				t.Fatalf("no poll within %v", limit)
			}
			r.run(time.Second, time.Second)
		}
		return r.clk.Now().Sub(start)
	}
	waitPoll(time.Minute) // this poll is paced 25 minutes, to the reset
	// At the reset the poll is refused: the forge still reads the limit
	// used up for a moment more.
	r.forge.FailTimes(fakeforge.MPendingCommits, repoSite, 1, core.ErrRateLimited)
	pf.delay.Store(int64(time.Second))
	waitPoll(26 * time.Minute)
	pf.delay.Store(0)
	pf.cheap.Store(true)
	if d := waitPoll(time.Hour); d > pollMin+time.Second {
		t.Fatalf("next poll %v after the refused one, want at most %v", d, pollMin+time.Second)
	}
}
