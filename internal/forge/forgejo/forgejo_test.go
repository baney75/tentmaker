package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// leakMarker is the fake credential the client is given; any error or log
// line containing it is a leak.
const leakMarker = "tm-fake-forgejo-cred-0123456789abcdef"

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type fake struct {
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	hits   map[string]int
	bodies map[string][]byte
}

func newFake(t *testing.T) *fake {
	f := &fake{routes: map[string]http.HandlerFunc{}, hits: map[string]int{}, bodies: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits[key]++
		f.bodies[key] = b
		h := f.routes[key]
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "token "+leakMarker {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"token is required"}`)
			return
		}
		if h == nil {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"The target couldn't be found."}`)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) on(method, uri string, h http.HandlerFunc) {
	f.mu.Lock()
	f.routes[method+" /api/v1"+uri] = h
	f.mu.Unlock()
}

func (f *fake) json(method, uri string, status int, v any) {
	f.on(method, uri, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	})
}

func (f *fake) count(method, uri string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[method+" /api/v1"+uri]
}

func (f *fake) body(method, uri string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(f.bodies[method+" /api/v1"+uri], &m)
	return m
}

type harness struct {
	c      *Client
	f      *fake
	logs   *bytes.Buffer
	sleeps []time.Duration
}

func newHarness(t *testing.T, flavor string) *harness {
	h := &harness{f: newFake(t), logs: &bytes.Buffer{}}
	h.c = New(Options{
		BaseURL: h.f.srv.URL + "/",
		Token:   func(context.Context) (string, error) { return leakMarker, nil },
		Now:     func() time.Time { return t0 },
		Log:     slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Flavor:  flavor,
	})
	h.c.sleep = func(ctx context.Context, d time.Duration) error { h.sleeps = append(h.sleeps, d); return nil }
	h.c.backoff = func(try int) time.Duration { return time.Duration(try+1) * time.Second }
	return h
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

func TestFlavorDetection(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"11.0.3+gitea-1.22.0", Forgejo},
		{"17.0.0-121-39320c0+gitea-1.22.0", Forgejo},
		{"1.25.1", Gitea},
	} {
		h := newHarness(t, "")
		h.f.json("GET", "/version", 200, map[string]any{"version": tc.version})
		got, err := h.c.Flavor(context.Background())
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %q, %v", tc.version, got, err)
		}
		_, _ = h.c.Flavor(context.Background())
		if h.f.count("GET", "/version") != 1 {
			t.Fatal("version asked twice")
		}
	}
	h := newHarness(t, "")
	if _, err := h.c.Flavor(context.Background()); err == nil {
		t.Fatal("want an error when the version is unreadable (fail closed)")
	}
}

func forgejoJobsFixture(h *harness) {
	h.f.json("GET", "/repos/acme/app/actions/runners/jobs", 200, []map[string]any{
		{"id": 11, "run_id": 1, "name": "test", "runs_on": []string{"self-hosted", "tentmaker", "linux"}, "status": "waiting"},
		{"id": 12, "run_id": 2, "name": "fork", "runs_on": []string{"tentmaker"}, "status": "waiting"},
		{"id": 13, "run_id": 3, "name": "old", "runs_on": []string{"tentmaker"}, "status": "waiting"},
		{"id": 14, "run_id": 1, "name": "build", "runs_on": []string{"tentmaker"}, "status": "running"},
	})
	h.f.json("GET", "/repos/acme/app/actions/runs/1", 200, map[string]any{"id": 1, "event": "push", "trigger_event": "push", "prettyref": "main", "workflow_id": "ci.yml", "created": ts(t0.Add(-time.Minute)), "is_fork_pull_request": false})
	h.f.json("GET", "/repos/acme/app/actions/runs/2", 200, map[string]any{"id": 2, "event": "pull_request", "prettyref": "#4", "workflow_id": "ci.yml", "created": ts(t0.Add(-5 * time.Minute)), "is_fork_pull_request": true})
	h.f.json("GET", "/repos/acme/app/actions/runs/3", 200, map[string]any{"id": 3, "event": "push", "created": ts(t0.Add(-25 * time.Hour))})
}

func TestForgejoQueuedAndRunningJobs(t *testing.T) {
	h := newHarness(t, Forgejo)
	forgejoJobsFixture(h)
	ctx := context.Background()
	got, err := h.c.QueuedJobs(ctx, "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 12 || got[1].ID != 11 {
		t.Fatalf("want [12 11] oldest first without the 25 h old run, got %+v", got)
	}
	if !got[0].FromFork || got[1].FromFork {
		t.Fatalf("fork flags: %+v", got)
	}
	j := got[1]
	if j.Forge != "forgejo" || j.Status != "queued" || j.Event != "push" || j.Workflow != "ci.yml" || j.HeadBranch != "main" || len(j.Labels) != 3 || j.RunID != 1 {
		t.Fatalf("job fields: %+v", j)
	}
	if _, err := h.c.QueuedJobs(ctx, "acme/app"); err != nil {
		t.Fatal(err)
	}
	if n := h.f.count("GET", "/repos/acme/app/actions/runs/1"); n != 1 {
		t.Fatalf("run details fetched %d times, want once (cached)", n)
	}
	running, err := h.c.RunningJobs(ctx, "acme/app")
	if err != nil || len(running) != 1 || running[0].ID != 14 || running[0].Status != "in_progress" {
		t.Fatalf("running: %+v, %v", running, err)
	}
	h.noLeak(t)
}

func TestForgejoOldVersionUnsupported(t *testing.T) {
	h := newHarness(t, Forgejo)
	_, err := h.c.QueuedJobs(context.Background(), "acme/app")
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestGiteaQueuedJobsForkRules(t *testing.T) {
	h := newHarness(t, Gitea)
	h.f.json("GET", "/repos/acme/app/actions/jobs?status=queued&limit=50&page=1", 200, map[string]any{"jobs": []map[string]any{
		{"id": 21, "run_id": 1, "name": "push", "labels": []string{"tentmaker"}, "status": "queued"},
		{"id": 22, "run_id": 2, "name": "pr-unknown", "labels": []string{"tentmaker"}, "status": "queued"},
		{"id": 23, "run_id": 3, "name": "pr-same", "labels": []string{"tentmaker"}, "status": "queued"},
		{"id": 24, "run_id": 4, "name": "pr-fork", "labels": []string{"tentmaker"}, "status": "queued"},
	}})
	h.f.json("GET", "/repos/acme/app/actions/runs/1", 200, map[string]any{"event": "push", "created_at": ts(t0.Add(-4 * time.Minute))})
	h.f.json("GET", "/repos/acme/app/actions/runs/2", 200, map[string]any{"event": "pull_request", "created_at": ts(t0.Add(-3 * time.Minute))})
	h.f.json("GET", "/repos/acme/app/actions/runs/3", 200, map[string]any{"event": "pull_request", "created_at": ts(t0.Add(-2 * time.Minute)),
		"pull_requests": []map[string]any{{"head": map[string]any{"repo": map[string]any{"id": 9}}, "base": map[string]any{"repo": map[string]any{"id": 9}}}}})
	h.f.json("GET", "/repos/acme/app/actions/runs/4", 200, map[string]any{"event": "pull_request", "created_at": ts(t0.Add(-1 * time.Minute)),
		"head_repository": map[string]any{"full_name": "evil/app"}})
	got, err := h.c.QueuedJobs(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	fork := map[int64]bool{}
	for _, j := range got {
		fork[j.ID] = j.FromFork
	}
	if len(got) != 4 || fork[21] || !fork[22] || fork[23] || !fork[24] {
		t.Fatalf("fork rules: %v", fork)
	}
}

func TestForgejoGrantEphemeralRunner(t *testing.T) {
	h := newHarness(t, Forgejo)
	h.f.json("POST", "/repos/acme/app/actions/runners", 201, map[string]any{"id": 77, "uuid": "0b9f6a5e-1111-4c1e-9f00-123456789abc", "token": "runner-cred-xyz"})
	g, err := h.c.Grant(context.Background(), "acme/app", "tm-box-1", []string{"self-hosted", "tentmaker", "linux", "custom:docker://node:20"})
	if err != nil {
		t.Fatal(err)
	}
	if g.RunnerID != 77 || g.RunnerName != "tm-box-1" || g.Forge != "forgejo" || !g.ExpiresAt.After(t0) {
		t.Fatalf("grant: %+v", g)
	}
	var reg Registration
	if err := json.Unmarshal(g.Secret, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.UUID == "" || reg.Token != "runner-cred-xyz" || reg.ID != 77 || reg.Address != h.f.srv.URL || reg.Name != "tm-box-1" {
		t.Fatalf("registration: %+v", reg)
	}
	if fmt.Sprint(reg.Labels) != "[self-hosted:host tentmaker:host linux:host custom:docker://node:20]" {
		t.Fatalf("labels: %v", reg.Labels)
	}
	if b := h.f.body("POST", "/repos/acme/app/actions/runners"); b["ephemeral"] != true || b["name"] != "tm-box-1" {
		t.Fatalf("request body: %v", b)
	}
	if strings.Contains(h.logs.String(), "runner-cred-xyz") {
		t.Fatal("the runner credential reached the log")
	}
}

func TestGrantUnsupported(t *testing.T) {
	h := newHarness(t, Forgejo) // Forgejo before 15: no POST runners
	_, err := h.c.Grant(context.Background(), "acme/app", "tm-box-1", nil)
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "checks") {
		t.Fatalf("old Forgejo: %v", err)
	}
	g := newHarness(t, Gitea)
	_, err = g.c.Grant(context.Background(), "acme/app", "tm-box-1", nil)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Gitea: %v", err)
	}
	if g.f.count("POST", "/repos/acme/app/actions/runners/registration-token") != 0 {
		t.Fatal("Gitea grant asked for a reusable registration token")
	}
}

func TestRunnerStatus(t *testing.T) {
	h := newHarness(t, Forgejo)
	h.f.json("GET", "/repos/acme/app/actions/runners/5", 200, map[string]any{"id": 5, "status": "active"})
	h.f.json("GET", "/repos/acme/app/actions/runners/6", 200, map[string]any{"id": 6, "status": "idle"})
	h.f.json("GET", "/repos/acme/app/actions/runners/7", 200, map[string]any{"id": 7, "status": "offline"})
	ctx := context.Background()
	for id, want := range map[int64]core.RunnerStatus{
		5: {Exists: true, Busy: true, Status: "online"},
		6: {Exists: true, Status: "online"},
		7: {Exists: true, Status: "offline"},
		8: {Exists: false},
	} {
		got, err := h.c.Runner(ctx, "acme/app", id)
		if err != nil || got != want {
			t.Fatalf("runner %d: got %+v, %v", id, got, err)
		}
	}
}

func TestJobByID(t *testing.T) {
	h := newHarness(t, Forgejo)
	forgejoJobsFixture(h)
	h.f.json("GET", "/repos/acme/app/actions/jobs/99", 200, map[string]any{"id": 99, "run_id": 1, "name": "done", "status": "success"})
	ctx := context.Background()
	j, err := h.c.JobByID(ctx, "acme/app", 14)
	if err != nil || j.Status != "in_progress" || j.Name != "build" {
		t.Fatalf("from list: %+v, %v", j, err)
	}
	j, err = h.c.JobByID(ctx, "acme/app", 99)
	if err != nil || j.Status != "completed" {
		t.Fatalf("fallback: %+v, %v", j, err)
	}
	if _, err := h.c.JobByID(ctx, "acme/app", 100); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestDeleteAndCancel(t *testing.T) {
	h := newHarness(t, Forgejo)
	h.f.on("DELETE", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h.f.on("POST", "/repos/acme/app/actions/runs/1/cancel", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h.f.json("POST", "/repos/acme/app/actions/runs/2/cancel", 409, map[string]any{"message": "done"})
	h.f.json("POST", "/repos/acme/app/actions/runs/3/cancel", 405, map[string]any{})
	ctx := context.Background()
	if err := h.c.DeleteRunner(ctx, "acme/app", 5); err != nil {
		t.Fatal(err)
	}
	if err := h.c.DeleteRunner(ctx, "acme/app", 6); err != nil {
		t.Fatalf("missing runner: %v", err)
	}
	if err := h.c.CancelRun(ctx, "acme/app", 1); err != nil {
		t.Fatal(err)
	}
	if err := h.c.CancelRun(ctx, "acme/app", 2); err != nil {
		t.Fatalf("409: %v", err)
	}
	if err := h.c.CancelRun(ctx, "acme/app", 3); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("405: %v", err)
	}
	if err := h.c.CancelRun(ctx, "acme/app", 4); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
}

func TestCheckRepo(t *testing.T) {
	h := newHarness(t, Forgejo)
	h.f.json("GET", "/repos/acme/app", 200, map[string]any{"private": false, "default_branch": "main", "has_actions": true, "permissions": map[string]any{"admin": true}})
	h.f.json("GET", "/repos/acme/app/actions/runners", 200, []any{})
	h.f.json("GET", "/repos/acme/app/actions/runners/jobs", 200, []any{})
	info, err := h.c.CheckRepo(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Public || info.DefaultBranch != "main" || info.ForkPolicy != "approve_first_time" || len(info.Problems) != 0 {
		t.Fatalf("got %+v", info)
	}
	old := newHarness(t, Forgejo)
	old.f.json("GET", "/repos/acme/app", 200, map[string]any{"private": true, "default_branch": "main", "has_actions": false, "permissions": map[string]any{"admin": false}})
	old.f.json("GET", "/repos/acme/app/actions/runners/jobs", 200, []any{})
	info, err = old.c.CheckRepo(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if info.Public || len(info.Problems) != 3 || !strings.Contains(strings.Join(info.Problems, "\n"), "Forgejo 15") {
		t.Fatalf("old Forgejo: %+v", info)
	}
	g := newHarness(t, Gitea)
	g.f.json("GET", "/repos/acme/app", 200, map[string]any{"private": false, "default_branch": "main", "permissions": map[string]any{"admin": true}})
	info, err = g.c.CheckRepo(context.Background(), "acme/app")
	if err != nil || len(info.Problems) != 1 || !strings.Contains(info.Problems[0], `mode = "checks"`) {
		t.Fatalf("Gitea: %+v, %v", info, err)
	}
}

func TestStatusForge(t *testing.T) {
	h := newHarness(t, Gitea)
	ctx := context.Background()
	h.f.json("GET", "/repos/acme/app", 200, map[string]any{"default_branch": "main"})
	h.f.json("GET", "/repos/acme/app/pulls?state=open&limit=50&page=1", 200, []map[string]any{
		{"number": 3, "user": map[string]any{"login": "owner"}, "head": map[string]any{"ref": "feat", "sha": "aaa", "repo": map[string]any{"full_name": "acme/app"}}},
		{"number": 4, "user": map[string]any{"login": "x"}, "head": map[string]any{"ref": "main", "sha": "bbb", "repo": map[string]any{"full_name": "x/app"}}},
	})
	h.f.json("GET", "/repos/acme/app/branches/main", 200, map[string]any{"commit": map[string]any{"id": "ccc", "author": map[string]any{"username": "owner"}}})
	commits, err := h.c.PendingCommits(ctx, "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Commit{
		{Repo: "acme/app", SHA: "aaa", Ref: "feat", PR: 3, Author: "owner"},
		{Repo: "acme/app", SHA: "bbb", Ref: "main", PR: 4, Author: "x", FromFork: true},
		{Repo: "acme/app", SHA: "ccc", Ref: "main", Author: "owner"},
	}
	if fmt.Sprint(commits) != fmt.Sprint(want) {
		t.Fatalf("got %+v", commits)
	}

	h.f.on("GET", "/repos/acme/app/raw/.forgejo/workflows/ci.yml?ref=abc123", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "on: push\n") })
	b, err := h.c.FileAt(ctx, "acme/app", "abc123", ".forgejo/workflows/ci.yml")
	if err != nil || string(b) != "on: push\n" {
		t.Fatalf("FileAt: %q, %v", b, err)
	}
	if _, err := h.c.FileAt(ctx, "acme/app", "abc123", "missing.yml"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	h.f.json("GET", "/repos/acme/app/contents/.forgejo/workflows?ref=abc123", 200, []map[string]any{{"name": "ci.yml"}, {"name": "lint.yaml"}})
	names, err := h.c.ListDir(ctx, "acme/app", "abc123", ".forgejo/workflows")
	if err != nil || fmt.Sprint(names) != "[ci.yml lint.yaml]" {
		t.Fatalf("ListDir: %v, %v", names, err)
	}

	u, hdr, err := h.c.Archive(ctx, "acme/app", "abc123")
	if err != nil || u != h.f.srv.URL+"/api/v1/repos/acme/app/archive/abc123.tar.gz" || hdr != "Authorization: token "+leakMarker {
		t.Fatalf("Archive: %q %q %v", u, hdr, err)
	}

	var full []map[string]any
	for i := 0; i < pageSize; i++ {
		full = append(full, map[string]any{"context": fmt.Sprintf("ci/%d", i), "status": "success"})
	}
	h.f.json("GET", "/repos/acme/app/commits/abc123/status?limit=50&page=1", 200, map[string]any{"statuses": full})
	h.f.json("GET", "/repos/acme/app/commits/abc123/status?limit=50&page=2", 200, map[string]any{"statuses": []map[string]any{{"context": "tentmaker/test", "status": "pending"}}})
	st, err := h.c.Statuses(ctx, "acme/app", "abc123")
	if err != nil || len(st) != pageSize+1 || st["tentmaker/test"] != "pending" {
		t.Fatalf("Statuses: %d, %v", len(st), err)
	}

	h.f.json("POST", "/repos/acme/app/statuses/abc123", 201, map[string]any{"id": 1})
	if err := h.c.PostStatus(ctx, "acme/app", "abc123", "tentmaker/test", "success", strings.Repeat("x", 300), ""); err != nil {
		t.Fatal(err)
	}
	body := h.f.body("POST", "/repos/acme/app/statuses/abc123")
	if body["state"] != "success" || body["context"] != "tentmaker/test" || len(body["description"].(string)) != MaxDescription {
		t.Fatalf("status body: %v", body)
	}
	if _, has := body["target_url"]; has {
		t.Fatal("sent an empty target_url")
	}
	h.noLeak(t)
}

func TestRetriesAndLeaks(t *testing.T) {
	h := newHarness(t, Forgejo)
	n := 0
	h.f.on("GET", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(502)
			return
		}
		fmt.Fprint(w, `{"status":"idle"}`)
	})
	if st, err := h.c.Runner(context.Background(), "acme/app", 5); err != nil || !st.Exists {
		t.Fatalf("after 502s: %+v, %v", st, err)
	}
	if n != 3 || fmt.Sprint(h.sleeps) != "[1s 2s]" {
		t.Fatalf("tries=%d sleeps=%v", n, h.sleeps)
	}
	_, err := h.c.JobByID(context.Background(), "acme/app", 1)
	if h.f.count("GET", "/repos/acme/app/actions/jobs/1") != 1 {
		t.Fatal("404 retried")
	}
	h.f.on("GET", "/repos/acme/app/actions/runners/9", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprintf(w, `{"message":"echo %s"}`, r.Header.Get("Authorization"))
	})
	_, e2 := h.c.Runner(context.Background(), "acme/app", 9)
	if e2 == nil || !strings.Contains(e2.Error(), "[redacted]") {
		t.Fatalf("echo not redacted: %v", e2)
	}
	h.f.srv.Close()
	_, e3 := h.c.Runner(context.Background(), "acme/app", 5)
	if e3 == nil {
		t.Fatal("want a network error")
	}
	h.noLeak(t, err, e2, e3)
}
