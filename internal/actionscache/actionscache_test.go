package actionscache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/repocache"
)

// fakeStore is a repo cache on a temp folder.
type fakeStore struct{ dir string }

func (f fakeStore) ActionsRoot(c core.CacheRepo, create bool) (*os.Root, error) {
	k, err := repocache.Key(c)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(f.dir, k, repocache.ActionsDir)
	if create {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
	}
	r, err := os.OpenRoot(p)
	if err != nil {
		return nil, repocache.ErrNoActionsDir
	}
	return r, nil
}

// harness runs a Service on loopback. Each fake job dials its own
// connections, and JobOf names the job by the connection's client port,
// the way ProcPeers names it by the pasta process that owns it.
type harness struct {
	t     testing.TB
	svc   *Service
	base  string
	store fakeStore

	mu    sync.Mutex
	ports map[uint16]string
}

func newHarness(t testing.TB, o Options) *harness {
	t.Helper()
	h := &harness{t: t, ports: map[uint16]string{}}
	h.store = fakeStore{dir: shmDir(t)}
	o.JobOf = func(remote, _ netip.AddrPort, jobs []string) (string, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		j, ok := h.ports[remote.Port()]
		if !ok {
			return "", false
		}
		for _, x := range jobs {
			if x == j {
				return j, true
			}
		}
		return "", false
	}
	h.svc = New(o)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.svc.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for h.svc.URL() == "" {
		time.Sleep(time.Millisecond)
	}
	h.base = strings.TrimSuffix(h.svc.URL(), "/")
	return h
}

// shmDir is a temp folder on tmpfs when the host has one, so the benchmark
// measures the service, not the disk.
func shmDir(t testing.TB) string {
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		d, err := os.MkdirTemp("/dev/shm", "actionscache-test-")
		if err == nil {
			t.Cleanup(func() { os.RemoveAll(d) })
			return d
		}
	}
	return t.TempDir()
}

var repo = core.CacheRepo{Forge: "github", Repo: "acme/widget"}

type job struct {
	h     *harness
	name  string
	token string
	http  *http.Client
}

// grant registers a job and returns a client that acts as it.
func (h *harness) job(name, ref, def string, a Access, tok string) *job {
	h.t.Helper()
	if err := h.svc.Grant(Binding{Job: name, Repo: repo, Ref: ref, Default: def, Access: a, Store: h.store}); err != nil {
		h.t.Fatal(err)
	}
	return h.client(name, tok)
}

// client acts as job name without granting it.
func (h *harness) client(name, tok string) *job {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			h.mu.Lock()
			h.ports[uint16(c.LocalAddr().(*net.TCPAddr).Port)] = name
			h.mu.Unlock()
			return c, nil
		},
		MaxIdleConnsPerHost: 16,
	}
	h.t.Cleanup(tr.CloseIdleConnections)
	return &job{h: h, name: name, token: tok, http: &http.Client{Transport: tr, Timeout: time.Minute}}
}

// token makes a runtime token whose ac claim grants these scopes. The
// signature is junk: the service never trusts the token for more than the
// binding.
func token(scopes ...tokenScope) string {
	ac, _ := json.Marshal(scopes)
	claims, _ := json.Marshal(map[string]string{"ac": string(ac)})
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(claims) + ".c2ln"
}

func rw(scope string) tokenScope { return tokenScope{Scope: scope, Permission: 3} }
func ro(scope string) tokenScope { return tokenScope{Scope: scope, Permission: 1} }

func (j *job) twirp(method string, req, resp any) int {
	j.h.t.Helper()
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, j.h.base+cachePrefix+method, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if j.token != "" {
		r.Header.Set("Authorization", "Bearer "+j.token)
	}
	res, err := j.http.Do(r)
	if err != nil {
		j.h.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusOK && resp != nil {
		if err := json.Unmarshal(data, resp); err != nil {
			j.h.t.Fatalf("%s: %v: %s", method, err, data)
		}
	}
	return res.StatusCode
}

func (j *job) put(url string, body []byte) int {
	j.h.t.Helper()
	r, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	r.Header.Set("x-ms-blob-type", "BlockBlob")
	res, err := j.http.Do(r)
	if err != nil {
		j.h.t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

// upload sends data the way the Azure SDK does: one PUT up to block, or
// staged blocks of that size, eight at a time, and a block list.
func (j *job) upload(url string, data []byte, block int) {
	j.h.t.Helper()
	if len(data) <= block {
		if c := j.put(url, data); c != http.StatusCreated {
			j.h.t.Fatalf("single put: %d", c)
		}
		return
	}
	var ids []string
	for off := 0; off < len(data); off += block {
		ids = append(ids, base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "block-%06d", len(ids))))
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	errs := make(chan string, len(ids))
	for i, id := range ids {
		end := min((i+1)*block, len(data))
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			u := url + "&comp=block&blockid=" + strings.ReplaceAll(id, "=", "%3D")
			if c := j.put(u, data[i*block:end]); c != http.StatusCreated {
				errs <- fmt.Sprintf("block %d: %d", i, c)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		j.h.t.Fatal(e)
	}
	var xml strings.Builder
	xml.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		xml.WriteString("<Latest>" + id + "</Latest>")
	}
	xml.WriteString("</BlockList>")
	if c := j.put(url+"&comp=blocklist", []byte(xml.String())); c != http.StatusCreated {
		j.h.t.Fatalf("block list: %d", c)
	}
}

// save is the toolkit's saveCache: create, upload, finalize. It returns
// the create response's message when the save was refused.
func (j *job) save(key, version string, data []byte, block int) (bool, string) {
	j.h.t.Helper()
	var c createResp
	if code := j.twirp("CreateCacheEntry", createReq{Key: key, Version: version}, &c); code != http.StatusOK {
		j.h.t.Fatalf("create: %d", code)
	}
	if !c.OK {
		return false, c.Message
	}
	j.upload(c.SignedUploadURL, data, block)
	var f finalizeResp
	if code := j.twirp("FinalizeCacheEntryUpload", map[string]any{"key": key, "version": version, "size_bytes": fmt.Sprint(len(data))}, &f); code != http.StatusOK {
		j.h.t.Fatalf("finalize: %d", code)
	}
	if !f.OK {
		return false, f.Message
	}
	return true, ""
}

func (j *job) lookup(key, version string, restore ...string) downloadResp {
	j.h.t.Helper()
	var d downloadResp
	if code := j.twirp("GetCacheEntryDownloadURL", downloadReq{Key: key, RestoreKeys: restore, Version: version}, &d); code != http.StatusOK {
		j.h.t.Fatalf("lookup: %d", code)
	}
	return d
}

// get is the toolkit's download: a plain GET with no Authorization.
func (j *job) get(url string, hdr ...string) (int, []byte) {
	j.h.t.Helper()
	r, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := j.http.Do(r)
	if err != nil {
		j.h.t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		j.h.t.Fatal(err)
	}
	return res.StatusCode, data
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

const ver = "4f0b1d0ea8c2d6d4f9d6c6c1c9a1e5b7e6b0f1d2c3b4a5968778695a4b3c2d1e"

func TestRoundTrip(t *testing.T) {
	h := newHarness(t, Options{})
	main := h.job("tm-main-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))

	small := randBytes(1000)
	big := randBytes(5*1024 + 17) // staged in 1 KiB blocks
	if ok, msg := main.save("go-linux-abc", ver, small, 4096); !ok {
		t.Fatalf("save small: %s", msg)
	}
	if ok, msg := main.save("npm-linux-123", ver, big, 1024); !ok {
		t.Fatalf("save blocks: %s", msg)
	}

	d := main.lookup("go-linux-abc", ver)
	if !d.OK || d.MatchedKey != "go-linux-abc" {
		t.Fatalf("exact lookup: %+v", d)
	}
	if !strings.Contains(d.SignedDownloadURL, "sig=") {
		t.Fatalf("download URL has no capability: %s", d.SignedDownloadURL)
	}
	if code, got := main.get(d.SignedDownloadURL); code != 200 || !bytes.Equal(got, small) {
		t.Fatalf("restore exact: %d, %d bytes", code, len(got))
	}

	d = main.lookup("npm-linux-999", ver, "npm-linux-", "npm-")
	if !d.OK || d.MatchedKey != "npm-linux-123" {
		t.Fatalf("prefix lookup: %+v", d)
	}
	if code, got := main.get(d.SignedDownloadURL); code != 200 || !bytes.Equal(got, big) {
		t.Fatalf("restore prefix: %d, %d bytes", code, len(got))
	}

	if d := main.lookup("go-linux-abc", strings.Repeat("0", 64)); d.OK {
		t.Fatalf("another version matched: %+v", d)
	}
	if d := main.lookup("nothing", ver, "zzz"); d.OK {
		t.Fatalf("miss matched: %+v", d)
	}

	// Entries are immutable.
	if ok, _ := main.save("go-linux-abc", ver, randBytes(10), 4096); ok {
		t.Fatal("a second save of the same key and version was accepted")
	}
	if _, got := main.get(main.lookup("go-linux-abc", ver).SignedDownloadURL); !bytes.Equal(got, small) {
		t.Fatal("the entry changed")
	}
}

func TestNewestPrefixMatchWins(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	h := newHarness(t, Options{Now: func() time.Time { mu.Lock(); defer mu.Unlock(); now = now.Add(time.Minute); return now }})
	main := h.job("tm-main-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	for _, k := range []string{"deps-1", "deps-3", "deps-2"} {
		if ok, msg := main.save(k, ver, []byte(k), 4096); !ok {
			t.Fatal(msg)
		}
	}
	if d := main.lookup("deps-9", ver, "deps-"); d.MatchedKey != "deps-2" {
		t.Fatalf("got %q, want the newest, deps-2", d.MatchedKey)
	}
}

func TestRefScoping(t *testing.T) {
	h := newHarness(t, Options{})
	main := h.job("tm-main-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	mainData := []byte("from main")
	if ok, msg := main.save("deps-main", ver, mainData, 4096); !ok {
		t.Fatal(msg)
	}

	// A pull request job reads main's entries and writes its own scope.
	pr := h.job("tm-pr-1", RefScope("pull_request", "feature"), BranchScope("main"), Write,
		token(rw("refs/pull/7/merge"), ro("refs/heads/main")))
	d := pr.lookup("deps-main", ver)
	if !d.OK {
		t.Fatal("the PR did not read main's entry")
	}
	if _, got := pr.get(d.SignedDownloadURL); !bytes.Equal(got, mainData) {
		t.Fatal("the PR read the wrong bytes")
	}
	// The PR can save a key main already has: it lands in its own scope.
	if ok, msg := pr.save("deps-main", ver, []byte("poisoned"), 4096); !ok {
		t.Fatalf("PR save to its own scope: %s", msg)
	}
	if ok, msg := pr.save("deps-pr", ver, []byte("from pr"), 4096); !ok {
		t.Fatal(msg)
	}
	// It reads its own first.
	if _, got := pr.get(pr.lookup("deps-main", ver).SignedDownloadURL); string(got) != "poisoned" {
		t.Fatalf("PR read %q, want its own entry first", got)
	}

	// Main never sees the PR's writes.
	if _, got := main.get(main.lookup("deps-main", ver).SignedDownloadURL); !bytes.Equal(got, mainData) {
		t.Fatalf("main read %q", got)
	}
	if d := main.lookup("deps-pr", ver); d.OK {
		t.Fatal("main read the PR's entry")
	}
	if d := main.lookup("deps-x", ver, "deps-p"); d.OK {
		t.Fatal("main matched the PR's entry by prefix")
	}

	// Another branch reads main, not the PR.
	other := h.job("tm-other-1", BranchScope("dev"), BranchScope("main"), Write, token(rw("refs/heads/dev")))
	if d := other.lookup("deps-pr", ver); d.OK {
		t.Fatal("another branch read the PR's entry")
	}
	if d := other.lookup("deps-main", ver); !d.OK {
		t.Fatal("another branch did not read main")
	}

	// Another PR can't fetch this PR's entry, even with its file name.
	pr2 := h.job("tm-pr-2", RefScope("pull_request", "other"), BranchScope("main"), Write,
		token(rw("refs/pull/8/merge")))
	file := strings.TrimPrefix(strings.Split(pr.lookup("deps-pr", ver).SignedDownloadURL, "?")[0], h.base+blobPrefix)
	if code, _ := pr2.get(h.base + blobPrefix + file + "?sig=" + h.svc.grantOf("tm-pr-2").capability); code != http.StatusNotFound {
		t.Fatalf("another PR fetched a PR entry by file name: %d", code)
	}
}

func TestTokenNarrowsBinding(t *testing.T) {
	h := newHarness(t, Options{})
	main := h.job("tm-main-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	if ok, msg := main.save("dev-deps", ver, []byte("main's"), 4096); !ok {
		t.Fatal(msg)
	}
	// The runner was granted for dev but runs a job whose token names
	// another branch: dev is neither written nor read, main is still read.
	dev := h.job("tm-dev-1", BranchScope("dev"), BranchScope("main"), Write, token(rw("refs/heads/release")))
	if ok, msg := dev.save("dev-deps", ver, []byte("x"), 4096); ok || !strings.HasPrefix(msg, denied) {
		t.Fatalf("save with a mismatched token: %v %q", ok, msg)
	}
	if d := dev.lookup("dev-deps", ver); !d.OK {
		t.Fatal("the default branch was not read")
	}
	// No token at all: the same.
	none := h.job("tm-dev-2", BranchScope("dev"), BranchScope("main"), Write, "")
	if ok, _ := none.save("dev-deps-2", ver, []byte("x"), 4096); ok {
		t.Fatal("a save with no token was accepted")
	}
}

func TestReadOnlyCreateRefused(t *testing.T) {
	h := newHarness(t, Options{})
	w := h.job("tm-w-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	if ok, msg := w.save("k", ver, []byte("data"), 4096); !ok {
		t.Fatal(msg)
	}
	r := h.job("tm-r-1", BranchScope("main"), BranchScope("main"), Read, token(rw("refs/heads/main")))
	var c createResp
	if code := r.twirp("CreateCacheEntry", createReq{Key: "k2", Version: ver}, &c); code != 200 {
		t.Fatal(code)
	}
	if c.OK || c.SignedUploadURL != "" || !strings.HasPrefix(c.Message, denied) {
		t.Fatalf("read-only create: %+v", c)
	}
	if d := r.lookup("k", ver); !d.OK {
		t.Fatal("a read-only job could not restore")
	}

	n := h.job("tm-n-1", BranchScope("main"), "", None, token(rw("refs/heads/main")))
	c = createResp{}
	n.twirp("CreateCacheEntry", createReq{Key: "k3", Version: ver}, &c)
	if c.OK || !strings.HasPrefix(c.Message, denied) {
		t.Fatalf("no-access create: %+v", c)
	}
	if d := n.lookup("k", ver); d.OK {
		t.Fatal("a job with no access restored")
	}

	// A write grant lowered to read mid-upload can't finalize.
	var c2 createResp
	w.twirp("CreateCacheEntry", createReq{Key: "late", Version: ver}, &c2)
	if !c2.OK {
		t.Fatal(c2.Message)
	}
	w.upload(c2.SignedUploadURL, []byte("late data"), 4096)
	h.svc.Limit("tm-w-1", Read)
	var f finalizeResp
	w.twirp("FinalizeCacheEntryUpload", map[string]any{"key": "late", "version": ver, "size_bytes": "9"}, &f)
	if f.OK {
		t.Fatal("a job lowered to read finalized")
	}
	if d := w.lookup("late", ver); d.OK {
		t.Fatal("the lowered job's entry was committed")
	}
}

func TestRevokedCapabilityGets404(t *testing.T) {
	h := newHarness(t, Options{})
	j := h.job("tm-a-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	if ok, msg := j.save("k", ver, []byte("data"), 4096); !ok {
		t.Fatal(msg)
	}
	url := j.lookup("k", ver).SignedDownloadURL
	var c createResp
	j.twirp("CreateCacheEntry", createReq{Key: "pending", Version: ver}, &c)
	if !c.OK {
		t.Fatal(c.Message)
	}

	// Another job's connection can't use this job's URLs.
	other := h.job("tm-b-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	if code, _ := other.get(url); code != http.StatusNotFound {
		t.Fatalf("another job used the URL: %d", code)
	}
	if code := other.put(c.SignedUploadURL, []byte("x")); code != http.StatusNotFound {
		t.Fatalf("another job used the upload URL: %d", code)
	}
	// A connection no job owns gets nothing.
	stranger := h.client("tm-ghost-1", token(rw("refs/heads/main")))
	if code := stranger.twirp("GetCacheEntryDownloadURL", downloadReq{Key: "k", Version: ver}, nil); code != http.StatusNotFound {
		t.Fatalf("unknown connection: %d", code)
	}

	h.svc.Revoke("tm-a-1")
	if code, _ := j.get(url); code != http.StatusNotFound {
		t.Fatalf("revoked download: %d", code)
	}
	if code := j.put(c.SignedUploadURL, []byte("x")); code != http.StatusNotFound {
		t.Fatalf("revoked upload: %d", code)
	}
	if code := j.twirp("GetCacheEntryDownloadURL", downloadReq{Key: "k", Version: ver}, nil); code != http.StatusNotFound {
		t.Fatalf("revoked twirp: %d", code)
	}
	// Its staging folder is gone.
	root, _ := h.store.ActionsRoot(repo, false)
	defer root.Close()
	d, _ := root.Open(".")
	names, _ := d.Readdirnames(-1)
	d.Close()
	for _, n := range names {
		if strings.HasPrefix(n, "u-") {
			t.Fatalf("staging left behind: %s", n)
		}
	}
	// Granting the name again gives a new capability; the old URL stays dead.
	h.job("tm-a-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	if code, _ := j.get(url); code != http.StatusNotFound {
		t.Fatalf("old URL after a new grant: %d", code)
	}
}

func TestRangeGet(t *testing.T) {
	h := newHarness(t, Options{})
	j := h.job("tm-a-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	data := randBytes(4096)
	if ok, msg := j.save("k", ver, data, 1000); !ok {
		t.Fatal(msg)
	}
	url := j.lookup("k", ver).SignedDownloadURL
	code, got := j.get(url, "Range", "bytes=100-1099")
	if code != http.StatusPartialContent || !bytes.Equal(got, data[100:1100]) {
		t.Fatalf("Range: %d, %d bytes", code, len(got))
	}
	code, got = j.get(url, "x-ms-range", "bytes=4000-")
	if code != http.StatusPartialContent || !bytes.Equal(got, data[4000:]) {
		t.Fatalf("x-ms-range: %d, %d bytes", code, len(got))
	}
	if code, _ := j.get(url, "Range", "bytes=5000-6000"); code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out of range: %d", code)
	}
}

func TestUploadCaps(t *testing.T) {
	h := newHarness(t, Options{MaxEntry: 100, MaxJob: 150})
	j := h.job("tm-a-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	var c createResp
	j.twirp("CreateCacheEntry", createReq{Key: "big", Version: ver}, &c)
	if code := j.put(c.SignedUploadURL, randBytes(101)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the entry cap: %d", code)
	}
	if ok, msg := j.save("a", ver, randBytes(90), 4096); !ok {
		t.Fatal(msg)
	}
	var c2 createResp
	j.twirp("CreateCacheEntry", createReq{Key: "b", Version: ver}, &c2)
	if code := j.put(c2.SignedUploadURL, randBytes(90)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the job cap: %d", code)
	}
	// A short finalize size is refused.
	j2 := h.job("tm-b-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	var c3 createResp
	j2.twirp("CreateCacheEntry", createReq{Key: "c", Version: ver}, &c3)
	j2.upload(c3.SignedUploadURL, randBytes(50), 4096)
	var f finalizeResp
	j2.twirp("FinalizeCacheEntryUpload", map[string]any{"key": "c", "version": ver, "size_bytes": "60"}, &f)
	if f.OK {
		t.Fatal("a size mismatch was committed")
	}
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t, Options{})
	j := h.job("tm-a-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	if code := j.twirp("CreateCacheEntry", createReq{Key: "a,b", Version: ver}, nil); code != http.StatusBadRequest {
		t.Fatalf("comma key: %d", code)
	}
	if code := j.twirp("Nope", struct{}{}, nil); code != http.StatusNotFound {
		t.Fatalf("unknown method: %d", code)
	}
	// Only loopback Host names.
	r, _ := http.NewRequest(http.MethodPost, h.base+cachePrefix+"GetCacheEntryDownloadURL", strings.NewReader("{}"))
	r.Host = "evil.example"
	res, err := j.http.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host: %d", res.StatusCode)
	}
}

func TestPassthrough(t *testing.T) {
	var got struct {
		path, auth, host string
		body             []byte
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.host = r.URL.Path, r.Header.Get("Authorization"), r.Host
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"signed_upload_url":"https://blob.example/x"}`)
	}))
	defer up.Close()
	h := newHarness(t, Options{Upstream: up.URL, HTTP: up.Client()})
	j := h.job("tm-a-1", BranchScope("main"), "", None, "tok-123")

	path := "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact"
	r, _ := http.NewRequest(http.MethodPost, h.base+path, strings.NewReader(`{"name":"dist"}`))
	r.Header.Set("Authorization", "Bearer tok-123")
	res, err := j.http.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "blob.example") {
		t.Fatalf("passthrough: %d %s", res.StatusCode, body)
	}
	if got.path != path || got.auth != "Bearer tok-123" || string(got.body) != `{"name":"dist"}` {
		t.Fatalf("upstream saw %+v", got)
	}
	if !strings.HasPrefix(up.URL, "http://"+got.host) {
		t.Fatalf("upstream Host %q", got.host)
	}

	// No job, no passthrough.
	stranger := h.client("tm-ghost-1", "")
	r, _ = http.NewRequest(http.MethodPost, h.base+path, strings.NewReader(`{}`))
	res, err = stranger.http.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("stranger passthrough: %d", res.StatusCode)
	}
}

func TestPassthroughUpstreamDown(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	url := up.URL
	up.Close()
	h := newHarness(t, Options{Upstream: url})
	j := h.job("tm-a-1", BranchScope("main"), "", None, "")
	r, _ := http.NewRequest(http.MethodPost, h.base+"/twirp/x.Y/Z", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer secret-token-value")
	res, err := j.http.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "secret-token-value") {
		t.Fatalf("down upstream: %d %s", res.StatusCode, body)
	}
}

func TestGrantKeepsCapability(t *testing.T) {
	s := New(Options{})
	st := fakeStore{dir: t.TempDir()}
	if err := s.Grant(Binding{Job: "tm-a-1", Repo: repo, Access: Write, Store: st}); err != nil {
		t.Fatal(err)
	}
	c := s.grantOf("tm-a-1").capability
	s.grantOf("tm-a-1").used.Add(42)
	if err := s.Grant(Binding{Job: "tm-a-1", Repo: repo, Access: Read, Store: st}); err != nil {
		t.Fatal(err)
	}
	s.Limit("tm-a-1", None)
	g := s.grantOf("tm-a-1")
	if g.capability != c || g.Access != None || g.used.Load() != 42 {
		t.Fatalf("after update: cap kept %v, access %v, used %d", g.capability == c, g.Access, g.used.Load())
	}
	s.Limit("tm-a-1", Write) // never raises
	if a, _ := s.Granted("tm-a-1"); a != None {
		t.Fatalf("Limit raised access to %v", a)
	}
	if len(c) != 64 {
		t.Fatalf("capability is %d hex chars", len(c))
	}
	if err := s.Grant(Binding{Job: "../x"}); err == nil {
		t.Fatal("a bad job name was granted")
	}
	if err := s.Grant(Binding{Job: "tm-b-1", Access: Read}); err == nil {
		t.Fatal("a reading grant without a store was accepted")
	}
}

func TestScopes(t *testing.T) {
	for _, c := range []struct{ event, branch, want string }{
		{"push", "main", "refs/heads/main"},
		{"pull_request", "feat", "refs/pull/head/feat"},
		{"pull_request_target", "feat", "refs/pull/head/feat"},
		{"workflow_dispatch", "dev", "refs/heads/dev"},
		{"push", "", ""},
	} {
		if got := RefScope(c.event, c.branch); got != c.want {
			t.Errorf("RefScope(%q, %q) = %q, want %q", c.event, c.branch, got, c.want)
		}
	}
	ts := []tokenScope{rw("refs/pull/3/merge"), ro("refs/heads/main")}
	if !tokenGrants(ts, "refs/pull/head/x", writeBit) || tokenGrants(ts, "refs/heads/main", writeBit) || !tokenGrants(ts, "refs/heads/main", readBit) {
		t.Fatal("tokenGrants")
	}
	if tokenGrants([]tokenScope{rw("refs/heads/mainline")}, "refs/heads/main", writeBit) {
		t.Fatal("a branch prefix granted another branch")
	}
}

func TestSweepRemovesStaleStaging(t *testing.T) {
	now := time.Now()
	h := newHarness(t, Options{Now: func() time.Time { return now }})
	j := h.job("tm-a-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	root, err := h.store.ActionsRoot(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	old := now.Add(-7 * time.Hour)
	for _, n := range []string{"u-" + strings.Repeat("a", 32), "e-" + strings.Repeat("b", 32), "keep.txt"} {
		if err := root.Mkdir(n, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := root.Chtimes(n, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if ok, msg := j.save("k", ver, []byte("x"), 4096); !ok {
		t.Fatal(msg)
	}
	for n, want := range map[string]bool{"u-" + strings.Repeat("a", 32): false, "e-" + strings.Repeat("b", 32): false, "keep.txt": true} {
		_, err := root.Lstat(n)
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", n, err == nil, want)
		}
	}
	if d := j.lookup("k", ver); !d.OK {
		t.Fatal("the sweep removed a live entry")
	}
}

func TestProcPeers(t *testing.T) {
	proc := t.TempDir()
	cg := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// pasta of job b (pid 200) holds the connection 127.0.0.1:40000 -> 127.0.0.1:7436.
	write(filepath.Join(proc, "net", "tcp"), strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 0100007F:1D0C 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1111 1 0 100 0 0 10 0",
		"   1: 0100007F:9C40 0100007F:1D0C 01 00000000:00000000 00:00000000 00000000  1001        0 4242 1 0 20 4 30 10 -1",
		"   2: 0100007F:1D0C 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 4243 1 0 20 4 30 10 -1",
	}, "\n")+"\n")
	write(filepath.Join(proc, "net", "tcp6"), "  sl  local_address remote_address st\n")
	write(filepath.Join(cg, "a", "cgroup.procs"), "100\n")
	write(filepath.Join(cg, "b", "cgroup.procs"), "200\n201\n")
	for pid, links := range map[string][]string{
		"100": {"socket:[9999]", "/dev/null"},
		"200": {"/dev/null", "socket:[4243]"}, // the daemon's end: not this one
		"201": {"socket:[4242]"},
	} {
		if err := os.MkdirAll(filepath.Join(proc, pid, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		for i, l := range links {
			if err := os.Symlink(l, filepath.Join(proc, pid, "fd", fmt.Sprint(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	p := ProcPeers{Proc: proc, Cgroup: func(j string) string { return filepath.Join(cg, j) }}
	remote := netip.MustParseAddrPort("127.0.0.1:40000")
	local := netip.MustParseAddrPort("127.0.0.1:7436")
	if j, ok := p.JobOf(remote, local, []string{"a", "b"}); !ok || j != "b" {
		t.Fatalf("JobOf = %q %v, want b", j, ok)
	}
	if _, ok := p.JobOf(remote, local, []string{"a"}); ok {
		t.Fatal("a job that doesn't hold the socket was named")
	}
	if _, ok := p.JobOf(netip.MustParseAddrPort("127.0.0.1:40001"), local, []string{"a", "b"}); ok {
		t.Fatal("an unknown connection was named")
	}
	if _, ok := p.JobOf(netip.MustParseAddrPort("10.0.0.5:40000"), local, []string{"b"}); ok {
		t.Fatal("a non-loopback peer was named")
	}
	if got := procAddr(netip.MustParseAddrPort("[::1]:7436"), true); got != "00000000000000000000000001000000:1D0C" {
		t.Fatalf("procAddr v6 = %s", got)
	}
}

// BenchmarkSaveRestore256MiB saves and restores one 256 MiB entry on
// tmpfs, the way the toolkit does (64 MiB blocks, eight at a time; one
// GET).
func BenchmarkSaveRestore256MiB(b *testing.B) {
	const size = 256 << 20
	data := randBytes(size)
	h := newHarness(b, Options{})
	j := h.job("tm-bench-1", BranchScope("main"), BranchScope("main"), Write, token(rw("refs/heads/main")))
	n := 0
	b.Run("save", func(b *testing.B) {
		b.SetBytes(size)
		for b.Loop() {
			n++
			if ok, msg := j.save(fmt.Sprintf("bench-%d", n), ver, data, 64<<20); !ok {
				b.Fatal(msg)
			}
		}
	})
	if ok, msg := j.save("bench-restore", ver, data, 64<<20); !ok {
		b.Fatal(msg)
	}
	url := j.lookup("bench-restore", ver).SignedDownloadURL
	b.Run("restore", func(b *testing.B) {
		b.SetBytes(size)
		for b.Loop() {
			res, err := j.http.Get(url)
			if err != nil {
				b.Fatal(err)
			}
			n, err := io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if err != nil || n != size {
				b.Fatalf("restored %d bytes: %v", n, err)
			}
		}
	})
}
