package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

const (
	token   = "glpat-s3cr3t"
	repo    = "group/sub/name"
	encoded = "/api/v4/projects/group%2Fsub%2Fname"
	sha     = "0123456789abcdef0123456789abcdef01234567"
)

// fake routes on the escaped path, so the project id must arrive encoded.
type fake struct {
	t      *testing.T
	routes map[string]http.HandlerFunc
	hits   map[string]*atomic.Int32
}

func newFake(t *testing.T) (*fake, *Client) {
	f := &fake{t: t, routes: map[string]http.HandlerFunc{}, hits: map[string]*atomic.Int32{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"message":"401 Unauthorized for %s"}`, r.Header.Get("PRIVATE-TOKEN"))
			return
		}
		key := r.Method + " " + r.URL.EscapedPath()
		h, ok := f.routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"404 Not Found"}`)
			return
		}
		if f.hits[key] == nil {
			f.hits[key] = &atomic.Int32{}
		}
		f.hits[key].Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{
		BaseURL: srv.URL + "/api/v4/",
		Token:   func(context.Context) (string, error) { return token, nil },
	})
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return f, c
}

func (f *fake) on(method, path string, h http.HandlerFunc) { f.routes[method+" "+path] = h }

func (f *fake) count(method, path string) int32 {
	if c := f.hits[method+" "+path]; c != nil {
		return c.Load()
	}
	return 0
}

func json200(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func TestPendingCommits(t *testing.T) {
	f, c := newFake(t)
	f.on("GET", encoded, json200(`{"id":7,"default_branch":"main"}`))
	f.on("GET", encoded+"/repository/branches/main", json200(`{"name":"main","commit":{"id":"aaa"}}`))
	f.on("GET", encoded+"/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "opened" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"iid":4,"sha":"bbb","source_branch":"feat","source_project_id":7,"target_project_id":7,"author":{"username":"owner"}}]`)
		case "2":
			w.Header().Set("X-Next-Page", "")
			fmt.Fprint(w, `[{"iid":5,"sha":"ccc","source_branch":"patch","source_project_id":99,"target_project_id":7,"author":{"username":"stranger"}},
				{"iid":6,"sha":"","source_branch":"empty","source_project_id":7,"target_project_id":7,"author":{"username":"owner"}}]`)
		default:
			t.Errorf("page %s", r.URL.Query().Get("page"))
		}
	})
	got, err := c.PendingCommits(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Commit{
		{Repo: repo, SHA: "aaa", Ref: "main"},
		{Repo: repo, SHA: "bbb", Ref: "feat", PR: 4, Author: "owner"},
		{Repo: repo, SHA: "ccc", Ref: "patch", PR: 5, Author: "stranger", FromFork: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestFileAtAndListDir(t *testing.T) {
	f, c := newFake(t)
	f.on("GET", encoded+"/repository/files/.github%2Fworkflows%2Fci.yml/raw", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != sha {
			t.Errorf("ref = %s", r.URL.Query().Get("ref"))
		}
		fmt.Fprint(w, "on: push\n")
	})
	b, err := c.FileAt(context.Background(), repo, sha, ".github/workflows/ci.yml")
	if err != nil || string(b) != "on: push\n" {
		t.Fatalf("FileAt = %q, %v", b, err)
	}
	if _, err := c.FileAt(context.Background(), repo, sha, ".gitlab-ci.yml"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	f.on("GET", encoded+"/repository/tree", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("path") != ".github/workflows" || q.Get("ref") != sha {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if q.Get("page") == "1" {
			w.Header().Set("Link", `<http://x/api/v4/projects/1/repository/tree?page=2&per_page=100>; rel="next"`)
			fmt.Fprint(w, `[{"name":"release.yml","type":"blob"}]`)
			return
		}
		fmt.Fprint(w, `[{"name":"ci.yml","type":"blob"}]`)
	})
	names, err := c.ListDir(context.Background(), repo, sha, "/.github/workflows/")
	if err != nil || !reflect.DeepEqual(names, []string{"ci.yml", "release.yml"}) {
		t.Fatalf("ListDir = %v, %v", names, err)
	}
}

func TestArchive(t *testing.T) {
	_, c := newFake(t)
	u, h, err := c.Archive(context.Background(), repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	if pu.EscapedPath() != encoded+"/repository/archive.tar.gz" || pu.Query().Get("sha") != sha {
		t.Fatalf("url = %s", u)
	}
	if h != "PRIVATE-TOKEN: "+token {
		t.Fatalf("header = %q", h)
	}
	if strings.Contains(u, token) {
		t.Fatal("token in the URL")
	}
}

func TestStatuses(t *testing.T) {
	f, c := newFake(t)
	f.on("GET", encoded+"/repository/commits/"+sha+"/statuses", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "" {
			t.Error("Statuses must not filter by ref (GitLab would default to the default branch)")
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"name":"tentmaker/a","status":"running"},{"name":"tentmaker/b","status":"failed"}]`)
			return
		}
		fmt.Fprint(w, `[{"name":"tentmaker/c","status":"success"},{"name":"tentmaker/d","status":"canceled"},{"name":"tentmaker/a","status":"success"}]`)
	})
	got, err := c.Statuses(context.Background(), repo, sha)
	want := map[string]string{"tentmaker/a": "success", "tentmaker/b": "failure", "tentmaker/c": "success", "tentmaker/d": "error"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestPostStatus(t *testing.T) {
	f, c := newFake(t)
	var forms []url.Values
	f.on("POST", encoded+"/statuses/"+sha, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		forms = append(forms, form)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})
	ctx := context.Background()
	for state, want := range map[string]string{"success": "success", "failure": "failed", "error": "failed", "pending": "running"} {
		forms = nil
		if err := c.PostStatus(ctx, repo, sha, "tentmaker/Go", state, "on mac, 12s", "https://example.test/run/1"); err != nil {
			t.Fatal(err)
		}
		f := forms[0]
		if f.Get("state") != want || f.Get("name") != "tentmaker/Go" || f.Get("description") != "on mac, 12s" || f.Get("target_url") != "https://example.test/run/1" {
			t.Fatalf("%s: form %v", state, f)
		}
	}
	forms = nil
	long := strings.Repeat("é", 300)
	if err := c.PostStatus(ctx, repo, sha, "tentmaker/Go", "success", long, ""); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(forms[0].Get("description"))); n != 255 || forms[0].Has("target_url") {
		t.Fatalf("description %d runes, form %v", n, forms[0])
	}
	if err := c.PostStatus(ctx, repo, sha, "x", "weird", "", ""); err == nil {
		t.Fatal("unknown state accepted")
	}
}

func TestPostStatusSameStateIsFine(t *testing.T) {
	f, c := newFake(t)
	f.on("POST", encoded+"/statuses/"+sha, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"Cannot transition status via :run from :running"}`)
	})
	if err := c.PostStatus(context.Background(), repo, sha, "tentmaker/Go", "pending", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestRetries(t *testing.T) {
	f, c := newFake(t)
	path := encoded
	var n atomic.Int32
	f.on("GET", path, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"id":7,"default_branch":""}`)
	})
	f.on("GET", path+"/merge_requests", json200(`[]`))
	if _, err := c.PendingCommits(context.Background(), repo); err != nil {
		t.Fatalf("after two 502s: %v", err)
	}
	if got := f.count("GET", path); got != 3 {
		t.Fatalf("tries = %d", got)
	}

	f2, c2 := newFake(t)
	f2.on("GET", encoded+"/repository/files/a/raw", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c2.FileAt(context.Background(), repo, sha, "a"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	if got := f2.count("GET", encoded+"/repository/files/a/raw"); got != 3 {
		t.Fatalf("tries = %d", got)
	}

	f3, c3 := newFake(t)
	f3.on("GET", encoded+"/repository/files/a/raw", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"403 Forbidden"}`)
	})
	if _, err := c3.FileAt(context.Background(), repo, sha, "a"); err == nil {
		t.Fatal("no error")
	}
	if got := f3.count("GET", encoded+"/repository/files/a/raw"); got != 1 {
		t.Fatalf("a 403 was retried: %d tries", got)
	}

	f4, c4 := newFake(t)
	var waited []time.Duration
	c4.sleep = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }
	var m atomic.Int32
	f4.on("GET", encoded+"/repository/files/a/raw", func(w http.ResponseWriter, r *http.Request) {
		if m.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "ok")
	})
	if b, err := c4.FileAt(context.Background(), repo, sha, "a"); err != nil || string(b) != "ok" {
		t.Fatalf("after 429: %q %v", b, err)
	}
	if len(waited) != 1 || waited[0] != 7*time.Second {
		t.Fatalf("waited %v", waited)
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	_, good := newFake(t)
	bad := New(Options{
		BaseURL: good.base,
		Token:   func(context.Context) (string, error) { return "wrong-" + token, nil },
	})
	_, err := bad.FileAt(context.Background(), repo, sha, "a")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token in error: %v", err)
	}
	failing := New(Options{BaseURL: good.base, Token: func(context.Context) (string, error) {
		return "", errors.New("keychain says " + token)
	}})
	if _, err := failing.FileAt(context.Background(), repo, sha, "a"); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := failing.Archive(context.Background(), repo, sha); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaults(t *testing.T) {
	c := New(Options{})
	if c.base != defaultBaseURL || c.http.Timeout != 30*time.Second || c.Name() != "gitlab" {
		t.Fatalf("defaults: %+v", c)
	}
	u, h, err := c.Archive(context.Background(), "a/b", "s")
	if err != nil || h != "" || u != "https://gitlab.com/api/v4/projects/a%2Fb/repository/archive.tar.gz?sha=s" {
		t.Fatalf("public archive: %q %q %v", u, h, err)
	}
}
