package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

type firstJobForge struct {
	*fakeforge.Forge
	fj  core.FirstJob
	err error
	prs []core.PullRequest
}

func (f *firstJobForge) FirstJob(ctx context.Context, repo string) (core.FirstJob, error) {
	return f.fj, f.err
}

func (f *firstJobForge) OpenOrUpdatePR(ctx context.Context, pr core.PullRequest) (core.PullRequestResult, error) {
	f.prs = append(f.prs, pr)
	return core.PullRequestResult{Number: 9, URL: "https://github.com/o/r/pull/9", Created: true}, nil
}

func firstJob(t *testing.T, fj core.FirstJob, err error, mode string) []Finding {
	t.Helper()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	in := DoctorInput{
		Config: config.Config{Repos: []config.Repo{{Name: "o/r", Mode: mode}}},
		Forges: Forges{Forges: map[string]core.Forge{"github": &firstJobForge{Forge: fakeforge.New("github", nil), fj: fj, err: err}}},
		Now:    func() time.Time { return now },
	}
	return checkFirstJob(context.Background(), in)
}

func TestFirstJobCheck(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		fj     core.FirstJob
		err    error
		status string
		want   string
	}{
		{"online", core.FirstJob{Online: []string{"tm-box-1"}}, nil, OK, "tm-box-1 is online"},
		{"ran", core.FirstJob{RanJob: "test", RanOn: "tm-box-2", RanAt: now.Add(-time.Hour)}, nil, OK, "ran on Tentmaker runner tm-box-2"},
		{"nothing yet", core.FirstJob{}, nil, Warn, "no job has run"},
		{"just queued", core.FirstJob{Queued: 1, OldestQueued: now.Add(-time.Minute)}, nil, Warn, "are queued"},
		{"stuck", core.FirstJob{Queued: 2, OldestQueued: now.Add(-10 * time.Minute)}, nil, Fail, "no Tentmaker runner came online"},
		{"api error", core.FirstJob{}, errors.New("github: HTTP 403"), Warn, "can't list its runners"},
	}
	for _, c := range cases {
		fs := firstJob(t, c.fj, c.err, "")
		if len(fs) != 1 || fs[0].Check != "first-job" || fs[0].Status != c.status || !strings.Contains(fs[0].What, c.want) {
			t.Fatalf("%s: %+v", c.name, fs)
		}
		if c.status != OK && fs[0].Fix == "" {
			t.Fatalf("%s: a problem with no fix", c.name)
		}
	}
	if fs := firstJob(t, core.FirstJob{}, nil, ""); !strings.Contains(fs[0].Fix, "tentmaker migrate --pr") {
		t.Fatalf("fix: %q", fs[0].Fix)
	}
	if fs := firstJob(t, core.FirstJob{Queued: 2, OldestQueued: now.Add(-10 * time.Minute)}, nil, ""); !strings.Contains(fs[0].Fix, "tentmaker logs") {
		t.Fatalf("stuck fix: %q", fs[0].Fix)
	}
	if fs := firstJob(t, core.FirstJob{}, nil, "checks"); len(fs) != 0 {
		t.Fatalf("checks mode has no runners: %+v", fs)
	}
	if fs := checkFirstJob(context.Background(), DoctorInput{SkipForges: "not root"}); fs != nil {
		t.Fatal("ran without the token")
	}
}

func TestOpenPRServedRepoOnly(t *testing.T) {
	f := &firstJobForge{Forge: fakeforge.New("github", nil)}
	d := &Daemon{cfg: config.Config{Repos: []config.Repo{{Name: "o/r"}}}, forges: Forges{Forges: map[string]core.Forge{"github": f}}}
	res, err := d.OpenPR(context.Background(), core.PullRequest{Repo: "O/R", Head: "tentmaker/migrate"})
	if err != nil || res.Number != 9 || len(f.prs) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := d.OpenPR(context.Background(), core.PullRequest{Repo: "o/other", Head: "tentmaker/migrate"}); err == nil || !strings.Contains(err.Error(), "does not serve") {
		t.Fatalf("unserved repo: %v", err)
	}
	d.forges = Forges{}
	if _, err := d.OpenPR(context.Background(), core.PullRequest{Repo: "o/r", Head: "tentmaker/migrate"}); err == nil || !strings.Contains(err.Error(), "connect github") {
		t.Fatalf("no token: %v", err)
	}
}
