package gitlab

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// A 429 on every try is a rate limit with GitLab's reset, never the bare
// HTTP 429 that reads like a token problem.
func TestRateLimited429(t *testing.T) {
	f, c := newFake(t)
	reset := time.Now().Add(10 * time.Minute).Unix()
	f.on("GET", encoded+"/repository/files/a/raw", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit-Remaining", "0")
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.FileAt(context.Background(), repo, sha, "a")
	var rl *core.RateLimitError
	if !errors.Is(err, core.ErrRateLimited) || !errors.As(err, &rl) {
		t.Fatalf("want core.ErrRateLimited, got %v", err)
	}
	if rl.Forge != "gitlab" || rl.Until.Unix() != reset {
		t.Fatalf("rate limit %+v", rl)
	}
	if got := f.count("GET", encoded+"/repository/files/a/raw"); got != tries {
		t.Fatalf("%d tries, want %d", got, tries)
	}
}
