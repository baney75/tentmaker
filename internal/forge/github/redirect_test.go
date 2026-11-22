package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Review r1 partB sol-F2: Go copies Authorization to any port of the same
// host and to subdomains on a redirect.
func TestRedirectToOtherOriginKeepsToken(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer origin.Close()
	c := New(Options{
		BaseURL: origin.URL,
		Token:   func(context.Context) (string, error) { return "tok-canary", nil },
		Now:     func() time.Time { return t0 },
	})
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.CheckRepo(context.Background(), "acme/app")
	if leaked {
		t.Fatal("the token went to another origin")
	}
}
