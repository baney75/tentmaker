package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A forge (or a proxy in front of it) that redirects to another origin must
// not receive the token there. Go copies PRIVATE-TOKEN on every redirect and
// Authorization to any port of the same host. Review r1 partB sol-F2.
func TestVerifyTokenRefusesCrossOriginRedirect(t *testing.T) {
	var seen []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization") + r.Header.Get("PRIVATE-TOKEN"); a != "" {
			seen = append(seen, r.URL.Path+" "+a)
		}
		w.Write([]byte(`{"login":"octo","username":"octo"}`))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer origin.Close()
	for _, kind := range []string{"github", "forgejo", "gitlab"} {
		_, err := verifyToken(context.Background(), origin.Client(), kind, origin.URL, canary)
		if err == nil {
			t.Errorf("%s: followed a cross-origin redirect", kind)
		} else if strings.Contains(err.Error(), canary) {
			t.Errorf("%s: error leaks the token: %v", kind, err)
		}
	}
	for _, s := range seen {
		if strings.Contains(s, canary) {
			t.Errorf("the other origin got the token: %s", s)
		}
	}
}
