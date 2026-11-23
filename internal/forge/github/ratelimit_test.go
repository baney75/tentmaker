package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// A 403 that is a short secondary limit on every try is still a rate
// limit when the tries run out, not the bare 403 a permission refusal is.
func TestShortRateLimitsExhaustedStayRateLimited(t *testing.T) {
	h := newHarness(t)
	h.f.on("GET", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
	})
	_, err := h.c.Runner(context.Background(), "acme/app", 5)
	var rl *core.RateLimitError
	if !errors.Is(err, core.ErrRateLimited) || !errors.As(err, &rl) || statusOf(err) != 403 {
		t.Fatalf("want a 403 matching core.ErrRateLimited, got %v", err)
	}
	if rl.Forge != "github" || !rl.Until.Equal(h.c.now().Add(2*time.Second)) {
		t.Fatalf("rate limit %+v", rl)
	}
}

// A permission 403 is final and never reads as a rate limit.
func TestPermission403IsNotRateLimited(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app/actions/runners/5", 403, map[string]any{"message": "Must have admin rights to Repository."})
	_, err := h.c.Runner(context.Background(), "acme/app", 5)
	if err == nil || errors.Is(err, core.ErrRateLimited) || statusOf(err) != 403 {
		t.Fatalf("want a plain 403, got %v", err)
	}
	if h.f.count("GET", "/repos/acme/app/actions/runners/5") != 1 {
		t.Fatal("retried a permission refusal")
	}
}

// A rate limit while CheckRepo reads the token's permissions fails the
// check; it is never reported as a missing permission.
func TestCheckRepoRateLimitIsNotAProblem(t *testing.T) {
	for _, sub := range []string{
		"/repos/acme/app/actions/permissions/fork-pr-workflows-private-repos",
		"/repos/acme/app/actions/runners?per_page=1",
	} {
		t.Run(sub, func(t *testing.T) {
			h := newHarness(t)
			h.f.json("GET", "/repos/acme/app", 200, map[string]any{"private": true, "visibility": "private", "default_branch": "main", "permissions": map[string]any{"admin": true}})
			h.f.json("GET", "/repos/acme/app/actions/permissions/fork-pr-workflows-private-repos", 200, map[string]any{"run_workflows_from_fork_pull_requests": false})
			h.f.json("GET", "/repos/acme/app/actions/runs?per_page=1", 200, map[string]any{})
			h.f.json("GET", "/repos/acme/app/actions/runners?per_page=1", 200, map[string]any{})
			h.f.on("GET", sub, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(t0.Add(10*time.Minute).Unix(), 10))
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			})
			info, err := h.c.CheckRepo(context.Background(), "acme/app")
			if !errors.Is(err, core.ErrRateLimited) {
				t.Fatalf("want core.ErrRateLimited, got %v (problems %q)", err, info.Problems)
			}
		})
	}
}

// Rerun treats a 403 as "the run is still going"; a rate limited 403 must
// not pass for that.
func TestSendRateLimited403IsNotExpected(t *testing.T) {
	h := newHarness(t)
	h.f.on("POST", "/x", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(t0.Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	})
	if _, err := h.c.send(context.Background(), "POST", "/x", nil, http.StatusForbidden); !errors.Is(err, core.ErrRateLimited) {
		t.Fatalf("want core.ErrRateLimited, got %v", err)
	}
}

func TestRateLimitWait(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		status int
		h      http.Header
		body   string
		wait   time.Duration
		ok     bool
	}{
		{"primary", 403, h("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", "1000720"), `{"message":"API rate limit exceeded"}`, 720 * time.Second, true},
		{"secondary retry-after", 403, h("Retry-After", "90"), `{"message":"secondary rate limit"}`, 90 * time.Second, true},
		{"secondary message only", 403, h(), `{"message":"You have exceeded a secondary rate limit"}`, time.Minute, true},
		{"429 no headers", 429, h(), ``, time.Minute, true},
		{"permission", 403, h("X-RateLimit-Remaining", "4999"), `{"message":"Resource not accessible by personal access token"}`, 0, false},
		{"404", 404, h("Retry-After", "5"), ``, 0, false},
	} {
		wait, ok := RateLimitWait(tc.status, tc.h, []byte(tc.body), now)
		if wait != tc.wait || ok != tc.ok {
			t.Errorf("%s: %v %v, want %v %v", tc.name, wait, ok, tc.wait, tc.ok)
		}
	}
}
