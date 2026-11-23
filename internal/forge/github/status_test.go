package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func TestPendingCommits(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app", 200, map[string]any{"default_branch": "main"})
	h.f.json("GET", "/repos/acme/app/pulls?state=open&per_page=100&page=1", 200, []map[string]any{
		{"number": 3, "user": map[string]any{"login": "owner"}, "head": map[string]any{"ref": "feat", "sha": "aaa", "repo": map[string]any{"full_name": "acme/app"}}},
		{"number": 4, "user": map[string]any{"login": "stranger"}, "head": map[string]any{"ref": "main", "sha": "bbb", "repo": map[string]any{"full_name": "stranger/app"}}},
		{"number": 5, "user": map[string]any{"login": "gone"}, "head": map[string]any{"ref": "x", "sha": "ccc", "repo": nil}},
	})
	h.f.json("GET", "/repos/acme/app/branches/main", 200, map[string]any{"commit": map[string]any{"sha": "ddd", "author": map[string]any{"login": "owner"}}})
	got, err := h.c.PendingCommits(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Commit{
		{Repo: "acme/app", SHA: "aaa", Ref: "feat", PR: 3, Author: "owner"},
		{Repo: "acme/app", SHA: "bbb", Ref: "main", PR: 4, Author: "stranger", FromFork: true},
		{Repo: "acme/app", SHA: "ccc", Ref: "x", PR: 5, Author: "gone", FromFork: true},
		{Repo: "acme/app", SHA: "ddd", Ref: "main", Author: "owner"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestPendingCommitsBranches(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app", 200, map[string]any{"default_branch": "main"})
	h.f.json("GET", "/repos/acme/app/pulls?state=open&per_page=100&page=1", 200, []map[string]any{})
	h.f.json("GET", "/repos/acme/app/branches/main", 200, map[string]any{"commit": map[string]any{"sha": "ddd"}})
	h.f.json("GET", "/repos/acme/app/branches/release%2F1", 200, map[string]any{"commit": map[string]any{"sha": "eee", "author": map[string]any{"login": "owner"}}})
	h.f.json("GET", "/repos/acme/app/branches/gone", 404, map[string]any{"message": "Branch not found"})
	got, err := h.c.PendingCommits(context.Background(), "acme/app", "release/1", "main", "gone")
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Commit{
		{Repo: "acme/app", SHA: "ddd", Ref: "main"},
		{Repo: "acme/app", SHA: "eee", Ref: "release/1", Author: "owner"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// busyRepo scripts a repo whose other branches see a push every second:
// the repo object's ETag changes on every call (its pushed_at moves), while
// the PR list and the watched branches stay put and answer 304.
func busyRepo(h *harness) {
	pushes := 0
	h.f.on("GET", "/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		h.f.mu.Lock()
		pushes++
		etag := fmt.Sprintf(`"repo-%d"`, pushes)
		h.f.mu.Unlock()
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, `{"default_branch":"main"}`)
	})
	h.f.etagJSON("/repos/acme/app/pulls?state=open&per_page=100&page=1", `"prs"`, []map[string]any{})
	h.f.etagJSON("/repos/acme/app/branches/main", `"main"`, map[string]any{"commit": map[string]any{"sha": "ddd"}})
	h.f.etagJSON("/repos/acme/app/branches/dev", `"dev"`, map[string]any{"commit": map[string]any{"sha": "eee"}})
}

// Polling an unchanged repo every second costs almost nothing against the
// primary limit, however busy the rest of the repo is. Before, every poll
// read the repo object, whose ETag changes on any push, so a busy repo
// billed a request a poll per device (3600 an hour; issue #23).
func TestPendingCommitsIdleBillsAlmostNothing(t *testing.T) {
	h := newHarness(t)
	busyRepo(h)
	ctx := context.Background()
	for i := 0; i < 1800; i++ { // half an hour at one poll a second
		got, err := h.c.PendingCommits(ctx, "acme/app", "dev")
		if err != nil || len(got) != 2 {
			t.Fatalf("poll %d: %v, %v", i, got, err)
		}
		h.advance(time.Second)
	}
	st := h.c.APIStats()
	// The first poll reads all four; then one repo read per DefaultBranchTTL.
	if st.Billed > 3+3 {
		t.Fatalf("billed %d requests in half an hour of idle polls; want at most 6 (%+v)", st.Billed, st)
	}
	if n := h.f.count("GET", "/repos/acme/app"); n != 3 {
		t.Fatalf("read the repo %d times in 30 minutes; want 3 (every %v)", n, DefaultBranchTTL)
	}
	if st.Requests < 3*1800 {
		t.Fatalf("only %d requests: the PR list and branches must be read every poll", st.Requests)
	}
}

// The PR list and the branch heads are read at once, so a poll takes about
// one round trip, not one per branch.
func TestPendingCommitsReadsConcurrently(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app", 200, map[string]any{"default_branch": "main"})
	var mu sync.Mutex
	inflight, most := 0, 0
	slow := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			inflight++
			most = max(most, inflight)
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			inflight--
			mu.Unlock()
			fmt.Fprint(w, body)
		}
	}
	h.f.on("GET", "/repos/acme/app/pulls?state=open&per_page=100&page=1", slow(`[]`))
	for _, b := range []string{"main", "a", "b", "c"} {
		h.f.on("GET", "/repos/acme/app/branches/"+b, slow(`{"commit":{"sha":"abc"}}`))
	}
	got, err := h.c.PendingCommits(context.Background(), "acme/app", "a", "b", "c")
	if err != nil || len(got) != 4 {
		t.Fatalf("%v, %v", got, err)
	}
	if most < 5 {
		t.Fatalf("at most %d requests in flight; want the PR list and 4 branches (5) at once", most)
	}
	if got[0].Ref+got[1].Ref+got[2].Ref+got[3].Ref != "mainabc" {
		t.Fatalf("order %v", got)
	}
}

func TestFileAtAndListDir(t *testing.T) {
	h := newHarness(t)
	h.f.on("GET", "/repos/acme/app/contents/.github/workflows/ci.yml?ref=abc123", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "raw") {
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, "on: push\n")
	})
	h.f.json("GET", "/repos/acme/app/contents/.github/workflows?ref=abc123", 200, []map[string]any{{"name": "ci.yml", "type": "file"}, {"name": "release.yml", "type": "file"}})
	ctx := context.Background()
	b, err := h.c.FileAt(ctx, "acme/app", "abc123", ".github/workflows/ci.yml")
	if err != nil || string(b) != "on: push\n" {
		t.Fatalf("FileAt = %q, %v", b, err)
	}
	names, err := h.c.ListDir(ctx, "acme/app", "abc123", ".github/workflows")
	if err != nil || fmt.Sprint(names) != "[ci.yml release.yml]" {
		t.Fatalf("ListDir = %v, %v", names, err)
	}
	if _, err := h.c.FileAt(ctx, "acme/app", "abc123", ".github/workflows/none.yml"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := h.c.FileAt(ctx, "acme/app", "abc123", "../../etc/passwd"); err == nil {
		t.Fatal("accepted a path with ..")
	}
	if _, err := h.c.FileAt(ctx, "acme/app", "main;rm", "x"); err == nil {
		t.Fatal("accepted a bad sha")
	}
}

func TestArchiveCapturesRedirect(t *testing.T) {
	h := newHarness(t)
	loc := "https://codeload.example.com/acme/app/legacy.tar.gz/abc123?token=SHORTLIVED"
	h.f.on("GET", "/repos/acme/app/tarball/abc123", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loc, http.StatusFound)
	})
	u, hdr, err := h.c.Archive(context.Background(), "acme/app", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if u != loc || hdr != "" {
		t.Fatalf("got %q %q", u, hdr)
	}
	if strings.Contains(h.logs.String(), "SHORTLIVED") {
		t.Fatal("the archive URL token reached the log")
	}
}

func TestArchiveFallsBackToAPIURL(t *testing.T) {
	h := newHarness(t)
	h.f.on("GET", "/repos/acme/app/tarball/abc123", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "tarball bytes") })
	u, hdr, err := h.c.Archive(context.Background(), "acme/app", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if u != h.f.srv.URL+"/repos/acme/app/tarball/abc123" || hdr != "Authorization: Bearer "+leakMarker {
		t.Fatalf("got %q %q", u, hdr)
	}
	h.noLeak(t)
}

func TestStatusesPaginated(t *testing.T) {
	h := newHarness(t)
	var page1 []map[string]any
	for i := 0; i < perPage; i++ {
		page1 = append(page1, map[string]any{"context": fmt.Sprintf("ci/%d", i), "state": "success"})
	}
	h.f.json("GET", "/repos/acme/app/commits/abc123/status?per_page=100&page=1", 200, map[string]any{"state": "pending", "statuses": page1})
	h.f.json("GET", "/repos/acme/app/commits/abc123/status?per_page=100&page=2", 200, map[string]any{"statuses": []map[string]any{{"context": "tentmaker/test", "state": "pending"}}})
	got, err := h.c.Statuses(context.Background(), "acme/app", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != perPage+1 || got["tentmaker/test"] != "pending" || got["ci/5"] != "success" {
		t.Fatalf("got %d statuses: %v", len(got), got["tentmaker/test"])
	}
}

func TestPostStatus(t *testing.T) {
	h := newHarness(t)
	h.f.json("POST", "/repos/acme/app/statuses/abc123", 201, map[string]any{"id": 1})
	long := strings.Repeat("é", 200)
	if err := h.c.PostStatus(context.Background(), "acme/app", "abc123", "tentmaker/test", "failure", long, "https://example.com/log"); err != nil {
		t.Fatal(err)
	}
	b := h.f.body("POST", "/repos/acme/app/statuses/abc123")
	if b["state"] != "failure" || b["context"] != "tentmaker/test" || b["target_url"] != "https://example.com/log" {
		t.Fatalf("body: %v", b)
	}
	if d := b["description"].(string); len([]rune(d)) != MaxDescription {
		t.Fatalf("description is %d characters, want %d", len([]rune(d)), MaxDescription)
	}
	if err := h.c.PostStatus(context.Background(), "acme/app", "abc123", "tentmaker/test", "done", "", ""); err == nil {
		t.Fatal("accepted a bad state")
	}
	h.f.json("POST", "/repos/acme/app/statuses/def456", 422, map[string]any{"message": "Validation Failed"})
	if err := h.c.PostStatus(context.Background(), "acme/app", "def456", "c", "success", "", ""); statusOf(err) != 422 {
		t.Fatalf("want 422, got %v", err)
	}
	if n := h.f.count("POST", "/repos/acme/app/statuses/def456"); n != 1 {
		t.Fatalf("422 retried: %d", n)
	}
}
