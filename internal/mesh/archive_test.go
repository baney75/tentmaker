package mesh

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

func TestArchiveThroughMesh(t *testing.T) {
	ref := strings.Repeat("ab", 32)
	broken := strings.Repeat("cd", 32)
	failing := strings.Repeat("ef", 32)
	tarball := []byte("\x1f\x8b the source archive")
	fb := &fakeBroker{archives: map[string]fakeArchive{
		ref:     {device: "a-worker", data: tarball},
		broken:  {device: "a-worker", data: []byte("half"), err: errors.New("forge hung up")},
		failing: {device: "a-worker", err: errors.New("forge said 500")},
	}}
	ms := newCluster(t,
		nodeSpec{device: "a-worker", pool: "home"},
		nodeSpec{device: "b-broker", pool: "home", broker: fb},
	)
	srcs := ms[0].node.Brokers(context.Background())
	if len(srcs) != 1 {
		t.Fatalf("brokers: %v", names(srcs))
	}
	as, ok := srcs[0].(core.ArchiveSource)
	if !ok {
		t.Fatal("the remote broker is not an ArchiveSource")
	}
	var buf bytes.Buffer
	if err := as.FetchArchive(context.Background(), ref, "a-worker", &buf); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), tarball) {
		t.Fatalf("got %q", buf.Bytes())
	}
	// Asking as another device is refused before the broker sees it.
	buf.Reset()
	if err := as.FetchArchive(context.Background(), ref, "mallory", &buf); err == nil || buf.Len() != 0 {
		t.Errorf("fetch as mallory: err=%v bytes=%d", err, buf.Len())
	}
	// An unknown ref, a malformed ref: not found.
	for _, c := range []struct{ ref, device string }{{strings.Repeat("00", 32), "a-worker"}, {"../v1/work", "a-worker"}} {
		buf.Reset()
		if err := as.FetchArchive(context.Background(), c.ref, c.device, &buf); !errors.Is(err, core.ErrNotFound) || buf.Len() != 0 {
			t.Errorf("fetch %q as %s: err=%v bytes=%d", c.ref, c.device, err, buf.Len())
		}
	}
	// A broker error before any byte is an error, not an empty archive.
	buf.Reset()
	if err := as.FetchArchive(context.Background(), failing, "a-worker", &buf); err == nil || errors.Is(err, core.ErrNotFound) {
		t.Errorf("failing broker: %v", err)
	}
	// A stream that breaks midway is an error, never a short archive.
	buf.Reset()
	if err := as.FetchArchive(context.Background(), broken, "a-worker", &buf); err == nil {
		t.Errorf("a broken stream looked whole (%d bytes)", buf.Len())
	}
}

func TestArchiveEndpointAuth(t *testing.T) {
	ref := strings.Repeat("ab", 32)
	fb := &fakeBroker{archives: map[string]fakeArchive{ref: {device: "w", data: []byte("tar")}}}
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: fb})
	knowPeer(n, "n2", "100.64.0.2", "w")
	get := func(remote, pool, device string) int {
		req, _ := http.NewRequest("GET", "/v1/archive/"+ref, nil)
		req.RemoteAddr = remote
		if pool != "" {
			req.Header.Set(poolHeader, pool)
		}
		if device != "" {
			req.Header.Set(deviceHeader, device)
		}
		rec := httptest.NewRecorder()
		n.handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := get("100.64.0.2:1", "home", "w"); c != 200 {
		t.Errorf("granted device: %d", c)
	}
	if c := get("100.64.0.5:1", "home", "w"); c != http.StatusForbidden {
		t.Errorf("another user: %d", c)
	}
	if c := get("100.64.0.2:1", "", "w"); c == 200 {
		t.Errorf("no pool header: %d", c)
	}
	if c := get("100.64.0.2:1", "home", ""); c != 404 {
		t.Errorf("no device header: %d", c)
	}
	// A broker that can't serve archives answers 404.
	n2 := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: brokerOnly{fb}})
	req, _ := http.NewRequest("GET", "/v1/archive/"+ref, nil)
	req.RemoteAddr = "100.64.0.2:1"
	req.Header.Set(poolHeader, "home")
	req.Header.Set(deviceHeader, "w")
	rec := httptest.NewRecorder()
	n2.handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("broker without archives: %d", rec.Code)
	}
	if c := do(t, n.handler(), "POST", "/v1/archive/"+ref, "100.64.0.2:1", nil, "home").Code; c != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", c)
	}
}

// brokerOnly hides FetchArchive.
type brokerOnly struct{ b *fakeBroker }

func (b brokerOnly) Name() string { return b.b.Name() }
func (b brokerOnly) Work(ctx context.Context, r core.WorkRequest) (core.WorkResponse, error) {
	return b.b.Work(ctx, r)
}
