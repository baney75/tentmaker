package mesh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

// fakeTS is a Tailscale whose answers the test sets. WhoIs maps a remote
// IP to an identity.
type fakeTS struct {
	mu       sync.Mutex
	self     Peer
	selfErr  error
	peers    []Peer
	peersErr error
	ids      map[string]Identity
	whoisErr error
	whois    int
}

func (f *fakeTS) Self(ctx context.Context) (Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.self, f.selfErr
}

func (f *fakeTS) Peers(ctx context.Context) ([]Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Peer(nil), f.peers...), f.peersErr
}

func (f *fakeTS) WhoIs(ctx context.Context, remoteAddr string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.whois++
	if f.whoisErr != nil {
		return Identity{}, f.whoisErr
	}
	ap, err := parseRemote(remoteAddr)
	if err != nil {
		return Identity{}, err
	}
	id, ok := f.ids[ap.Addr().String()]
	if !ok {
		return Identity{}, errors.New("no match for IP")
	}
	return id, nil
}

func (f *fakeTS) set(fn func(f *fakeTS)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

var (
	alice  = Identity{Node: "alice-linux.example-tailnet.ts.net", HostName: "alice-linux", UserID: 1001, LoginName: "alice@example.com"}
	bob    = Identity{Node: "bob-pc.example-tailnet.ts.net", HostName: "bob-pc", UserID: 1002, LoginName: "bob@example.com"}
	tagged = Identity{Node: "ci-box.example-tailnet.ts.net", HostName: "ci-box", UserID: 1003, LoginName: TaggedLogin, Tags: []string{"tag:ci"}}
)

func aliceSelf(ip string) Peer {
	return Peer{ID: "nSELF", HostName: "alice-mac", Online: true, UserID: 1001, LoginName: "alice@example.com",
		IPs: []netip.Addr{netip.MustParseAddr(ip)}}
}

func newFakeTS() *fakeTS {
	return &fakeTS{
		self: aliceSelf("100.64.0.1"),
		ids: map[string]Identity{
			"100.64.0.2": alice,
			"100.64.0.5": bob,
			"100.64.0.6": tagged,
			"127.0.0.1":  alice,
		},
	}
}

// syncBuf is a log sink safe for concurrent writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func debugLog() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// fakeBroker hands out one offer with a secret per request.
type fakeBroker struct {
	secret []byte
	mu     sync.Mutex
	reqs   []core.WorkRequest
	err    error
}

func (b *fakeBroker) Name() string { return "fake" }

func (b *fakeBroker) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	err := b.err
	b.mu.Unlock()
	if err != nil {
		return core.WorkResponse{}, err
	}
	return core.WorkResponse{
		Offers: []core.WorkOffer{{
			Job:      core.Job{Forge: "github", Repo: "alice/app", ID: 42, Name: "test"},
			Grant:    core.Grant{Forge: "github", Repo: "alice/app", RunnerID: 7, RunnerName: "tm-alice-mac-1", Secret: append([]byte(nil), b.secret...)},
			Platform: core.Platform{OS: "linux", Arch: "x64"},
		}},
		Stop: []string{"tm-old-runner"},
	}, nil
}

func do(t *testing.T, h http.Handler, method, path, remote string, body io.Reader, pool string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = remote
	if pool != "" {
		req.Header.Set(poolHeader, pool)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errText(rec *httptest.ResponseRecorder) string {
	var eb errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &eb)
	return eb.Error
}

func TestHelloSameUser(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Version: "1.2.3", Tailscale: newFakeTS(), Broker: &fakeBroker{},
		Platforms: []core.Platform{{OS: "macos", Arch: "arm64"}}})
	rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.2:40000", nil, "")
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var h helloResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	want := helloResponse{Device: "alice-mac", Pool: "home", Version: "1.2.3", Broker: true, Platforms: []core.Platform{{OS: "macos", Arch: "arm64"}}}
	if h.Device != want.Device || h.Pool != want.Pool || h.Version != want.Version || !h.Broker || len(h.Platforms) != 1 || h.Platforms[0] != want.Platforms[0] {
		t.Fatalf("hello: %+v", h)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
}

func TestOtherUserForbidden(t *testing.T) {
	log, buf := debugLog()
	ts := newFakeTS()
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Broker: &fakeBroker{secret: []byte("s")}, Log: log})
	for _, path := range []string{"/v1/hello", "/v1/status", "/v1/work", "/v1/claim", "/v1/nope"} {
		rec := do(t, n.handler(), "GET", path, "100.64.0.5:40000", nil, "home")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from another user: code %d", path, rec.Code)
		}
		if errText(rec) == "" {
			t.Errorf("%s: no JSON error", path)
		}
	}
	if !strings.Contains(buf.String(), "bob@example.com") || !strings.Contains(buf.String(), "100.64.0.5") {
		t.Errorf("refusal not logged with the caller:\n%s", buf)
	}
}

func TestTaggedForbiddenUnlessMember(t *testing.T) {
	ts := newFakeTS()
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts})
	if rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.6:1", nil, ""); rec.Code != 403 {
		t.Errorf("tagged device: code %d", rec.Code)
	}
	n = New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Members: []string{"Tagged-Devices"}})
	if rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.6:1", nil, ""); rec.Code != 200 {
		t.Errorf("tagged device listed in members: code %d", rec.Code)
	}
	// A tagged Self serves no one by "same user", even another tagged device.
	ts2 := newFakeTS()
	ts2.self = Peer{ID: "nSELF", UserID: 1003, LoginName: TaggedLogin, Tags: []string{"tag:ci"}, IPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}}
	n = New(Options{Device: "ci", Pool: "home", Tailscale: ts2})
	if rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.6:1", nil, ""); rec.Code != 403 {
		t.Errorf("tagged self, tagged caller: code %d", rec.Code)
	}
}

func TestMembersAllowed(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Members: []string{" BOB@example.com "}})
	if rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.5:1", nil, ""); rec.Code != 200 {
		t.Errorf("member: code %d", rec.Code)
	}
	if rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.2:1", nil, ""); rec.Code != 200 {
		t.Errorf("same user with members set: code %d", rec.Code)
	}
}

func TestWhoIsFailureForbidden(t *testing.T) {
	log, buf := debugLog()
	ts := newFakeTS()
	ts.whoisErr = errors.New("tailscaled is not answering")
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Log: log})
	rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.2:1", nil, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("whois failure: code %d", rec.Code)
	}
	if !strings.Contains(buf.String(), "whois failed") {
		t.Errorf("not logged:\n%s", buf)
	}
	// Unknown address: also refused.
	ts.set(func(f *fakeTS) { f.whoisErr = nil })
	if rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.99:1", nil, ""); rec.Code != 403 {
		t.Errorf("unknown caller: code %d", rec.Code)
	}
	// Self unreadable: refused too.
	ts2 := newFakeTS()
	ts2.selfErr = errors.New("stopped")
	n2 := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts2})
	if rec := do(t, n2.handler(), "GET", "/v1/hello", "100.64.0.2:1", nil, ""); rec.Code != 403 {
		t.Errorf("self unreadable: code %d", rec.Code)
	}
}

func TestWorkOnNonBrokerIs404(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS()})
	rec := do(t, n.handler(), "POST", "/v1/work", "100.64.0.2:1", strings.NewReader(`{"device":"w"}`), "home")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code %d", rec.Code)
	}
	if errText(rec) == "" {
		t.Error("no JSON error")
	}
}

func TestOversizedBody413(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: &fakeBroker{}})
	big := `{"device":"` + strings.Repeat("a", maxBody+10) + `"}`
	for _, path := range []string{"/v1/work", "/v1/claim", "/v1/release"} {
		// With a Content-Length.
		rec := do(t, n.handler(), "POST", path, "100.64.0.2:1", strings.NewReader(big), "home")
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s with length: code %d", path, rec.Code)
		}
		// Without one (chunked).
		req := httptest.NewRequest("POST", path, io.MultiReader(strings.NewReader(big)))
		req.ContentLength = -1
		req.RemoteAddr = "100.64.0.2:1"
		req.Header.Set(poolHeader, "home")
		rr := httptest.NewRecorder()
		n.handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s chunked: code %d", path, rr.Code)
		}
	}
}

func TestPoolHeaderRequired(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: &fakeBroker{}})
	for _, pool := range []string{"", "work"} {
		rec := do(t, n.handler(), "POST", "/v1/work", "100.64.0.2:1", strings.NewReader(`{}`), pool)
		if rec.Code != 403 {
			t.Errorf("pool %q: code %d", pool, rec.Code)
		}
	}
}

func TestMethodsAndPaths(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: &fakeBroker{}})
	if rec := do(t, n.handler(), "GET", "/v1/work", "100.64.0.2:1", nil, "home"); rec.Code != 405 || errText(rec) == "" {
		t.Errorf("GET /v1/work: %d", rec.Code)
	}
	if rec := do(t, n.handler(), "POST", "/v1/hello", "100.64.0.2:1", nil, "home"); rec.Code != 405 {
		t.Errorf("POST /v1/hello: %d", rec.Code)
	}
	if rec := do(t, n.handler(), "GET", "/v2/x", "100.64.0.2:1", nil, "home"); rec.Code != 404 || errText(rec) == "" {
		t.Errorf("unknown path: %d", rec.Code)
	}
	if rec := do(t, n.handler(), "POST", "/v1/work", "100.64.0.2:1", strings.NewReader(`{`), "home"); rec.Code != 400 || errText(rec) == "" {
		t.Errorf("bad JSON: %d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Status: func() any { return map[string]int{"running": 2} }})
	rec := do(t, n.handler(), "GET", "/v1/status", "100.64.0.2:1", nil, "home")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"running":2}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestWorkSecretInResponseNotLogs(t *testing.T) {
	log, buf := debugLog()
	secret := []byte("JITCONFIG-very-secret-0123456789")
	fb := &fakeBroker{secret: secret}
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: fb, Log: log})
	req := core.WorkRequest{Device: "alice-linux", Free: 1, Platforms: []core.Platform{{OS: "linux", Arch: "x64"}}}
	body, _ := json.Marshal(req)
	rec := do(t, n.handler(), "POST", "/v1/work", "100.64.0.2:1", bytes.NewReader(body), "home")
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var resp core.WorkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Offers) != 1 || !bytes.Equal(resp.Offers[0].Secret, secret) {
		t.Fatalf("offer secret missing from the response: %+v", resp.Offers)
	}
	if len(fb.reqs) != 1 || fb.reqs[0].Device != "alice-linux" {
		t.Errorf("broker got %+v", fb.reqs)
	}
	assertNoSecret(t, buf.String(), secret)
	if !strings.Contains(buf.String(), "mesh: work") {
		t.Errorf("work not logged at all:\n%s", buf)
	}
}

func assertNoSecret(t *testing.T, logs string, secret []byte) {
	t.Helper()
	for _, form := range []string{string(secret), base64.StdEncoding.EncodeToString(secret), base64.RawStdEncoding.EncodeToString(secret)} {
		if strings.Contains(logs, form) {
			t.Fatalf("secret leaked into logs:\n%s", logs)
		}
	}
}

func TestBrokerErrorIs502(t *testing.T) {
	fb := &fakeBroker{err: errors.New("forge down")}
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: fb})
	rec := do(t, n.handler(), "POST", "/v1/work", "100.64.0.2:1", strings.NewReader(`{}`), "home")
	if rec.Code != http.StatusBadGateway || !strings.Contains(errText(rec), "forge down") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestClaimEndpoints(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS()})
	h := n.handler()
	post := func(path, body string) (int, string) {
		rec := do(t, h, "POST", path, "100.64.0.2:1", strings.NewReader(body), "home")
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	if c, b := post("/v1/claim", `{"job_key":"github:a/b:1","device":"d1","ttl_s":60}`); c != 200 || b != `{"granted":true,"holder":"d1"}` {
		t.Fatalf("claim d1: %d %s", c, b)
	}
	if c, b := post("/v1/claim", `{"job_key":"github:a/b:1","device":"d2","ttl_s":60}`); c != 200 || b != `{"granted":false,"holder":"d1"}` {
		t.Fatalf("claim d2: %d %s", c, b)
	}
	if c, b := post("/v1/release", `{"job_key":"github:a/b:1","device":"d2"}`); c != 200 || b != `{"released":false}` {
		t.Fatalf("release by non-holder: %d %s", c, b)
	}
	if c, b := post("/v1/release", `{"job_key":"github:a/b:1","device":"d1"}`); c != 200 || b != `{"released":true}` {
		t.Fatalf("release d1: %d %s", c, b)
	}
	if c, b := post("/v1/claim", `{"job_key":"github:a/b:1","device":"d2","ttl_s":60}`); c != 200 || b != `{"granted":true,"holder":"d2"}` {
		t.Fatalf("claim d2 after release: %d %s", c, b)
	}
	for _, bad := range []string{
		`{"job_key":"","device":"d","ttl_s":1}`,
		`{"job_key":"k","device":"","ttl_s":1}`,
		`{"job_key":"k","device":"d","ttl_s":0}`,
		`{"job_key":"k","device":"d","ttl_s":99999999}`,
	} {
		if c, _ := post("/v1/claim", bad); c != 400 {
			t.Errorf("claim %s: %d", bad, c)
		}
	}
}
