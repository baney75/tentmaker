package control

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
)

// viewsHandler is fakeHandler with metrics and a status page.
type viewsHandler struct {
	fakeHandler
	served atomic.Int32
}

func (v *viewsHandler) Views() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.served.Add(1)
		_, _ = w.Write([]byte("view " + r.URL.Path))
	})
}

// serveViews serves the control API over viewsHandler with ident naming
// every caller; it returns the base URL.
func serveViews(t *testing.T, ident Identify) (*viewsHandler, string) {
	t.Helper()
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &viewsHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(h, "", ident, nil).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return h, "http://" + ln.Addr().String()
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var viewPaths = []string{"/metrics", "/", "/status"}

// A local reader gets the metrics and the status page with no token, like
// status (issue #45).
func TestViewsServedToReaders(t *testing.T) {
	h, base := serveViews(t, func(net.Conn) (Caller, error) { return reader, nil })
	for _, p := range viewPaths {
		if code, body := get(t, base+p); code != 200 || body != "view "+p {
			t.Errorf("GET %s: %d %q", p, code, body)
		}
	}
	if code, _ := get(t, base+"/nope"); code != http.StatusNotFound {
		t.Errorf("GET /nope: %d, want 404", code)
	}
	if n := h.served.Load(); n != 3 {
		t.Fatalf("served %d views, want 3", n)
	}
}

// Every caller the control API refuses is refused the views, before they
// run: a CI job, a caller the server can't name, and every caller when it
// has no way to name them.
func TestViewsRefuseWhomTheControlAPIRefuses(t *testing.T) {
	for name, ident := range map[string]Identify{
		"a CI job":           func(net.Conn) (Caller, error) { return Caller{Runner: true, Who: "uid 900"}, nil },
		"an unnamed peer":    func(net.Conn) (Caller, error) { return Caller{}, io.EOF },
		"no identify at all": nil,
	} {
		h, base := serveViews(t, ident)
		for _, p := range append([]string{"/v1/status"}, viewPaths...) {
			if code, _ := get(t, base+p); code != http.StatusForbidden {
				t.Errorf("%s: GET %s: %d, want 403", name, p, code)
			}
		}
		if n := h.served.Load(); n != 0 {
			t.Errorf("%s got %d views", name, n)
		}
	}
}

// The views answer only on loopback names, like the rest of the API.
func TestViewsRejectForeignHost(t *testing.T) {
	h, base := serveViews(t, func(net.Conn) (Caller, error) { return reader, nil })
	req, _ := http.NewRequest("GET", base+"/metrics", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || h.served.Load() != 0 {
		t.Fatalf("foreign Host: %d, served %d", resp.StatusCode, h.served.Load())
	}
}
