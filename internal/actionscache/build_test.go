package actionscache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"

	"github.com/baney75/tentmaker/internal/buildcache"
	"github.com/baney75/tentmaker/internal/core"
)

// bput PUTs body to the build cache path as job j and returns the status
// and whether the device kept it.
func (j *job) bput(path string, body []byte) (int, string) {
	j.h.t.Helper()
	r, _ := http.NewRequest(http.MethodPut, j.h.base+path, bytes.NewReader(body))
	res, err := j.http.Do(r)
	if err != nil {
		j.h.t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, res.Header.Get(buildcache.StoredHeader)
}

func casPath(b []byte) string { s := sha256.Sum256(b); return "/cas/" + hex.EncodeToString(s[:]) }

func TestBuildCacheFollowsGrants(t *testing.T) {
	h := newHarness(t, Options{Build: buildcache.New(buildcache.Options{})})
	blob := randBytes(50_000)
	p := casPath(blob)

	// A connection no job owns gets 404 on every build cache path.
	stranger := h.client("stranger", "")
	for _, path := range []string{p, "/cache/k", "/v8/artifacts/status", "/v8/artifacts/k"} {
		if code, _ := stranger.get(h.base + path); code != 404 {
			t.Fatalf("unnamed connection GET %s: %d", path, code)
		}
	}
	if code, how := stranger.bput(p, blob); code != 404 || how != "" {
		t.Fatalf("unnamed connection PUT: %d %q", code, how)
	}

	// A confirmed writer of a push stores; a reader of the same repo hits.
	w := h.job("writer", "refs/heads/main", "refs/heads/main", Write, "")
	if code, how := w.bput(p, blob); code != 200 || how != "stored" {
		t.Fatalf("writer PUT: %d %q", code, how)
	}
	r := h.job("reader", "refs/pull/head/feature", "refs/heads/main", Read, "")
	if code, got := r.get(h.base + p); code != 200 || !bytes.Equal(got, blob) {
		t.Fatalf("reader GET: %d", code)
	}
	// A reader's write is read and dropped.
	other := randBytes(1000)
	if code, how := r.bput(casPath(other), other); code != 200 || how != "not-stored" {
		t.Fatalf("reader PUT: %d %q", code, how)
	}
	if code, _ := r.get(h.base + casPath(other)); code != 404 {
		t.Fatalf("a reader's write was kept: %d", code)
	}

	// Another repo's job sees nothing of this one's.
	if err := h.svc.Grant(Binding{Job: "elsewhere", Repo: core.CacheRepo{Forge: "github", Repo: "acme/other"},
		Ref: "refs/heads/main", Default: "refs/heads/main", Access: Write, Store: h.store, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.client("elsewhere", "").get(h.base + p); code != 404 {
		t.Fatalf("another repo's job read this repo's entry: %d", code)
	}

	// Held: a writer whose neighbor is not a confirmed writer may not save.
	h.svc.Hold("writer", true)
	if code, how := w.bput(casPath(other), other); code != 200 || how != "not-stored" {
		t.Fatalf("held writer PUT: %d %q", code, how)
	}
	h.svc.Hold("writer", false)

	// Limit to none (another trust's job started beside it): 404, a miss.
	h.svc.Limit("reader", None)
	if code, _ := r.get(h.base + p); code != 404 {
		t.Fatalf("a job limited to none read: %d", code)
	}
	// Revoked: 404.
	h.svc.Revoke("writer")
	if code, _ := w.get(h.base + p); code != 404 {
		t.Fatalf("a revoked job read: %d", code)
	}
}

func TestBuildCacheUnconfirmedAndPullRequestOnlyRead(t *testing.T) {
	h := newHarness(t, Options{Build: buildcache.New(buildcache.Options{})})
	blob := randBytes(2000)
	seed := h.job("seed", "refs/heads/main", "refs/heads/main", Write, "")
	if _, how := seed.bput("/cache/k1", blob); how != "stored" {
		t.Fatal("seed not stored")
	}

	// Unconfirmed: the runner may have taken another job; read only.
	b := Binding{Job: "unconfirmed", Repo: repo, Ref: "refs/heads/main", Default: "refs/heads/main", Access: Write, Store: h.store}
	if err := h.svc.Grant(b); err != nil {
		t.Fatal(err)
	}
	u := h.client("unconfirmed", "")
	if code, got := u.get(h.base + "/cache/k1"); code != 200 || !bytes.Equal(got, blob) {
		t.Fatalf("unconfirmed GET: %d", code)
	}
	if _, how := u.bput("/cache/k2", blob); how != "not-stored" {
		t.Fatalf("unconfirmed PUT kept: %q", how)
	}
	// Confirmed as a pull_request: Confirm lowers it to read for good.
	if err := h.svc.Grant(b.Confirm("pull_request", "main")); err != nil {
		t.Fatal(err)
	}
	if _, how := u.bput("/cache/k2", blob); how != "not-stored" {
		t.Fatalf("pull_request PUT kept: %q", how)
	}
	// Confirmed as a push of its own ref: it writes.
	b.Job = "pusher"
	if err := h.svc.Grant(b.Confirm("push", "main")); err != nil {
		t.Fatal(err)
	}
	if _, how := h.client("pusher", "").bput("/cache/k2", blob); how != "stored" {
		t.Fatalf("confirmed push PUT not kept: %q", how)
	}
}

func TestNoBuildCacheWithoutOption(t *testing.T) {
	h := newHarness(t, Options{})
	j := h.job("writer", "refs/heads/main", "refs/heads/main", Write, "")
	if code, _ := j.get(h.base + "/cache/k1"); code != 404 {
		t.Fatalf("GET with no build cache: %d", code)
	}
}
