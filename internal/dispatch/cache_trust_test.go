package dispatch

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
)

func cacheOffer(repo string, safe, drain bool) core.WorkOffer {
	return core.WorkOffer{Cache: safe, Drain: drain, Job: core.Job{Forge: "github", Repo: repo, Name: "test"}}
}

// Every job on a device runs as one user and can reach the others through
// /proc, so a cache goes only to a device where every job is vouched safe
// for that repo, and while one job holds it nothing else may start.
func TestCacheIsExclusiveToOneTrustedRepo(t *testing.T) {
	ex := fakeexec.New("fake", 0, linuxX64)
	e := &execInfo{ex: ex, name: "fake"}
	w := &Worker{}
	cached := func(off core.WorkOffer) (bool, bool) {
		c, ok := w.cacheDecisionLocked(e, off)
		return !c.IsZero(), ok
	}
	if c, ok := cached(cacheOffer(repoApp, true, false)); c || !ok {
		t.Fatalf("an executor without repo caches gave one: cached %v ok %v", c, ok)
	}
	ex.SetCachesRepos(true)
	for _, tc := range []struct {
		name      string
		jobs      []*jobRec
		off       core.WorkOffer
		cache, ok bool
	}{
		{"empty device", nil, cacheOffer(repoApp, true, false), true, true},
		{"drain", nil, cacheOffer(repoApp, true, true), false, true},
		{"untrusted", nil, cacheOffer(repoApp, false, false), false, true},
		{"beside another repo's cold job", []*jobRec{{Forge: "github", Repo: repoSite}}, cacheOffer(repoApp, true, false), false, true},
		{"beside a cold job of its own repo", []*jobRec{{Forge: "github", Repo: repoApp, Trust: trustKey(core.CacheRepo{Forge: "github", Repo: repoApp})}}, cacheOffer(repoApp, true, false), true, true},
		{"beside an untrusted job of its own repo", []*jobRec{{Forge: "github", Repo: repoApp}}, cacheOffer(repoApp, true, false), false, true},
		{"another repo while one holds a cache", []*jobRec{heldBy(repoApp)}, cacheOffer(repoSite, true, false), false, false},
		{"an untrusted job of the holding repo", []*jobRec{heldBy(repoApp)}, cacheOffer(repoApp, false, false), false, false},
		{"a drained job of another repo", []*jobRec{heldBy(repoApp)}, cacheOffer(repoSite, true, true), false, false},
		{"the holding repo, any case", []*jobRec{heldBy(repoApp)}, cacheOffer("Baney75/APP", true, false), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.st.Jobs, w.st.Dirty, w.st.DirtyTrust = tc.jobs, false, ""
			if c, ok := cached(tc.off); c != tc.cache || ok != tc.ok {
				t.Fatalf("cached %v ok %v, want %v %v", c, ok, tc.cache, tc.ok)
			}
		})
	}
}

// A job can leave a process behind that only a reset kills, and it can
// read a later job's cache through /proc. So after another repo's job, an
// untrusted job, a restart or a failed cleanup, no repo gets a cache until
// the node is reset; a run of one repo's trusted jobs keeps its cache.
func TestCacheWaitsForAResetAfterAnythingElseRan(t *testing.T) {
	ex := fakeexec.New("fake", 0, linuxX64)
	ex.SetCachesRepos(true)
	e := &execInfo{ex: ex, name: "fake"}
	app := trustKey(core.CacheRepo{Forge: "github", Repo: repoApp})
	site := trustKey(core.CacheRepo{Forge: "github", Repo: repoSite})
	for _, tc := range []struct {
		name  string
		ran   []string // trust keys of jobs started since the reset, in order
		dirty bool     // something other than a job dirtied the node
		cache bool
	}{
		{"clean node", nil, false, true},
		{"only its own trusted jobs ran", []string{app, app}, false, true},
		{"another repo's job ran and ended", []string{site}, false, false},
		{"its own job, then another repo's", []string{app, site}, false, false},
		{"another repo's, then its own", []string{site, app}, false, false},
		{"an untrusted job ran", []string{""}, false, false},
		{"a restart or failed cleanup", nil, true, false},
		{"its own jobs, then a failed cleanup", []string{app}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{}
			for _, tk := range tc.ran {
				w.ranLocked(tk)
			}
			if tc.dirty {
				w.markDirtyLocked()
			}
			c, ok := w.cacheDecisionLocked(e, cacheOffer(repoApp, true, false))
			if c.IsZero() == tc.cache || !ok {
				t.Fatalf("cached %v ok %v, want cached %v", !c.IsZero(), ok, tc.cache)
			}
		})
	}
	// A reset makes the node clean again.
	w := &Worker{}
	w.ranLocked(site)
	w.st.Dirty, w.st.DirtyTrust = false, ""
	if c, _ := w.cacheDecisionLocked(e, cacheOffer(repoApp, true, false)); c.IsZero() {
		t.Fatal("no cache after a reset")
	}
}

func heldBy(repo string) *jobRec {
	return &jobRec{Forge: "github", Repo: repo, Trust: trustKey(core.CacheRepo{Forge: "github", Repo: repo}), Cached: true}
}

// One response with two repos' jobs runs both cold instead of declining one.
func TestMixedOffersRunCold(t *testing.T) {
	if !mixedTrust([]core.WorkOffer{cacheOffer(repoApp, true, false), cacheOffer(repoSite, true, false)}) {
		t.Fatal("two repos are mixed")
	}
	if !mixedTrust([]core.WorkOffer{cacheOffer(repoApp, true, false), cacheOffer(repoApp, false, false)}) {
		t.Fatal("a trusted and an untrusted job are mixed")
	}
	if mixedTrust([]core.WorkOffer{cacheOffer(repoApp, true, false), cacheOffer(repoApp, true, false)}) || mixedTrust(nil) {
		t.Fatal("one repo is not mixed")
	}
}

// While a device holds a repo's cache, the broker offers it only that
// repo's jobs, marked Drain once another repo waits; the other repo gets in
// once the device is empty and reset, since an executor may keep the
// repo's data reachable until then.
func TestBrokerKeepsOtherReposOffACachedDevice(t *testing.T) {
	r := newRig(t, checksRepo(repoSite), checksRepo(repoApp))
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
	r.tick()
	first := started(w.ex)
	if len(first) != 1 {
		t.Fatalf("starts = %v", first)
	}
	if spec, _ := w.ex.Spec(first[0]); spec.Cache.IsZero() {
		t.Fatal("the only job on the device should hold its repo's cache")
	}

	r.forge.SetFile(repoApp, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoApp, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
	r.forge.SetFile(repoSite, sha2, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha2, Ref: "main", Author: "baney75"}, {SHA: sha1, Ref: "main", Author: "baney75"}})
	r.run(6*time.Second, time.Second)
	names := started(w.ex)
	if len(names) != 2 {
		t.Fatalf("starts = %v, want the first job plus site's second", names)
	}
	for _, n := range names[1:] {
		spec, _ := w.ex.Spec(n)
		if spec.Grant.Repo != repoSite {
			t.Fatalf("%s of %s started beside a cached job of %s", n, spec.Grant.Repo, repoSite)
		}
		if !spec.Cache.IsZero() {
			t.Fatalf("%s got the cache though another repo waits (drain)", n)
		}
	}
	if l := r.logs.String(); strings.Contains(l, "a job holding another repo") {
		t.Fatal("the broker offered a conflicting job instead of holding it back")
	}

	w.ex.SetResult(first[0], core.CheckResult{State: "success", Description: "ok"})
	w.ex.Exit(first[0])
	r.run(8*time.Second, time.Second)
	if n := len(started(w.ex)); n != 2 {
		t.Fatalf("starts = %d, want none while site's job runs on a device that held its cache", n)
	}
	for _, n := range names[1:] {
		w.ex.SetResult(n, core.CheckResult{State: "success", Description: "ok"})
		w.ex.Exit(n)
	}
	r.run(70*time.Second, 5*time.Second) // resets are a minute apart
	if n := len(started(w.ex)); n != 3 {
		t.Fatalf("starts = %d, want app's job once the device is reset\n%s", n, r.logs.String())
	}
}

// After a cached job ends, the device stays held for its repo until a
// reset: another repo's job, and a drained job of the same repo, are
// declined, and the reset frees it.
func TestCacheHoldLastsUntilReset(t *testing.T) {
	ex := fakeexec.New("fake", 0, linuxX64)
	ex.SetCachesRepos(true)
	e := &execInfo{ex: ex, name: "fake"}
	app := core.CacheRepo{Forge: "github", Repo: repoApp}
	w := &Worker{}
	w.st.Held = &app
	w.ranLocked(trustKey(app))
	if got := w.cachedRepoLocked(); got == nil || got.Repo != repoApp {
		t.Fatalf("OnlyRepo %+v with no job running, want %s until the reset", got, repoApp)
	}
	for _, tc := range []struct {
		name      string
		jobs      []*jobRec
		off       core.WorkOffer
		cache, ok bool
	}{
		{"another repo", nil, cacheOffer(repoSite, true, false), false, false},
		{"another repo, untrusted", nil, cacheOffer(repoSite, false, false), false, false},
		{"its own repo", nil, cacheOffer(repoApp, true, false), true, true},
		{"its own repo, drained", nil, cacheOffer(repoApp, true, true), false, true},
		{"its own repo, drained beside its cached job", []*jobRec{heldBy(repoApp)}, cacheOffer(repoApp, true, true), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.st.Jobs = tc.jobs
			c, ok := w.cacheDecisionLocked(e, tc.off)
			if !c.IsZero() != tc.cache || ok != tc.ok {
				t.Fatalf("cached %v ok %v, want %v %v", !c.IsZero(), ok, tc.cache, tc.ok)
			}
		})
	}
}

// Starting a cached job holds the device; a successful reset frees it, a
// failed one doesn't.
func TestResetFreesTheCacheHold(t *testing.T) {
	r := newRig(t, checksRepo(repoApp))
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	r.forge.SetFile(repoApp, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoApp, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
	r.tick()
	first := started(w.ex)
	if len(first) != 1 {
		t.Fatalf("starts = %v", first)
	}
	held := func() *core.CacheRepo {
		w.w.mu.Lock()
		defer w.w.mu.Unlock()
		return w.w.st.Held
	}
	if h := held(); h == nil || h.Repo != repoApp {
		t.Fatalf("held %+v after a cached start", h)
	}
	w.ex.SetResetErr(errors.New("boom"))
	w.ex.SetResult(first[0], core.CheckResult{State: "success", Description: "ok"})
	w.ex.Exit(first[0])
	r.run(3*time.Second, time.Second)
	if h := held(); h == nil {
		t.Fatal("a failed reset freed the hold")
	}
	w.ex.SetResetErr(nil)
	r.run(2*time.Minute, 10*time.Second)
	if h := held(); h != nil {
		t.Fatalf("held %+v after a reset", h)
	}
}

// Two repos' jobs offered in one exchange both start, cold, instead of the
// second being declined for the first's cache.
func TestOneExchangeOfTwoReposStartsBoth(t *testing.T) {
	r := newRig(t, checksRepo(repoSite), checksRepo(repoApp))
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	for _, repo := range []string{repoSite, repoApp} {
		r.forge.SetFile(repo, sha1, ciWorkflow, []byte("test\n"))
		r.forge.SetCommits(repo, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
	}
	r.tick()
	names := started(w.ex)
	if len(names) != 2 {
		t.Fatalf("starts = %v, want both repos' jobs", names)
	}
	for _, n := range names {
		if spec, _ := w.ex.Spec(n); !spec.Cache.IsZero() {
			t.Fatalf("%s got a cache beside another repo's job", n)
		}
	}
}

// Runner mode: while a private, fork-free repo's runner holds its cache,
// another repo's queued job waits for the device.
func TestRunnerCacheKeepsOtherReposWaiting(t *testing.T) {
	a := config.Repo{Name: repoApp, Mode: "runner", AllowPublic: true}
	b := config.Repo{Name: repoSite, Mode: "runner", AllowPublic: true}
	r := newRig(t, a, b)
	for _, n := range []string{repoApp, repoSite} {
		r.forge.SetRepo(core.RepoInfo{Repo: n, DefaultBranch: "main", ForkPolicy: "off"})
	}
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	r.addJob(repoApp)
	r.run(10*time.Second, time.Second)
	first := started(w.ex)
	if len(first) != 1 {
		t.Fatalf("starts = %v", first)
	}
	if spec, _ := w.ex.Spec(first[0]); spec.Cache.IsZero() {
		t.Fatal("the only runner on the device should hold its repo's cache")
	}
	r.addJob(repoSite)
	r.run(10*time.Second, time.Second)
	for _, n := range started(w.ex) {
		if spec, _ := w.ex.Spec(n); spec.Grant.Repo != repoApp {
			t.Fatalf("a runner of %s started beside %s's cached runner", spec.Grant.Repo, repoApp)
		}
	}
	if strings.Contains(r.logs.String(), "a job holding another repo") {
		t.Fatal("the broker offered a conflicting runner instead of holding it back")
	}
}

// Once no job holds the cache, a device still held for its repo until a
// reset takes nothing while another repo waits, not even its own repo's
// next job, so it empties and resets instead of keeping the other repo out.
func TestIdleHoldEmptiesTheDeviceForAWaitingRepo(t *testing.T) {
	r := newRig(t, checksRepo(repoSite), checksRepo(repoApp))
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
	r.tick()
	first := started(w.ex)
	if len(first) != 1 {
		t.Fatalf("starts = %v", first)
	}
	w.ex.SetResult(first[0], core.CheckResult{State: "success", Description: "ok"})
	w.ex.Exit(first[0])
	r.forge.SetFile(repoApp, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoApp, []core.Commit{{SHA: sha1, Ref: "main", Author: "baney75"}})
	r.forge.SetFile(repoSite, sha2, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha2, Ref: "main", Author: "baney75"}, {SHA: sha1, Ref: "main", Author: "baney75"}})
	r.run(8*time.Second, time.Second)
	if names := started(w.ex); len(names) != 1 {
		t.Fatalf("starts = %v, want none before the reset", names)
	}
	r.run(70*time.Second, 5*time.Second) // resets are a minute apart
	app := false
	for _, n := range started(w.ex) {
		if spec, _ := w.ex.Spec(n); spec.Grant.Repo == repoApp {
			app = true
		}
	}
	if !app {
		t.Fatalf("app's job did not start after the reset: %v", started(w.ex))
	}
}
