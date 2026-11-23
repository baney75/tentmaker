package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAsRateLimited(t *testing.T) {
	now := time.Date(2026, 11, 23, 5, 6, 0, 0, time.UTC)
	answer := errors.New("github: GET /user: HTTP 403: API rate limit exceeded")
	for _, tc := range []struct {
		err   error
		msg   string
		retry int
	}{
		{&RateLimitError{Forge: "github", Until: now.Add(12 * time.Minute), Err: answer},
			"GitHub rate limit reached for this token; it resets at 05:18 UTC (in 12 min)", 720},
		// Rounded up to the minute: 05:18:20 shows as 05:19, never early.
		{&RateLimitError{Forge: "github", Until: now.Add(12*time.Minute + 20*time.Second)},
			"GitHub rate limit reached for this token; it resets at 05:19 UTC (in 13 min)", 740},
		// Under 5 minutes, to the second.
		{fmt.Errorf("poll: %w", &RateLimitError{Forge: "gitlab", Until: now.Add(90 * time.Second)}),
			"GitLab rate limit reached for this token; it resets at 05:07:30 UTC (in 2 min)", 90},
		{&RateLimitError{Forge: "forgejo", Until: now.Add(2*time.Hour + 5*time.Minute)},
			"Forgejo rate limit reached for this token; it resets at 07:11 UTC (in 2 h 5 min)", 7500},
		// A reset already past still waits a second.
		{&RateLimitError{Forge: "github", Until: now.Add(-time.Minute)},
			"GitHub rate limit reached for this token; it resets at 05:06:01 UTC (in 1 s)", 1},
		// Only the sentinel: the forge didn't say, so a minute.
		{fmt.Errorf("x: %w", ErrRateLimited),
			"The forge's rate limit reached for this token; it did not say when it resets, so wait until 05:07:00 UTC (1 min)", 60},
		// Past midnight UTC: the date too.
		{&RateLimitError{Forge: "github", Until: time.Date(2026, 11, 24, 0, 30, 0, 0, time.UTC)},
			"GitHub rate limit reached for this token; it resets at 2026-11-24 00:30 UTC (in 19 h 24 min)", 69840},
	} {
		rl, ok := AsRateLimited(tc.err, now, "run it again")
		if !ok || rl.Message != tc.msg || rl.RetryAfterS() != tc.retry {
			t.Errorf("%v:\n got %v %q %d\nwant %q %d", tc.err, ok, rl.Message, rl.RetryAfterS(), tc.msg, tc.retry)
		}
		if !strings.Contains(rl.Fix, "wait until") || !strings.HasPrefix(rl.Fix, "the token is fine") || strings.Contains(rl.Fix, "new token") {
			t.Errorf("fix %q", rl.Fix)
		}
	}
	if _, ok := AsRateLimited(errors.New("github: GET /user: HTTP 403"), now, ""); ok {
		t.Error("a plain 403 read as a rate limit")
	}
	if _, ok := AsRateLimited(nil, now, ""); ok {
		t.Error("nil read as a rate limit")
	}
}

func TestRateLimitErrorChain(t *testing.T) {
	answer := errors.New("HTTP 403")
	err := error(&RateLimitError{Forge: "github", Until: time.Unix(0, 0), Err: answer})
	if !errors.Is(err, ErrRateLimited) || !errors.Is(err, answer) {
		t.Errorf("chain %v", err)
	}
	if got := err.Error(); got != "HTTP 403; rate limited until 1970-01-01 00:00:00 UTC" {
		t.Errorf("Error() %q", got)
	}
}
