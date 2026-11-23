package forgejo

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// A 429 that asks for an hour fails the call at once with the reset; it
// never sleeps the hour through (that held the broker's tick, issue #23).
func TestRateLimitLongWaitFailsFast(t *testing.T) {
	h := newHarness(t, Forgejo)
	h.f.on("GET", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := h.c.Runner(context.Background(), "acme/app", 5)
	var rl *core.RateLimitError
	if !errors.Is(err, core.ErrRateLimited) || !errors.As(err, &rl) || statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("want a 429 matching core.ErrRateLimited, got %v", err)
	}
	if rl.Forge != "forgejo" || !rl.Until.Equal(t0.Add(time.Hour)) {
		t.Fatalf("rate limit %+v", rl)
	}
	if len(h.sleeps) != 0 || h.f.count("GET", "/repos/acme/app/actions/runners/5") != 1 {
		t.Fatalf("slept %v over %d tries", h.sleeps, h.f.count("GET", "/repos/acme/app/actions/runners/5"))
	}
	h.noLeak(t, err)
}

// A short 429 is slept through; one still there after every try is a rate
// limit, not a bare 429.
func TestRateLimitShortWaitsExhausted(t *testing.T) {
	h := newHarness(t, Forgejo)
	h.f.on("GET", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := h.c.Runner(context.Background(), "acme/app", 5)
	if !errors.Is(err, core.ErrRateLimited) {
		t.Fatalf("want core.ErrRateLimited, got %v", err)
	}
	if len(h.sleeps) != Tries-1 {
		t.Fatalf("slept %v", h.sleeps)
	}
}
