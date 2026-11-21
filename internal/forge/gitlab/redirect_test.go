package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Review r1 partB sol-F2: Go copies PRIVATE-TOKEN to every redirect target.
func TestRedirectToOtherOriginKeepsToken(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "" || r.Header.Get("Authorization") != "" {
			leaked = true
		}
		w.Write([]byte(`[]`))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer origin.Close()
	c := New(Options{
		BaseURL: origin.URL + "/api/v4/",
		Token:   func(context.Context) (string, error) { return token, nil },
	})
	c.sleep = func(context.Context, time.Duration) error { return nil }
	c.Statuses(context.Background(), "acme/app", "0123456789abcdef0123456789abcdef01234567")
	if leaked {
		t.Fatal("the token went to another origin")
	}
}
