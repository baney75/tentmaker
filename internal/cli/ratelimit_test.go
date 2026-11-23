package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rateCase is one forge answer to the token check and what the user must
// read: the message, the fix and retry_after_s (0: none).
type rateCase struct {
	name    string
	kind    string
	handler func(w http.ResponseWriter, r *http.Request)
	msg     []string // all in the error
	retry   [2]int   // retry_after_s within [lo, hi]; {0, 0}: absent
	permFix bool     // the fix is the make-a-token help
}

func rateCases(now time.Time) []rateCase {
	reset := now.Add(12 * time.Minute).Unix()
	// Shown to the minute, rounded up so waiting until it is never early.
	resetAt := time.Unix(reset, 0).UTC()
	if resetAt.Second() != 0 {
		resetAt = resetAt.Truncate(time.Minute).Add(time.Minute)
	}
	at := resetAt.Format("15:04 UTC")
	return []rateCase{
		{
			name: "github primary limit",
			kind: "github",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Limit", "5000")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"message":"API rate limit exceeded for user ID 1."}`)
			},
			msg:   []string{"GitHub rate limit reached for this token; it resets at " + at + " (in 12 min)"},
			retry: [2]int{700, 720},
		},
		{
			name: "github secondary limit with retry-after",
			kind: "github",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "90")
				w.Header().Set("X-RateLimit-Remaining", "4000")
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
			},
			msg:   []string{"GitHub rate limit reached for this token", "(in 2 min)"},
			retry: [2]int{85, 90},
		},
		{
			name: "github secondary limit by message only",
			kind: "github",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
			},
			msg:   []string{"GitHub rate limit reached for this token", "(in 1 min)"},
			retry: [2]int{55, 60},
		},
		{
			name: "github 429",
			kind: "github",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			msg:   []string{"GitHub rate limit reached for this token", "(in 30 s)"},
			retry: [2]int{25, 30},
		},
		{
			// Actions' GITHUB_TOKEN: /user is a normal 403 for an app
			// installation, and the installation call is the one rate
			// limited (the CI failure in issue #23).
			name: "github installation token rate limited",
			kind: "github",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
					return
				}
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 9."}`)
			},
			msg:   []string{"GitHub rate limit reached for this token; it resets at " + at + " (in 12 min)"},
			retry: [2]int{700, 720},
		},
		{
			name: "gitlab 429",
			kind: "gitlab",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("RateLimit-Remaining", "0")
				w.Header().Set("RateLimit-Reset", strconv.FormatInt(reset, 10))
				w.WriteHeader(http.StatusTooManyRequests)
			},
			msg:   []string{"GitLab rate limit reached for this token; it resets at " + at + " (in 12 min)"},
			retry: [2]int{700, 720},
		},
		{
			name: "forgejo 429",
			kind: "forgejo",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "45")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			msg:   []string{"Forgejo rate limit reached for this token", "(in 45 s)"},
			retry: [2]int{40, 45},
		},
		{
			name: "github permission refusal",
			kind: "github",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "4999")
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"message":"Resource not accessible by personal access token"}`)
			},
			msg:     []string{"the github token did not work: the forge refused the token (403); check its permissions"},
			permFix: true,
		},
	}
}

// A rate limit is told apart from a permission refusal in connect --json:
// its own message with the reset, a fix that says to wait (never to make a
// new token), retry_after_s, and nothing stored.
func TestTokenCheckRateLimit(t *testing.T) {
	for _, tc := range rateCases(time.Now()) {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()
			f := newFixture(t)
			f.setUp()
			f.env.VerifyToken = func(ctx context.Context, kind, base, tok string) (string, error) {
				return verifyToken(ctx, srv.Client(), kind, srv.URL, tok)
			}
			args := []string{"connect", tc.kind, "--url", srv.URL, "--token-stdin", "--json"}
			if code := f.run(canary, args...); code != ExitFailed {
				t.Fatalf("exit %d %s", code, f.stdout)
			}
			out := f.jsonObject(args)
			msg, _ := out["error"].(string)
			fix, _ := out["fix"].(string)
			if out["ok"] != false {
				t.Errorf("ok %v", out["ok"])
			}
			for _, want := range tc.msg {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q\nwant %q", msg, want)
				}
			}
			ra, has := out["retry_after_s"].(float64)
			if tc.permFix {
				if has {
					t.Errorf("a permission refusal has retry_after_s %v", ra)
				}
				if !strings.Contains(fix, "Make a") {
					t.Errorf("fix %q", fix)
				}
			} else {
				if !has || int(ra) < tc.retry[0] || int(ra) > tc.retry[1] {
					t.Errorf("retry_after_s %v, want %d..%d", out["retry_after_s"], tc.retry[0], tc.retry[1])
				}
				if strings.Contains(msg, "permission") || strings.Contains(fix, "Make a") || strings.Contains(fix, "personal-access-tokens") {
					t.Errorf("a rate limit reads as a token problem:\nerror %q\nfix %q", msg, fix)
				}
				if !strings.Contains(fix, "wait until") || !strings.Contains(fix, "not shared") {
					t.Errorf("fix %q", fix)
				}
			}
			f.store.mu.Lock()
			for name, v := range f.store.m {
				if string(v) == canary {
					t.Errorf("a refused token was stored as %s", name)
				}
			}
			f.store.mu.Unlock()
		})
	}
}

// Setup stops at the token step on a rate limit, before it writes the
// config, and says when to run it again; human output carries the fix.
func TestSetupRateLimitedWritesNothing(t *testing.T) {
	reset := time.Now().Add(12 * time.Minute).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()
	f := newFixture(t)
	f.env.VerifyToken = func(ctx context.Context, kind, base, tok string) (string, error) {
		return verifyToken(ctx, srv.Client(), kind, srv.URL, tok)
	}
	if code := f.run(canary, "setup", "--yes", "--no-service", "--no-mesh", "--repo", "o/r", "--token-stdin"); code != ExitFailed {
		t.Fatalf("exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
	errOut := f.stderr.String()
	if !strings.Contains(errOut, "GitHub rate limit reached for this token; it resets at") || !strings.Contains(errOut, "fix: the token is fine; wait until") {
		t.Errorf("stderr:\n%s", errOut)
	}
	if _, err := os.Stat(f.env.ConfigPath); !os.IsNotExist(err) {
		t.Errorf("setup wrote the config despite the rate limit: %v", err)
	}
	if f.execInst != 0 {
		t.Errorf("setup installed executors despite the rate limit")
	}
}
