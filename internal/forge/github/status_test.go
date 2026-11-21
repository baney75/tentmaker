package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
