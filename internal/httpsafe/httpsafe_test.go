package httpsafe

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// A request with a credential follows a same-origin redirect, refuses a
// cross-origin one, and a request without credentials follows both.
func TestGuardRefusesCrossOriginWithCredential(t *testing.T) {
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, k := range CredentialHeaders {
			if r.Header.Get(k) != "" {
				leaked = append(leaked, k)
			}
		}
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/ok", http.StatusFound)
		}
	}))
	defer origin.Close()
	c := Guard(origin.Client())
	for _, h := range CredentialHeaders {
		req, _ := http.NewRequest(http.MethodGet, origin.URL+"/away", nil)
		req.Header.Set(h, "secret")
		_, err := c.Do(req)
		if !errors.Is(err, ErrCrossOrigin) {
			t.Errorf("%s: want ErrCrossOrigin, got %v", h, err)
		}
		req, _ = http.NewRequest(http.MethodGet, origin.URL+"/here", nil)
		req.Header.Set(h, "secret")
		if resp, err := c.Do(req); err != nil || resp.StatusCode != 200 {
			t.Errorf("%s: same-origin redirect: %v", h, err)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("other origin saw %v", leaked)
	}
	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/away", nil)
	if resp, err := c.Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("no credential: %v", err)
	}
}

func TestOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"https://Git.Example.com/a", "https://git.example.com:443/b", true},
		{"http://h/a", "http://h:80/b", true},
		{"https://h/a", "http://h/a", false},
		{"https://h/a", "https://h:8443/a", false},
		{"https://h/a", "https://sub.h/a", false},
		{"https://h/a", "https://h.evil/a", false},
	} {
		a, _ := url.Parse(tc.a)
		b, _ := url.Parse(tc.b)
		if got := SameOrigin(a, b); got != tc.same {
			t.Errorf("SameOrigin(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
}
