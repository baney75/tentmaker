package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

// rateLimitedFirstJob is a forge whose FirstJob is rate limited.
type rateLimitedFirstJob struct {
	*fakeforge.Forge
	err error
}

func (f rateLimitedFirstJob) FirstJob(ctx context.Context, repo string) (core.FirstJob, error) {
	return core.FirstJob{}, f.err
}

// A rate limit while doctor reads the repo is reported as one, with the
// reset and retry_after_s, never as a token that can't read the repo or
// lacks a permission.
func TestDoctorRateLimitIsNotAPermission(t *testing.T) {
	now := time.Date(2026, 11, 23, 5, 6, 0, 0, time.UTC)
	cfg := Resolve(config.Default(16<<30, 8), Paths{State: t.TempDir(), Cache: t.TempDir()})
	cfg.Repos = []config.Repo{{Name: "o/r"}}
	rl := fmt.Errorf("github: GET /repos/o/r: HTTP 403: API rate limit exceeded: %w",
		&core.RateLimitError{Forge: "github", Until: now.Add(12 * time.Minute)})
	ff := fakeforge.New("github", nil)
	ff.SetRepo(core.RepoInfo{Repo: "o/r"})
	ff.SetErr(fakeforge.MCheckRepo, "o/r", rl)
	rep := Doctor(context.Background(), DoctorInput{
		Version: "1.0.0", ConfigPath: "/etc/tentmaker/config.toml", ConfigExists: true, Config: cfg,
		Forges: Forges{Forges: map[string]core.Forge{"github": rateLimitedFirstJob{ff, rl}}, Tokens: []string{"github"}},
		Now:    func() time.Time { return now },
	})
	want := "o/r: GitHub rate limit reached for this token; it resets at 05:18 UTC (in 12 min)"
	seen := map[string]Finding{}
	for _, f := range rep.Checks {
		if f.Check == "forge" || f.Check == "first-job" {
			seen[f.Check] = f
		}
	}
	for check, status := range map[string]string{"forge": Fail, "first-job": Warn} {
		f, ok := seen[check]
		if !ok {
			t.Fatalf("no %s finding in %+v", check, rep.Checks)
		}
		if f.Status != status || f.What != want || f.RetryAfterS != 720 {
			t.Errorf("%s: %+v", check, f)
		}
		if strings.Contains(f.Fix, "permission") || strings.Contains(f.Fix, "Administration") || strings.Contains(f.Fix, "connect") ||
			!strings.Contains(f.Fix, "wait until 05:18 UTC, then run tentmaker doctor again") {
			t.Errorf("%s fix %q", check, f.Fix)
		}
	}
}
