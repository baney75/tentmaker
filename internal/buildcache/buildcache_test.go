package buildcache

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rig serves a Cache on loopback to one caller the test can change.
type rig struct {
	t    testing.TB
	c    *Cache
	base string
	dir  string
	srv  *httptest.Server

	mu  sync.Mutex
	who Caller
}

func newRig(t testing.TB, o Options) *rig {
	t.Helper()
	r := &rig{t: t, dir: tmpDir(t)}
	r.c = New(o)
	r.who = r.caller(true)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		who := r.who
		r.mu.Unlock()
		r.c.Serve(w, req, who)
	}))
	t.Cleanup(r.srv.Close)
	r.base = r.srv.URL
	return r
}

// tmpDir is on tmpfs when the host has one, so the benchmark measures the
// service, not the disk.
func tmpDir(t testing.TB) string {
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		if d, err := os.MkdirTemp("/dev/shm", "buildcache-test-"); err == nil {
			t.Cleanup(func() { os.RemoveAll(d) })
			return d
		}
	}
	return t.TempDir()
}

func (r *rig) caller(write bool) Caller {
	return Caller{Repo: "github/acme/widget", MayWrite: func() bool { return write }, Open: func(create bool) (*os.Root, error) {
		if create {
			if err := os.MkdirAll(r.dir, 0o700); err != nil {
				return nil, err
			}
		}
		return os.OpenRoot(r.dir)
	}}
}

func (r *rig) set(who Caller) { r.mu.Lock(); r.who = who; r.mu.Unlock() }

func (r *rig) do(method, path string, body []byte, hdr ...string) (*http.Response, []byte) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, r.base+path, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp, got
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestBazelRoundTrip(t *testing.T) {
	r := newRig(t, Options{})
	blob := randBytes(100_000)
	d := digest(blob)
	if resp, _ := r.do("GET", "/cas/"+d, nil); resp.StatusCode != 404 {
		t.Fatalf("empty cache GET: %d", resp.StatusCode)
	}
	resp, _ := r.do("PUT", "/cas/"+d, blob)
	if resp.StatusCode != 200 || resp.Header.Get(StoredHeader) != "stored" {
		t.Fatalf("PUT: %d %q", resp.StatusCode, resp.Header.Get(StoredHeader))
	}
	resp, got := r.do("GET", "/cas/"+d, nil)
	if resp.StatusCode != 200 || !bytes.Equal(got, blob) {
		t.Fatalf("GET: %d, %d bytes", resp.StatusCode, len(got))
	}
	if resp, _ := r.do("HEAD", "/cas/"+d, nil); resp.StatusCode != 200 || resp.ContentLength != int64(len(blob)) {
		t.Fatalf("HEAD: %d %d", resp.StatusCode, resp.ContentLength)
	}
	// The action cache is a separate keyspace, and its body is not its hash.
	ar := []byte("an ActionResult")
	if resp, _ := r.do("GET", "/ac/"+d, nil); resp.StatusCode != 404 {
		t.Fatalf("AC shares the CAS keyspace: %d", resp.StatusCode)
	}
	if resp, _ := r.do("PUT", "/ac/"+d, ar); resp.StatusCode != 200 {
		t.Fatalf("AC PUT: %d", resp.StatusCode)
	}
	if _, got := r.do("GET", "/ac/"+d, nil); !bytes.Equal(got, ar) {
		t.Fatalf("AC GET: %q", got)
	}
	// An AC entry may be replaced.
	ar2 := []byte("a newer ActionResult")
	r.do("PUT", "/ac/"+d, ar2)
	if _, got := r.do("GET", "/ac/"+d, nil); !bytes.Equal(got, ar2) {
		t.Fatalf("AC GET after replace: %q", got)
	}
	s := r.c.Stats()[0]
	if s.Tool != ToolBazel || s.Hits != 3 || s.Misses != 2 || s.Stored != 3 || s.BytesServed != int64(len(blob)+len(ar)+len(ar2)) {
		t.Fatalf("stats: %+v", s)
	}
}

func TestCASDigestChecked(t *testing.T) {
	r := newRig(t, Options{})
	blob := randBytes(1000)
	wrong := digest([]byte("something else"))
	if resp, _ := r.do("PUT", "/cas/"+wrong, blob); resp.StatusCode != 400 {
		t.Fatalf("mismatched PUT: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/cas/"+wrong, nil); resp.StatusCode != 404 {
		t.Fatalf("a mismatched body was kept: %d", resp.StatusCode)
	}
	if names, _ := os.ReadDir(filepath.Join(r.dir, Folder, "tmp")); len(names) != 0 {
		t.Fatalf("temp files left: %v", names)
	}
}

func TestBadPathsAndMethods(t *testing.T) {
	r := newRig(t, Options{})
	for _, p := range []string{"/cas/abc", "/cas/" + strings.Repeat("A", 64), "/ac/../x", "/cache/", "/cache/a%2Fb", "/v8/artifacts/", "/nope"} {
		if resp, _ := r.do("GET", p, nil); resp.StatusCode != 404 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := r.do("DELETE", "/cache/k1", nil); resp.StatusCode != 405 {
		t.Errorf("DELETE: %d", resp.StatusCode)
	}
}

func TestGradleRoundTrip(t *testing.T) {
	r := newRig(t, Options{})
	body := randBytes(5000)
	if resp, _ := r.do("GET", "/cache/0123abcd", nil); resp.StatusCode != 404 {
		t.Fatalf("miss: %d", resp.StatusCode)
	}
	if resp, _ := r.do("PUT", "/cache/0123abcd", body); resp.StatusCode != 200 {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	if resp, got := r.do("GET", "/cache/0123abcd", nil); resp.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

func TestTurboRoundTrip(t *testing.T) {
	r := newRig(t, Options{})
	body := randBytes(3000)
	resp, got := r.do("GET", "/v8/artifacts/status?teamId=x", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(got), `"enabled"`) {
		t.Fatalf("status: %d %s", resp.StatusCode, got)
	}
	if resp, _ := r.do("HEAD", "/v8/artifacts/f00d?slug=tentmaker", nil); resp.StatusCode != 404 {
		t.Fatalf("HEAD miss: %d", resp.StatusCode)
	}
	resp, got = r.do("PUT", "/v8/artifacts/f00d?slug=tentmaker", body, "x-artifact-duration", "1234", "x-artifact-tag", "sig-abc", "Authorization", "Bearer tentmaker-no-secret")
	if resp.StatusCode != 202 || !strings.Contains(string(got), `"urls"`) {
		t.Fatalf("PUT: %d %s", resp.StatusCode, got)
	}
	if resp, _ := r.do("HEAD", "/v8/artifacts/f00d", nil); resp.StatusCode != 200 {
		t.Fatalf("HEAD hit: %d", resp.StatusCode)
	}
	resp, got = r.do("GET", "/v8/artifacts/f00d", nil)
	if resp.StatusCode != 200 || !bytes.Equal(got, body) || resp.Header.Get("x-artifact-tag") != "sig-abc" || resp.Header.Get("x-artifact-duration") != "1234" {
		t.Fatalf("GET: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := r.do("POST", "/v8/artifacts/events", []byte(`[{"event":"HIT"}]`)); resp.StatusCode != 200 {
		t.Fatalf("events: %d", resp.StatusCode)
	}
}

func TestReadOnlyWriteIsDropped(t *testing.T) {
	r := newRig(t, Options{})
	r.set(r.caller(false))
	body := randBytes(2000)
	resp, _ := r.do("PUT", "/cache/k1", body)
	if resp.StatusCode != 200 || resp.Header.Get(StoredHeader) != "not-stored" {
		t.Fatalf("read-only PUT: %d %q", resp.StatusCode, resp.Header.Get(StoredHeader))
	}
	if resp, _ := r.do("PUT", "/v8/artifacts/k1", body); resp.StatusCode != 202 || resp.Header.Get(StoredHeader) != "not-stored" {
		t.Fatalf("read-only turbo PUT: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/cache/k1", nil); resp.StatusCode != 404 {
		t.Fatalf("a read-only write was kept: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(r.dir, Folder)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read-only write touched the disk: %v", err)
	}
	if s := r.c.Stats()[1]; s.Tool != ToolGradle || s.NotStored != 1 || s.Stored != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestEntryCap(t *testing.T) {
	r := newRig(t, Options{MaxEntry: 1000})
	if resp, _ := r.do("PUT", "/cache/big", randBytes(1001)); resp.StatusCode != 413 {
		t.Fatalf("over the cap: %d", resp.StatusCode)
	}
	if resp, _ := r.do("PUT", "/cache/ok", randBytes(1000)); resp.StatusCode != 200 {
		t.Fatalf("at the cap: %d", resp.StatusCode)
	}
	// A chunked body (no Content-Length) is capped as it streams.
	req, _ := http.NewRequest("PUT", r.base+"/cache/chunked", io.MultiReader(bytes.NewReader(randBytes(800)), bytes.NewReader(randBytes(800))))
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("chunked over the cap: %d", resp.StatusCode)
	}
	if resp, _ := r.do("GET", "/cache/chunked", nil); resp.StatusCode != 404 {
		t.Fatalf("an over-cap entry was kept: %d", resp.StatusCode)
	}
}

func TestOpenErrorIsAMiss(t *testing.T) {
	r := newRig(t, Options{})
	r.set(Caller{Repo: "x", MayWrite: func() bool { return true }, Open: func(bool) (*os.Root, error) { return nil, errors.New("no folder") }})
	if resp, _ := r.do("GET", "/cache/k1", nil); resp.StatusCode != 404 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	// The build goes on: the write is answered as dropped.
	if resp, _ := r.do("PUT", "/cache/k1", []byte("x")); resp.StatusCode != 200 || resp.Header.Get(StoredHeader) != "not-stored" {
		t.Fatalf("PUT: %d %q", resp.StatusCode, resp.Header.Get(StoredHeader))
	}
}

func TestTornEntryIsAMiss(t *testing.T) {
	r := newRig(t, Options{})
	r.do("PUT", "/cache/k1", randBytes(5000))
	p := filepath.Join(r.dir, filepath.FromSlash(entryPath(kindGradle, "k1")))
	if err := os.Truncate(p, 3000); err != nil {
		t.Fatal(err)
	}
	if resp, _ := r.do("GET", "/cache/k1", nil); resp.StatusCode != 404 {
		t.Fatalf("a cut-short entry was served: %d", resp.StatusCode)
	}
	if err := os.WriteFile(p, []byte("not an entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, _ := r.do("GET", "/cache/k1", nil); resp.StatusCode != 404 {
		t.Fatalf("a foreign file was served: %d", resp.StatusCode)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func TestLeastRecentlyUsedEvictedAtTheBudget(t *testing.T) {
	clk := &clock{t: time.Now().Add(-time.Hour)}
	r := newRig(t, Options{MaxRepo: 10_500, Now: clk.now})
	put := func(k string) {
		t.Helper()
		if resp, _ := r.do("PUT", "/cache/"+k, randBytes(3000)); resp.StatusCode != 200 {
			t.Fatalf("PUT %s: %d", k, resp.StatusCode)
		}
		// mtimes come from the file system; set them from the clock.
		p := filepath.Join(r.dir, filepath.FromSlash(entryPath(kindGradle, k)))
		if err := os.Chtimes(p, clk.now(), clk.now()); err != nil {
			t.Fatal(err)
		}
		clk.add(10 * time.Minute)
	}
	put("a")
	put("b")
	put("c")
	// a is read, so b is now the least recently used.
	if resp, _ := r.do("GET", "/cache/a", nil); resp.StatusCode != 200 {
		t.Fatalf("GET a: %d", resp.StatusCode)
	}
	clk.add(10 * time.Minute)
	put("d") // 4 entries of ~3000 bytes pass 10500; eviction stops at 90%
	hit := func(k string) bool { resp, _ := r.do("HEAD", "/cache/"+k, nil); return resp.StatusCode == 200 }
	if hit("b") || !hit("a") || !hit("c") || !hit("d") {
		t.Fatalf("after eviction: a=%v b=%v c=%v d=%v", hit("a"), hit("b"), hit("c"), hit("d"))
	}
	// The caller's budget is honored when it is the smaller.
	who := r.caller(true)
	who.Budget = 4000
	r.set(who)
	put("e")
	if n := countEntries(t, r.dir); n != 1 {
		t.Fatalf("entries after a 4000 byte budget: %d", n)
	}
	if !hit("e") {
		t.Fatal("the entry just written was evicted")
	}
}

func countEntries(t *testing.T, dir string) int {
	n := 0
	for _, k := range kinds {
		es, _ := os.ReadDir(filepath.Join(dir, Folder, k.name))
		n += len(es)
	}
	return n
}

func TestOwns(t *testing.T) {
	for p, want := range map[string]bool{
		"/ac/x": true, "/cas/x": true, "/cache/x": true, "/v8/artifacts/x": true, "/v8/artifacts/events": true,
		"/twirp/x": false, "/devstoreaccount1/blob/x": false, "/": false, "/v8/teams": false,
	} {
		if Owns(p) != want {
			t.Errorf("Owns(%q) = %v", p, !want)
		}
	}
}

// BenchmarkBazelCASGet10MiB measures a local hit: one 10 MiB Bazel CAS GET
// round trip over loopback HTTP, from the request to the last byte read.
func BenchmarkBazelCASGet10MiB(b *testing.B) {
	r := newRig(b, Options{})
	blob := randBytes(10 << 20)
	d := digest(blob)
	if resp, _ := r.do("PUT", "/cas/"+d, blob); resp.StatusCode != 200 {
		b.Fatalf("PUT: %d", resp.StatusCode)
	}
	cl := &http.Client{}
	b.SetBytes(int64(len(blob)))
	b.ResetTimer()
	for b.Loop() {
		resp, err := cl.Get(r.base + "/cas/" + d)
		if err != nil {
			b.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || n != int64(len(blob)) {
			b.Fatalf("GET: %d, %d bytes", resp.StatusCode, n)
		}
	}
}

// BenchmarkBazelCASPut10MiB measures a save: one 10 MiB CAS PUT with its
// sha256 check and the rename into place.
func BenchmarkBazelCASPut10MiB(b *testing.B) {
	r := newRig(b, Options{})
	blob := randBytes(10 << 20)
	d := digest(blob)
	b.SetBytes(int64(len(blob)))
	b.ResetTimer()
	for b.Loop() {
		if resp, _ := r.do("PUT", "/cas/"+d, blob); resp.StatusCode != 200 {
			b.Fatalf("PUT: %d", resp.StatusCode)
		}
	}
}

func TestWriteRightCheckedAgainAtCommit(t *testing.T) {
	r := newRig(t, Options{})
	var calls int
	var mu sync.Mutex
	who := r.caller(true)
	who.MayWrite = func() bool { mu.Lock(); defer mu.Unlock(); calls++; return calls == 1 } // lost mid-write
	r.set(who)
	resp, _ := r.do("PUT", "/cache/k1", randBytes(1000))
	if resp.StatusCode != 200 || resp.Header.Get(StoredHeader) != "not-stored" {
		t.Fatalf("PUT: %d %q", resp.StatusCode, resp.Header.Get(StoredHeader))
	}
	r.set(r.caller(true))
	if resp, _ := r.do("GET", "/cache/k1", nil); resp.StatusCode != 404 {
		t.Fatalf("a write whose right went was kept: %d", resp.StatusCode)
	}
	if names, _ := os.ReadDir(filepath.Join(r.dir, Folder, "tmp")); len(names) != 0 {
		t.Fatalf("temp files left: %v", names)
	}
}
