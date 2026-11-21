package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// leakMarker is the fake credential the client is given. It is unique, so
// any error or log line that contains it is a leak.
const leakMarker = "tm-fake-cred-0123456789abcdefABCDEF"

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// fakeGH is a scripted GitHub API. Routes match "METHOD /path?query"
// exactly; a route that isn't scripted is a 404.
type fakeGH struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	hits   map[string]int
	bodies map[string][]byte
	inm    map[string][]string // If-None-Match values seen per route
}

func newFake(t *testing.T) *fakeGH {
	f := &fakeGH{t: t, routes: map[string]http.HandlerFunc{}, hits: map[string]int{}, bodies: map[string][]byte{}, inm: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGH) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.RequestURI()
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.hits[key]++
	f.bodies[key] = body
	if v := r.Header.Get("If-None-Match"); v != "" {
		f.inm[key] = append(f.inm[key], v)
	}
	h := f.routes[key]
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+leakMarker {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
		return
	}
	if h == nil {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	}
	h(w, r)
}

func (f *fakeGH) on(method, uri string, h http.HandlerFunc) {
	f.mu.Lock()
	f.routes[method+" "+uri] = h
	f.mu.Unlock()
}

func (f *fakeGH) json(method, uri string, status int, v any) {
	f.on(method, uri, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	})
}

// etagJSON serves v with an ETag and answers a matching If-None-Match with 304.
func (f *fakeGH) etagJSON(uri, etag string, v any) {
	f.on("GET", uri, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_ = json.NewEncoder(w).Encode(v)
	})
}

func (f *fakeGH) count(method, uri string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[method+" "+uri]
}

func (f *fakeGH) body(method, uri string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(f.bodies[method+" "+uri], &m)
	return m
}

type harness struct {
	c      *Client
	f      *fakeGH
	logs   *bytes.Buffer
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newHarness(t *testing.T) *harness {
	h := &harness{f: newFake(t), logs: &bytes.Buffer{}, now: t0}
	h.c = New(Options{
		BaseURL: h.f.srv.URL,
		Token:   func(context.Context) (string, error) { return leakMarker, nil },
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.now
		},
		Log: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	h.c.sleep = func(ctx context.Context, d time.Duration) error {
		h.mu.Lock()
		h.sleeps = append(h.sleeps, d)
		h.now = h.now.Add(d)
		h.mu.Unlock()
		return ctx.Err()
	}
	h.c.backoff = func(try int) time.Duration { return time.Duration(try+1) * time.Second }
	return h
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *harness) slept() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.sleeps...)
}

func (h *harness) noLeak(t *testing.T, errs ...error) {
	t.Helper()
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), leakMarker) {
			t.Fatalf("error leaks the credential: %v", err)
		}
	}
	if strings.Contains(h.logs.String(), leakMarker) {
		t.Fatalf("log leaks the credential:\n%s", h.logs.String())
	}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func mkRun(id int64, head string, created time.Time, updated string) map[string]any {
	r := map[string]any{
		"id": id, "name": "CI", "path": ".github/workflows/ci.yml", "event": "pull_request",
		"head_branch": "feature", "created_at": ts(created), "run_started_at": ts(created), "updated_at": updated,
		"repository": map[string]any{"full_name": "acme/app"},
	}
	if head != "" {
		r["head_repository"] = map[string]any{"full_name": head}
	}
	return r
}

func mkJob(id, runID int64, status string, created time.Time, labels ...string) map[string]any {
	return map[string]any{"id": id, "run_id": runID, "name": fmt.Sprintf("job-%d", id), "status": status, "labels": labels, "created_at": ts(created)}
}

func runList(rs ...map[string]any) map[string]any {
	if rs == nil {
		rs = []map[string]any{}
	}
	return map[string]any{"total_count": len(rs), "workflow_runs": rs}
}

func jobList(js ...map[string]any) map[string]any {
	if js == nil {
		js = []map[string]any{}
	}
	return map[string]any{"total_count": len(js), "jobs": js}
}

const (
	queuedURI     = "/repos/acme/app/actions/runs?status=queued&per_page=100&page=1"
	inProgressURI = "/repos/acme/app/actions/runs?status=in_progress&per_page=100&page=1"
)

func jobsURI(runID int64) string {
	return fmt.Sprintf("/repos/acme/app/actions/runs/%d/jobs?filter=latest&per_page=100&page=1", runID)
}
