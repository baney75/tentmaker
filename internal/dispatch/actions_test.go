package dispatch

import (
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/actionscache"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
)

// fakeActions records what the worker asks of the Actions cache service.
type fakeActions struct {
	mu       sync.Mutex
	url      string
	grantErr error
	grants   map[string]actionscache.Binding
	revoked  []string
}

func newFakeActions() *fakeActions {
	return &fakeActions{url: "http://127.0.0.1:7436/", grants: map[string]actionscache.Binding{}}
}

func (f *fakeActions) URL() string { f.mu.Lock(); defer f.mu.Unlock(); return f.url }

func (f *fakeActions) Grant(b actionscache.Binding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grantErr != nil {
		return f.grantErr
	}
	f.grants[b.Job] = b
	return nil
}

func (f *fakeActions) Revoke(job string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.grants, job)
	f.revoked = append(f.revoked, job)
}

func (f *fakeActions) Limit(job string, a actionscache.Access) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.grants[job]; ok && b.Access > a {
		b.Access = a
		f.grants[job] = b
	}
}

func (f *fakeActions) grant(job string) (actionscache.Binding, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.grants[job]
	return b, ok
}

func actionsOffer(repo string, safe, drain bool) core.WorkOffer {
	off := cacheOffer(repo, safe, drain)
	off.Job.Event, off.Job.HeadBranch = "pull_request", "feature"
	off.Actions = &core.ActionsOffer{DefaultBranch: "main"}
	return off
}

func TestActionsAccessFollowsTheCacheDecision(t *testing.T) {
	ex := fakeexec.New("fake", 0, linuxX64)
	ex.SetCachesRepos(true)
	ex.SetActionsCache(t.TempDir())
	e := &execInfo{ex: ex, name: "fake"}
	svc := newFakeActions()
	app := trustKey(core.CacheRepo{Forge: "github", Repo: repoApp})
	for _, tc := range []struct {
		name string
		jobs []*jobRec
		off  core.WorkOffer
		kind string
		want string // "" means no grant
	}{
		{"cached job writes", nil, actionsOffer(repoApp, true, false), core.KindRunner, "write"},
		{"drained job reads", nil, actionsOffer(repoApp, true, true), core.KindRunner, "read"},
		{"untrusted job", nil, actionsOffer(repoApp, false, false), core.KindRunner, ""},
		{"fork job", nil, func() core.WorkOffer { o := actionsOffer(repoApp, true, false); o.Job.FromFork = true; return o }(), core.KindRunner, ""},
		{"not github.com", nil, func() core.WorkOffer { o := actionsOffer(repoApp, true, false); o.Actions = nil; return o }(), core.KindRunner, ""},
		{"check job", nil, actionsOffer(repoApp, true, false), core.KindCheck, ""},
		{"beside another repo's job", []*jobRec{{Forge: "github", Repo: repoSite}}, actionsOffer(repoApp, true, true), core.KindRunner, ""},
		{"beside its own repo's job", []*jobRec{{Forge: "github", Repo: repoApp, Trust: app}}, actionsOffer(repoApp, true, true), core.KindRunner, "read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{o: WorkerOptions{ActionsCache: svc}}
			w.st.Jobs = tc.jobs
			c, _ := w.cacheDecisionLocked(e, tc.off)
			got := w.actionsLocked(e, tc.off, tc.kind, c)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("granted %+v, want none", got)
			case tc.want != "" && (got == nil || got.Access != tc.want):
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
			if got != nil && (got.Ref != "refs/pull/head/feature" || got.Default != "refs/heads/main") {
				t.Fatalf("scopes %+v", got)
			}
		})
	}

	// Nothing when the service isn't listening or the executor's runner
	// isn't patched.
	w := &Worker{o: WorkerOptions{ActionsCache: svc}}
	off := actionsOffer(repoApp, true, false)
	c, _ := w.cacheDecisionLocked(e, off)
	svc.url = ""
	if got := w.actionsLocked(e, off, core.KindRunner, c); got != nil {
		t.Fatal("granted while the service is down")
	}
	svc.url = "http://127.0.0.1:7436/"
	ex.SetActionsCache("")
	if got := w.actionsLocked(e, off, core.KindRunner, c); got != nil {
		t.Fatal("granted on an unpatched executor")
	}
	w.o.ActionsCache = nil
	ex.SetActionsCache(t.TempDir())
	if got := w.actionsLocked(e, off, core.KindRunner, c); got != nil {
		t.Fatal("granted without a service")
	}
}

func TestAnotherRepoClosesTheActionsCache(t *testing.T) {
	svc := newFakeActions()
	w := &Worker{o: WorkerOptions{ActionsCache: svc}, log: slog.New(slog.DiscardHandler)}
	app := trustKey(core.CacheRepo{Forge: "github", Repo: repoApp})
	site := trustKey(core.CacheRepo{Forge: "github", Repo: repoSite})
	reader := &jobRec{Name: "tm-r", Forge: "github", Repo: repoApp, Trust: app, Actions: &actionsRec{Access: "read"}}
	same := &jobRec{Name: "tm-s", Forge: "github", Repo: repoApp, Trust: app, Actions: &actionsRec{Access: "write"}}
	w.st.Jobs = []*jobRec{reader, same}
	for _, j := range w.st.Jobs {
		svc.grants[j.Name] = actionscache.Binding{Job: j.Name, Access: actionscache.ParseAccess(j.Actions.Access)}
	}
	w.limitActionsLocked(app)
	if reader.Actions.Access != "read" || same.Actions.Access != "write" {
		t.Fatal("a job of the same repo lowered the cache")
	}
	w.limitActionsLocked(site)
	for _, j := range w.st.Jobs {
		if b, _ := svc.grant(j.Name); j.Actions.Access != "none" || b.Access != actionscache.None {
			t.Fatalf("%s kept %s / %v beside another repo's job", j.Name, j.Actions.Access, b.Access)
		}
	}
}

// A runner job of a private github.com repo with fork PRs off gets a write
// grant before it starts, its spec points at the service, and the grant is
// revoked when the job ends.
func TestRunnerJobGetsAnActionsGrant(t *testing.T) {
	repo := config.Repo{Name: repoApp, Mode: "runner"}
	r := newRig(t, repo)
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "trunk", ForkPolicy: "off"})
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	w.ex.SetActionsCache(t.TempDir())
	svc := newFakeActions()
	w.actions = svc
	w.w = r.newWorker(w)
	r.addJob(repoApp)
	r.run(10*time.Second, time.Second)
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v", names)
	}
	spec, _ := w.ex.Spec(names[0])
	if spec.ActionsCache != "http://127.0.0.1:7436/" {
		t.Fatalf("spec.ActionsCache = %q", spec.ActionsCache)
	}
	b, ok := svc.grant(names[0])
	if !ok || b.Access != actionscache.Write || b.Default != "refs/heads/trunk" || b.Repo.Repo != repoApp || b.Store == nil {
		t.Fatalf("grant %+v ok %v", b, ok)
	}
	w.ex.Exit(names[0])
	r.run(5*time.Second, time.Second)
	if _, ok := svc.grant(names[0]); ok || !slices.Contains(svc.revoked, names[0]) {
		t.Fatal("the grant outlived the job")
	}
}

func TestPublicRepoKeepsGitHubsCache(t *testing.T) {
	repo := config.Repo{Name: repoApp, Mode: "runner", AllowPublic: true}
	r := newRig(t, repo)
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", Public: true, ForkPolicy: "approve_all"})
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	w.ex.SetActionsCache(t.TempDir())
	svc := newFakeActions()
	w.actions = svc
	w.w = r.newWorker(w)
	r.addJob(repoApp)
	r.run(10*time.Second, time.Second)
	names := started(w.ex)
	if len(names) == 0 {
		t.Skip("repo refused outright")
	}
	spec, _ := w.ex.Spec(names[0])
	if spec.ActionsCache != "" {
		t.Fatalf("a public repo's job uses the device's cache: %q", spec.ActionsCache)
	}
	if _, ok := svc.grant(names[0]); ok {
		t.Fatal("a public repo's job was granted")
	}
}

func TestFailedBootRevokesTheGrant(t *testing.T) {
	repo := config.Repo{Name: repoApp, Mode: "runner"}
	r := newRig(t, repo)
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", ForkPolicy: "off"})
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	w.ex.SetActionsCache(t.TempDir())
	w.ex.FailStarts(errors.New("boom"))
	svc := newFakeActions()
	w.actions = svc
	w.w = r.newWorker(w)
	r.addJob(repoApp)
	r.run(3*time.Second, time.Second)
	if len(svc.revoked) == 0 {
		t.Fatal("a failed start left its grant")
	}
	for _, n := range svc.revoked {
		if _, ok := svc.grant(n); ok {
			t.Fatalf("%s still granted", n)
		}
	}
}

func TestGrantFailureFallsBackToGitHub(t *testing.T) {
	repo := config.Repo{Name: repoApp, Mode: "runner"}
	r := newRig(t, repo)
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", ForkPolicy: "off"})
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	w.ex.SetActionsCache(t.TempDir())
	svc := newFakeActions()
	svc.grantErr = errors.New("no")
	w.actions = svc
	w.w = r.newWorker(w)
	r.addJob(repoApp)
	r.run(10*time.Second, time.Second)
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v", names)
	}
	if spec, _ := w.ex.Spec(names[0]); spec.ActionsCache != "" {
		t.Fatal("a job whose grant failed was pointed at the service")
	}
}

// A daemon restart forgets every grant; the worker grants its running jobs
// again from state.json.
func TestRestartRegrantsRunningJobs(t *testing.T) {
	repo := config.Repo{Name: repoApp, Mode: "runner"}
	r := newRig(t, repo)
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", ForkPolicy: "off"})
	w := r.addWorker("alpha", 8)
	w.ex.SetCachesRepos(true)
	w.ex.SetActionsCache(t.TempDir())
	svc := newFakeActions()
	w.actions = svc
	w.w = r.newWorker(w)
	r.addJob(repoApp)
	r.run(10*time.Second, time.Second)
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v", names)
	}
	before, _ := svc.grant(names[0])

	fresh := newFakeActions()
	w.actions = fresh
	w.w = r.newWorker(w)
	r.tick()
	after, ok := fresh.grant(names[0])
	if !ok || after.Access != before.Access || after.Ref != before.Ref || after.Default != before.Default || after.Store == nil {
		t.Fatalf("after a restart: %+v ok %v, before %+v", after, ok, before)
	}
}

func TestBrokerMarksGitHubComOffersOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		forge config.Forge
		want  bool
	}{
		{"github.com by default", config.Forge{}, true},
		{"github.com spelled out", config.Forge{Kind: "github", URL: "https://api.github.com/"}, true},
		{"GitHub Enterprise Server", config.Forge{Kind: "github", URL: "https://ghe.example/api/v3"}, false},
		{"forgejo", config.Forge{Kind: "forgejo", URL: "https://code.example"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Broker{infos: map[string]*infoCache{"github:" + repoApp: {defaultBranch: "main"}}}
			b.o.Config.Forges = map[string]config.Forge{"github": tc.forge}
			got := b.actionsOffer(&grantRec{Forge: "github", Repo: repoApp})
			if (got != nil) != tc.want {
				t.Fatalf("offer %+v, want marked %v", got, tc.want)
			}
			if got != nil && got.DefaultBranch != "main" {
				t.Fatalf("default branch %q", got.DefaultBranch)
			}
		})
	}
}
