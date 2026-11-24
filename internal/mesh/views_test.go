package mesh

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// viewsNode serves a fake view on the peer API. Its self is 100.64.0.1,
// alice-mac.example-tailnet.ts.net.
func viewsNode() (*Node, *int) {
	ts := newFakeTS()
	ts.self.DNSName = "alice-mac.example-tailnet.ts.net"
	served := 0
	views := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		_, _ = w.Write([]byte("view " + r.URL.Path))
	})
	return New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Views: views}), &served
}

func getView(n *Node, path, remote, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = remote
	req.Host = host
	rec := httptest.NewRecorder()
	n.handler().ServeHTTP(rec, req)
	return rec
}

// The same tailnet user reaches /metrics and the status page by this
// device's tailnet IP or MagicDNS name, with no pool header (issue #45).
func TestViewsServedToTheSameUser(t *testing.T) {
	n, served := viewsNode()
	for _, c := range []struct{ path, host string }{
		{"/metrics", "100.64.0.1:7433"},
		{"/", "alice-mac.example-tailnet.ts.net:7433"},
		{"/status", "alice-mac:7433"},
		{"/", "ALICE-MAC.example-tailnet.ts.net."},
	} {
		if rec := getView(n, c.path, "100.64.0.2:4000", c.host); rec.Code != 200 || rec.Body.String() != "view "+c.path {
			t.Errorf("GET %s with Host %s: %d %q", c.path, c.host, rec.Code, rec.Body)
		}
	}
	if *served != 4 {
		t.Fatalf("served %d views, want 4", *served)
	}
}

// Every caller the peer API refuses is refused the views too, before they
// run: another tailnet user, a tagged device, this device itself (a local
// process such as a CI job), and a caller WhoIs can't name.
func TestViewsRefuseWhomThePeerAPIRefuses(t *testing.T) {
	n, served := viewsNode()
	for _, remote := range []string{"100.64.0.5:4000", "100.64.0.6:4000", "127.0.0.1:4000", "100.64.0.1:4000", "100.64.0.9:4000"} {
		for _, path := range []string{"/metrics", "/", "/status"} {
			if rec := getView(n, path, remote, "100.64.0.1:7433"); rec.Code != http.StatusForbidden {
				t.Errorf("GET %s from %s: %d, want 403", path, remote, rec.Code)
			}
			// The rest of the peer API refuses the same caller.
			if rec := do(t, n.handler(), "GET", "/v1/status", remote, nil, "home"); rec.Code != http.StatusForbidden {
				t.Errorf("GET /v1/status from %s: %d, want 403", remote, rec.Code)
			}
		}
	}
	if *served != 0 {
		t.Fatalf("a refused caller got %d views", *served)
	}
}

// A page that rebinds its own name to this device's tailnet address sends
// its own name as Host: refused, so it can't read the views.
func TestViewsRefuseAForeignHost(t *testing.T) {
	n, served := viewsNode()
	for _, host := range []string{"evil.example", "evil.example:7433", "10.0.0.5:7433", "alice-mac.evil.example", "", "localhost:7433"} {
		if rec := getView(n, "/metrics", "100.64.0.2:4000", host); rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403", host, rec.Code)
		}
	}
	if rec := getView(n, "/metrics", "100.64.0.2:4000", "100.64.0.1"); rec.Code != 200 {
		t.Fatalf("own IP refused: %d", rec.Code)
	}
	if *served != 1 {
		t.Fatalf("served %d views, want 1", *served)
	}
}

// The views are read-only, and a node with none answers 404.
func TestViewsMethodsAndAbsence(t *testing.T) {
	n, _ := viewsNode()
	req := httptest.NewRequest("POST", "/metrics", nil)
	req.RemoteAddr, req.Host = "100.64.0.2:4000", "100.64.0.1"
	rec := httptest.NewRecorder()
	n.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics: %d", rec.Code)
	}
	bare := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS()})
	if rec := getView(bare, "/metrics", "100.64.0.2:4000", "100.64.0.1"); rec.Code != http.StatusNotFound {
		t.Errorf("no views: %d, want 404", rec.Code)
	}
}

// Viewers reach the views and nothing else of the peer API (review
// second-F5): a tagged scraper such as tag:monitoring is let in without
// being admitted to work, claim or release. An entry that names no caller
// admits none, and this device itself stays refused.
func TestViewersSeeTheViewsAlone(t *testing.T) {
	ts := newFakeTS()
	ts.self.DNSName = "alice-mac.example-tailnet.ts.net"
	served := 0
	views := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++ })
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Views: views,
		Viewers: []string{"TAG:CI", "bob-pc", "alice-mac", "nSELF", "tag:"}})
	for _, remote := range []string{"100.64.0.6:4000", "100.64.0.5:4000"} {
		for _, path := range []string{"/metrics", "/", "/status"} {
			if rec := getView(n, path, remote, "100.64.0.1:7433"); rec.Code != 200 {
				t.Errorf("viewer %s GET %s: %d, want 200", remote, path, rec.Code)
			}
		}
		for _, c := range []struct{ method, path string }{
			{"GET", "/v1/status"}, {"GET", "/v1/runs"}, {"GET", "/v1/runs/log?id=x"}, {"POST", "/v1/work"},
			{"POST", "/v1/claim"}, {"POST", "/v1/release"}, {"POST", "/v1/rerun"}, {"GET", "/v1/archive/x"},
		} {
			if rec := do(t, n.handler(), c.method, c.path, remote, nil, "home"); rec.Code != http.StatusForbidden {
				t.Errorf("viewer %s %s %s: %d, want 403", remote, c.method, c.path, rec.Code)
			}
		}
	}
	for _, remote := range []string{"127.0.0.1:4000", "100.64.0.1:4000", "100.64.0.9:4000"} {
		if rec := getView(n, "/metrics", remote, "100.64.0.1:7433"); rec.Code != http.StatusForbidden {
			t.Errorf("GET /metrics from %s: %d, want 403", remote, rec.Code)
		}
	}
	if served != 6 {
		t.Fatalf("served %d views, want 6", served)
	}
	other := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Views: views, Viewers: []string{"tag:monitoring"}})
	if rec := getView(other, "/metrics", "100.64.0.6:4000", "100.64.0.1:7433"); rec.Code != http.StatusForbidden {
		t.Errorf("tag:ci under a tag:monitoring allow-list: %d, want 403", rec.Code)
	}
}
