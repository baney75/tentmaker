package dispatch

import (
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
			w.st.Jobs = tc.jobs
			if c, ok := cached(tc.off); c != tc.cache || ok != tc.ok {
				t.Fatalf("cached %v ok %v, want %v %v", c, ok, tc.cache, tc.ok)
			}
		})
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
// as soon as the cached job ends.
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
	if n := len(started(w.ex)); n != 3 {
		t.Fatalf("starts = %d, want app's job once the cache is free", n)
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
