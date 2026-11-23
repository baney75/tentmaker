package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// lentSource is a broker as a lent device's worker reaches it through the
// mesh: the host's mesh puts the guest's lend in the context (from its
// guest table), and the worker's side knows the lend it accepted.
type lentSource struct {
	inner core.WorkSource
	lend  core.Lend
	mu    sync.Mutex
	offs  []core.WorkOffer
}

func (s *lentSource) Name() string     { return s.inner.Name() }
func (s *lentSource) Lend() *core.Lend { l := s.lend; return &l }
func (s *lentSource) offers() []core.WorkOffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.WorkOffer(nil), s.offs...)
}

func (s *lentSource) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	resp, err := s.inner.Work(core.WithCallerLend(ctx, s.lend), req)
	s.mu.Lock()
	s.offs = append(s.offs, resp.Offers...)
	s.mu.Unlock()
	return resp, err
}

func grantedJobs(r *rig) map[int64]bool {
	got := map[int64]bool{}
	for _, g := range r.broker.Status().Runners {
		got[g.JobID] = true
	}
	return got
}

func TestTrustedGuestGetsOnlyListedVouchedJobs(t *testing.T) {
	open := "baney75/open-forks"
	r := newRig(t, runnerRepo(repoApp), runnerRepo(repoSite), runnerRepo(open))
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", ForkPolicy: "off"})
	r.forge.SetRepo(core.RepoInfo{Repo: repoSite, DefaultBranch: "main", ForkPolicy: "off"})
	r.forge.SetRepo(core.RepoInfo{Repo: open, DefaultBranch: "main", ForkPolicy: "approve_all"})
	w := r.addWorker("bob-pc", 10)
	src := &lentSource{inner: r.broker, lend: core.Lend{Trust: core.TrustTrusted, Repos: []string{repoApp, open}}}
	w.sources = []core.WorkSource{src}

	good := r.addJob(repoApp)
	fork := r.addJob(repoApp, func(j *core.Job) { j.FromFork, j.Event = true, core.EventPullRequest })
	prt := r.addJob(repoApp, func(j *core.Job) { j.Event = core.EventPullRequestTarget })
	offList := r.addJob(repoSite)
	reachable := r.addJob(open) // on the list, but a fork could reach its runner

	r.tick()
	r.run(10*time.Second, 2*time.Second)
	got := grantedJobs(r)
	if len(got) != 1 || !got[good.ID] {
		t.Fatalf("trusted guest granted %v, want only %d (not fork %d, pull_request_target %d, off-list %d, fork-reachable %d)",
			got, good.ID, fork.ID, prt.ID, offList.ID, reachable.ID)
	}
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("trusted guest started %d runners", n)
	}
	for _, g := range r.broker.Status().Runners {
		if g.JobID == good.ID && g.Device != "bob-pc" {
			t.Fatalf("grant %+v", g)
		}
	}

	// The owner's own device takes what the guest may not.
	alpha := r.addWorker("alpha", 10)
	r.run(10*time.Second, 2*time.Second)
	if got := grantedJobs(r); !got[offList.ID] || got[fork.ID] {
		t.Fatalf("owner device granted %v; it takes the off-list job and still no fork", got)
	}
	if len(started(alpha.ex)) == 0 {
		t.Fatal("the owner's device started nothing")
	}
}

func TestPublicGuestGetsOnlyForkPRsWithoutCacheOrSecrets(t *testing.T) {
	pub := "baney75/public"
	pubNo := "baney75/public-not-lent"
	r := newRig(t,
		config.Repo{Name: pub, Mode: "runner", AllowPublic: true, LendPublic: true},
		config.Repo{Name: pubNo, Mode: "runner", AllowPublic: true},
		runnerRepo(repoApp))
	r.forge.SetRepo(core.RepoInfo{Repo: pub, DefaultBranch: "main", Public: true, ForkPolicy: "approve_all"})
	r.forge.SetRepo(core.RepoInfo{Repo: pubNo, DefaultBranch: "main", Public: true, ForkPolicy: "approve_all"})
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", ForkPolicy: "off"})
	w := r.addWorker("bob-pc", 10)
	src := &lentSource{inner: r.broker, lend: core.Lend{Trust: core.TrustPublic}}
	w.sources = []core.WorkSource{src}

	forkPR := func(j *core.Job) { j.FromFork, j.Event, j.HeadBranch = true, core.EventPullRequest, "patch-1" }
	good := r.addJob(pub, forkPR)
	r.addJob(pub)                                                                                // a push: may use secrets
	r.addJob(pub, func(j *core.Job) { j.FromFork, j.Event = true, core.EventPullRequestTarget }) // runs with secrets
	r.addJob(pubNo, forkPR)                                                                      // repo did not opt in
	r.addJob(repoApp, forkPR)                                                                    // private

	r.tick()
	r.run(10*time.Second, 2*time.Second)
	got := grantedJobs(r)
	if len(got) != 1 || !got[good.ID] {
		t.Fatalf("public guest granted %v, want only the opted-in public repo's fork PR %d", got, good.ID)
	}
	offs := src.offers()
	if len(offs) != 1 {
		t.Fatalf("offers %d", len(offs))
	}
	if offs[0].Cache || offs[0].Actions != nil {
		t.Fatalf("a public guest got a cache grant: cache %v actions %+v", offs[0].Cache, offs[0].Actions)
	}
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("public guest started %d runners", n)
	}
	// An owner's device still never runs a fork's job.
	alpha := r.addWorker("alpha", 10)
	r.run(10*time.Second, 2*time.Second)
	for _, g := range r.broker.Status().Runners {
		if g.Device == "alpha" && g.Repo != repoApp && g.Repo != pub {
			t.Fatalf("owner device took %+v", g)
		}
	}
	_ = alpha
}

func TestRevokedGuestGetsNoWork(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", ForkPolicy: "off"})
	w := r.addWorker("bob-pc", 10)
	w.sources = []core.WorkSource{&lentSource{inner: r.broker, lend: core.Lend{Trust: core.TrustRevoked}}}
	r.addJob(repoApp)
	r.tick()
	r.run(10*time.Second, 2*time.Second)
	if got := grantedJobs(r); len(got) != 0 {
		t.Fatalf("revoked guest granted %v", got)
	}
	if n := len(started(w.ex)); n != 0 {
		t.Fatalf("revoked guest started %d runners", n)
	}
	// Called directly, the broker answers a revoked lend with nothing.
	resp, err := r.broker.Work(core.WithCallerLend(bg, core.Lend{Trust: core.TrustRevoked}),
		core.WorkRequest{Device: "bob-pc", Free: 5, Labels: jobLabels, Platforms: []core.Platform{linuxX64}})
	if err != nil || len(resp.Offers) != 0 {
		t.Fatalf("revoked: %d offers, err %v", len(resp.Offers), err)
	}
}

// A lent device's worker checks every offer against the lend it accepted,
// so a broker that hands it more (a bug, or a pool owner who changed the
// code) still doesn't run it.
func TestLentWorkerDeclinesOffersOutsideItsLend(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp), runnerRepo(repoSite))
	r.forge.SetRepo(core.RepoInfo{Repo: repoApp, DefaultBranch: "main", ForkPolicy: "off"})
	r.forge.SetRepo(core.RepoInfo{Repo: repoSite, DefaultBranch: "main", ForkPolicy: "off"})
	w := r.addWorker("bob-pc", 10)
	// The broker treats bob as an owner device (no lend in the context),
	// but bob accepted trusted for repoApp only.
	w.sources = []core.WorkSource{&ownerBrokerLentWorker{inner: r.broker, lend: core.Lend{Trust: core.TrustTrusted, Repos: []string{repoApp}}}}
	app := r.addJob(repoApp)
	r.addJob(repoSite)
	r.tick()
	r.run(10*time.Second, 2*time.Second)
	starts := started(w.ex)
	if len(starts) != 1 {
		t.Fatalf("lent worker started %v, want only the %s job", starts, repoApp)
	}
	if g := r.grantFor(starts[0]); g.JobID != app.ID {
		t.Fatalf("lent worker started job %d, want %d", g.JobID, app.ID)
	}
}

// ownerBrokerLentWorker: the broker sees no lend, the worker its own.
type ownerBrokerLentWorker struct {
	inner core.WorkSource
	lend  core.Lend
}

func (s *ownerBrokerLentWorker) Name() string     { return s.inner.Name() }
func (s *ownerBrokerLentWorker) Lend() *core.Lend { l := s.lend; return &l }
func (s *ownerBrokerLentWorker) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	return s.inner.Work(ctx, req)
}

func TestTakenRefusal(t *testing.T) {
	forkPR := core.Job{FromFork: true, Event: core.EventPullRequest}
	push := core.Job{Event: "push"}
	prt := core.Job{Event: core.EventPullRequestTarget}
	for _, c := range []struct {
		lend   string
		j      core.Job
		refuse bool
	}{
		{"", push, false},
		{"", forkPR, true},
		{core.TrustTrusted, push, false},
		{core.TrustTrusted, forkPR, true},
		{core.TrustTrusted, prt, true},
		{core.TrustPublic, forkPR, false},
		{core.TrustPublic, push, true},
		{core.TrustPublic, core.Job{FromFork: true, Event: core.EventPullRequestTarget}, true},
	} {
		if got := takenRefusal(grantRec{Lend: c.lend}, c.j) != ""; got != c.refuse {
			t.Errorf("lend %q job %+v: refused %v, want %v", c.lend, c.j, got, c.refuse)
		}
	}
}
