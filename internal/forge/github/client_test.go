package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func TestQueuedJobsForkDetectionAndOrder(t *testing.T) {
	h := newHarness(t)
	h.f.etagJSON(queuedURI, `"q1"`, runList(
		mkRun(10, "acme/app", t0.Add(-5*time.Minute), "u1"),
		mkRun(11, "evil/app", t0.Add(-10*time.Minute), "u1"),
		mkRun(12, "ACME/App", t0.Add(-1*time.Minute), "u1"),
		mkRun(13, "", t0.Add(-2*time.Minute), "u1"), // deleted fork: no head repo
	))
	h.f.etagJSON(inProgressURI, `"p1"`, runList())
	h.f.etagJSON(jobsURI(10), `"j10"`, jobList(mkJob(100, 10, "queued", t0.Add(-5*time.Minute), "self-hosted", "tentmaker", "linux"), mkJob(101, 10, "completed", t0)))
	h.f.etagJSON(jobsURI(11), `"j11"`, jobList(mkJob(110, 11, "queued", t0.Add(-10*time.Minute), "self-hosted", "tentmaker")))
	h.f.etagJSON(jobsURI(12), `"j12"`, jobList(mkJob(120, 12, "queued", t0.Add(-1*time.Minute))))
	h.f.etagJSON(jobsURI(13), `"j13"`, jobList(mkJob(130, 13, "queued", t0.Add(-2*time.Minute))))
	got, err := h.c.QueuedJobs(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	fork := map[int64]bool{}
	for _, j := range got {
		ids = append(ids, j.ID)
		fork[j.ID] = j.FromFork
	}
	if fmt.Sprint(ids) != "[110 100 130 120]" {
		t.Fatalf("want oldest first [110 100 130 120], got %v", ids)
	}
	if fork[100] || fork[120] || !fork[110] || !fork[130] {
		t.Fatalf("fork detection wrong: %v", fork)
	}
	j := got[1]
	if j.Forge != "github" || j.Repo != "acme/app" || j.RunID != 10 || j.Workflow != ".github/workflows/ci.yml" ||
		j.Event != "pull_request" || j.HeadBranch != "feature" || j.Status != "queued" || len(j.Labels) != 3 {
		t.Fatalf("job fields: %+v", j)
	}
	h.noLeak(t)
}

func TestETag304ReusesCachedBody(t *testing.T) {
	h := newHarness(t)
	h.f.etagJSON(queuedURI, `"q1"`, runList(mkRun(10, "acme/app", t0.Add(-time.Minute), "u1")))
	h.f.etagJSON(inProgressURI, `"p1"`, runList())
	h.f.etagJSON(jobsURI(10), `"j10"`, jobList(mkJob(100, 10, "queued", t0)))
	for i := 0; i < 3; i++ {
		got, err := h.c.QueuedJobs(context.Background(), "acme/app")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != 100 {
			t.Fatalf("pass %d: got %+v", i, got)
		}
	}
	if n := len(h.f.inm["GET "+queuedURI]); n != 2 {
		t.Fatalf("want 2 conditional requests after the first, got %d", n)
	}
	if v := h.f.inm["GET "+jobsURI(10)]; len(v) != 2 || v[0] != `"j10"` {
		t.Fatalf("jobs list not conditional: %v", v)
	}
}

func TestOldRunsIgnored(t *testing.T) {
	h := newHarness(t)
	h.f.etagJSON(queuedURI, `"q"`, runList(
		mkRun(1, "acme/app", t0.Add(-25*time.Hour), "u"),
		mkRun(2, "acme/app", t0.Add(-23*time.Hour), "u"),
	))
	h.f.etagJSON(inProgressURI, `"p"`, runList())
	h.f.etagJSON(jobsURI(1), `"a"`, jobList(mkJob(10, 1, "queued", t0)))
	h.f.etagJSON(jobsURI(2), `"b"`, jobList(mkJob(20, 2, "queued", t0)))
	got, err := h.c.QueuedJobs(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 20 {
		t.Fatalf("got %+v", got)
	}
	if h.f.count("GET", jobsURI(1)) != 0 {
		t.Fatal("fetched jobs of a run older than 24 h")
	}
}

func TestInProgressJobsThrottled(t *testing.T) {
	h := newHarness(t)
	updated := "u1"
	h.f.etagJSON(queuedURI, `"q"`, runList())
	h.f.on("GET", inProgressURI, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"workflow_runs":[{"id":7,"head_repository":{"full_name":"acme/app"},"created_at":%q,"updated_at":%q}]}`, ts(t0), updated)
	})
	h.f.json("GET", jobsURI(7), 200, jobList(mkJob(70, 7, "in_progress", t0), mkJob(71, 7, "queued", t0)))
	poll := func() {
		t.Helper()
		got, err := h.c.QueuedJobs(context.Background(), "acme/app")
		if err != nil || len(got) != 1 || got[0].ID != 71 {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	poll()
	h.advance(5 * time.Second)
	poll()
	if n := h.f.count("GET", jobsURI(7)); n != 1 {
		t.Fatalf("refetched within 15 s with the run unchanged: %d fetches", n)
	}
	updated = "u2"
	poll()
	if n := h.f.count("GET", jobsURI(7)); n != 2 {
		t.Fatalf("run changed but jobs not refetched: %d", n)
	}
	h.advance(16 * time.Second)
	poll()
	if n := h.f.count("GET", jobsURI(7)); n != 3 {
		t.Fatalf("not refetched after 15 s: %d", n)
	}
}

func TestPagination(t *testing.T) {
	h := newHarness(t)
	var full []map[string]any
	for i := 0; i < perPage; i++ {
		full = append(full, mkRun(int64(1000+i), "acme/app", t0.Add(-25*time.Hour), "u"))
	}
	h.f.json("GET", queuedURI, 200, runList(full...))
	h.f.json("GET", "/repos/acme/app/actions/runs?status=queued&per_page=100&page=2", 200, runList(mkRun(5, "acme/app", t0, "u")))
	h.f.json("GET", inProgressURI, 200, runList())
	h.f.json("GET", jobsURI(5), 200, jobList(mkJob(50, 5, "queued", t0)))
	got, err := h.c.QueuedJobs(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 50 {
		t.Fatalf("second page not read: %+v", got)
	}
	if h.f.count("GET", "/repos/acme/app/actions/runs?status=queued&per_page=100&page=3") != 0 {
		t.Fatal("read past a short page")
	}
}

func TestPaginationTruncatedFails(t *testing.T) {
	h := newHarness(t)
	var full []map[string]any
	for i := 0; i < perPage; i++ {
		full = append(full, map[string]any{"context": "c" + strconv.Itoa(i), "state": "success"})
	}
	for p := 1; p <= maxPages; p++ {
		h.f.json("GET", fmt.Sprintf("/repos/acme/app/commits/abc123/status?per_page=100&page=%d", p), 200, map[string]any{"statuses": full})
	}
	_, err := h.c.Statuses(context.Background(), "acme/app", "abc123")
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("want a truncation error, got %v", err)
	}
}

func TestRateLimitLowWaitsForReset(t *testing.T) {
	h := newHarness(t)
	reset := t0.Add(30 * time.Second)
	h.f.on("GET", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "150")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		fmt.Fprint(w, `{"status":"online","busy":true}`)
	})
	if _, err := h.c.Runner(context.Background(), "acme/app", 5); err != nil {
		t.Fatal(err)
	}
	if len(h.slept()) != 0 {
		t.Fatal("slept before the limit was known")
	}
	st, err := h.c.Runner(context.Background(), "acme/app", 5)
	if err != nil || !st.Busy || !st.Exists {
		t.Fatalf("got %+v, %v", st, err)
	}
	if s := h.slept(); len(s) != 1 || s[0] != 30*time.Second {
		t.Fatalf("want one 30 s wait for the reset, got %v", s)
	}
}

func TestRateLimitWaitHonorsContext(t *testing.T) {
	h := newHarness(t)
	h.c.sleep = sleepCtx // real sleeper
	h.c.rateLeft, h.c.rateSet = 10, t0.Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := h.c.Runner(ctx, "acme/app", 5)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not return promptly")
	}
	if h.f.count("GET", "/repos/acme/app/actions/runners/5") != 0 {
		t.Fatal("sent a request while the rate limit was spent")
	}
}

func TestRateLimited403RetriedAfterReset(t *testing.T) {
	h := newHarness(t)
	n := 0
	h.f.on("GET", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(t0.Add(42*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"status":"online"}`)
	})
	st, err := h.c.Runner(context.Background(), "acme/app", 5)
	if err != nil || !st.Exists {
		t.Fatalf("got %+v, %v", st, err)
	}
	if s := h.slept(); len(s) == 0 || s[0] != 42*time.Second {
		t.Fatalf("want a 42 s wait, got %v", s)
	}
}

func TestPlain403NotRetried(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app/actions/runners/5", 403, map[string]any{"message": "Resource not accessible by integration"})
	_, err := h.c.Runner(context.Background(), "acme/app", 5)
	if statusOf(err) != 403 {
		t.Fatalf("want a 403, got %v", err)
	}
	if n := h.f.count("GET", "/repos/acme/app/actions/runners/5"); n != 1 {
		t.Fatalf("a plain 403 was retried: %d tries", n)
	}
}

func TestRetryOn502(t *testing.T) {
	h := newHarness(t)
	n := 0
	h.f.on("GET", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"status":"offline","busy":false}`)
	})
	st, err := h.c.Runner(context.Background(), "acme/app", 5)
	if err != nil || st.Status != "offline" {
		t.Fatalf("got %+v, %v", st, err)
	}
	if n != 3 {
		t.Fatalf("want 3 tries, got %d", n)
	}
	if s := h.slept(); fmt.Sprint(s) != "[1s 2s]" {
		t.Fatalf("want backoff [1s 2s], got %v", s)
	}
}

func TestRetryGivesUpAfterThreeTries(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app/actions/runners/5", 503, map[string]any{"message": "unavailable"})
	_, err := h.c.Runner(context.Background(), "acme/app", 5)
	if statusOf(err) != 503 {
		t.Fatalf("want 503, got %v", err)
	}
	if n := h.f.count("GET", "/repos/acme/app/actions/runners/5"); n != Tries {
		t.Fatalf("want %d tries, got %d", Tries, n)
	}
}

func TestRetryOnNetworkError(t *testing.T) {
	h := newHarness(t)
	h.f.srv.Close()
	_, err := h.c.Runner(context.Background(), "acme/app", 5)
	if err == nil {
		t.Fatal("want an error")
	}
	if len(h.slept()) != Tries-1 {
		t.Fatalf("want %d backoffs, got %v", Tries-1, h.slept())
	}
	h.noLeak(t, err)
}

func TestNoRetryOn404(t *testing.T) {
	h := newHarness(t)
	_, err := h.c.JobByID(context.Background(), "acme/app", 99)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("want core.ErrNotFound, got %v", err)
	}
	if n := h.f.count("GET", "/repos/acme/app/actions/jobs/99"); n != 1 {
		t.Fatalf("404 retried: %d tries", n)
	}
	if len(h.slept()) != 0 {
		t.Fatal("slept on a 404")
	}
	st, err := h.c.Runner(context.Background(), "acme/app", 77)
	if err != nil || st.Exists {
		t.Fatalf("missing runner: %+v, %v", st, err)
	}
	if err := h.c.DeleteRunner(context.Background(), "acme/app", 77); err != nil {
		t.Fatalf("deleting a missing runner: %v", err)
	}
}

func TestJobByID(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app/actions/jobs/70", 200, mkJob(70, 7, "in_progress", t0, "self-hosted", "tentmaker"))
	h.f.json("GET", "/repos/acme/app/actions/runs/7", 200, mkRun(7, "fork/app", t0, "u"))
	j, err := h.c.JobByID(context.Background(), "acme/app", 70)
	if err != nil {
		t.Fatal(err)
	}
	if j.ID != 70 || j.RunID != 7 || !j.FromFork || j.Status != "in_progress" {
		t.Fatalf("got %+v", j)
	}
}

func TestRunningJobs(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", inProgressURI, 200, runList(mkRun(7, "acme/app", t0, "u")))
	h.f.json("GET", jobsURI(7), 200, jobList(mkJob(70, 7, "in_progress", t0), mkJob(71, 7, "queued", t0), mkJob(72, 7, "completed", t0)))
	got, err := h.c.RunningJobs(context.Background(), "acme/app")
	if err != nil || len(got) != 1 || got[0].ID != 70 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The runner API never says which job a busy runner has, so the jobs must
// carry the runner that took them (review r1 partA first-F1).
func TestJobsNameTheirRunner(t *testing.T) {
	h := newHarness(t)
	running := mkJob(70, 7, "in_progress", t0)
	running["runner_id"], running["runner_name"] = 4242, "tm-box-1"
	h.f.json("GET", inProgressURI, 200, runList(mkRun(7, "acme/app", t0, "u")))
	h.f.json("GET", jobsURI(7), 200, jobList(running))
	h.f.json("GET", "/repos/acme/app/actions/jobs/70", 200, running)
	h.f.json("GET", "/repos/acme/app/actions/runs/7", 200, mkRun(7, "acme/app", t0, "u"))
	got, err := h.c.RunningJobs(context.Background(), "acme/app")
	if err != nil || len(got) != 1 || got[0].RunnerID != 4242 || got[0].RunnerName != "tm-box-1" {
		t.Fatalf("RunningJobs = %+v, %v", got, err)
	}
	j, err := h.c.JobByID(context.Background(), "acme/app", 70)
	if err != nil || j.RunnerID != 4242 || j.RunnerName != "tm-box-1" {
		t.Fatalf("JobByID = %+v, %v", j, err)
	}
}

func TestGrantParsesJITConfig(t *testing.T) {
	h := newHarness(t)
	h.f.json("POST", "/repos/acme/app/actions/runners/generate-jitconfig", 201, map[string]any{
		"runner":             map[string]any{"id": 4242, "name": "tm-box-1"},
		"encoded_jit_config": "ZW5jb2RlZC1qaXQ=",
	})
	g, err := h.c.Grant(context.Background(), "acme/app", "tm-box-1", []string{"self-hosted", "tentmaker", "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if g.RunnerID != 4242 || string(g.Secret) != "ZW5jb2RlZC1qaXQ=" || g.RunnerName != "tm-box-1" || g.Forge != "github" || !g.ExpiresAt.After(t0) {
		t.Fatalf("got %+v", g)
	}
	body := h.f.body("POST", "/repos/acme/app/actions/runners/generate-jitconfig")
	if body["runner_group_id"] != float64(1) || body["name"] != "tm-box-1" || fmt.Sprint(body["labels"]) != "[self-hosted tentmaker linux]" {
		t.Fatalf("request body: %v", body)
	}
	if strings.Contains(h.logs.String(), "ZW5jb2RlZC1qaXQ=") {
		t.Fatal("the JIT config reached the log")
	}
}

func TestGrantRejectsBadNameAndBadResponse(t *testing.T) {
	h := newHarness(t)
	if _, err := h.c.Grant(context.Background(), "acme/app", "Bad Name", nil); err == nil {
		t.Fatal("accepted a bad runner name")
	}
	h.f.json("POST", "/repos/acme/app/actions/runners/generate-jitconfig", 201, map[string]any{"runner": map[string]any{"id": 0}})
	if _, err := h.c.Grant(context.Background(), "acme/app", "tm-box-1", nil); err == nil {
		t.Fatal("accepted a response without a config")
	}
}

func TestCancelRun(t *testing.T) {
	h := newHarness(t)
	h.f.json("POST", "/repos/acme/app/actions/runs/7/cancel", 202, map[string]any{})
	h.f.json("POST", "/repos/acme/app/actions/runs/8/cancel", 409, map[string]any{"message": "Cannot cancel a workflow run that is completed."})
	if err := h.c.CancelRun(context.Background(), "acme/app", 7); err != nil {
		t.Fatal(err)
	}
	if err := h.c.CancelRun(context.Background(), "acme/app", 8); err != nil {
		t.Fatalf("409 should be fine: %v", err)
	}
	if err := h.c.CancelRun(context.Background(), "acme/app", 9); statusOf(err) != 404 {
		t.Fatalf("want 404 for a missing run, got %v", err)
	}
}

func TestDeleteRunner(t *testing.T) {
	h := newHarness(t)
	h.f.on("DELETE", "/repos/acme/app/actions/runners/5", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if err := h.c.DeleteRunner(context.Background(), "acme/app", 5); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRepo(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app", 200, map[string]any{"private": false, "visibility": "public", "default_branch": "main", "permissions": map[string]any{"admin": true}})
	h.f.json("GET", "/repos/acme/app/actions/permissions/fork-pr-contributor-approval", 200, map[string]any{"approval_policy": "all_external_contributors"})
	h.f.json("GET", "/repos/acme/app/actions/runs?per_page=1", 200, map[string]any{})
	h.f.json("GET", "/repos/acme/app/actions/runners?per_page=1", 200, map[string]any{})
	info, err := h.c.CheckRepo(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Public || info.DefaultBranch != "main" || info.ForkPolicy != "approve_all" || len(info.Problems) != 0 {
		t.Fatalf("got %+v", info)
	}

	h.f.json("GET", "/repos/acme/priv", 200, map[string]any{"private": true, "visibility": "private", "default_branch": "dev", "permissions": map[string]any{"admin": false}})
	h.f.json("GET", "/repos/acme/priv/actions/permissions/fork-pr-workflows-private-repos", 200, map[string]any{"run_workflows_from_fork_pull_requests": false})
	h.f.json("GET", "/repos/acme/priv/actions/runs?per_page=1", 200, map[string]any{})
	h.f.json("GET", "/repos/acme/priv/actions/runners?per_page=1", 403, map[string]any{"message": "Must have admin rights to Repository."})
	info, err = h.c.CheckRepo(context.Background(), "acme/priv")
	if err != nil {
		t.Fatal(err)
	}
	if info.Public || info.ForkPolicy != "off" || len(info.Problems) != 2 {
		t.Fatalf("got %+v", info)
	}

	h.f.json("GET", "/repos/acme/open", 200, map[string]any{"private": false, "default_branch": "main"})
	h.f.json("GET", "/repos/acme/open/actions/permissions/fork-pr-contributor-approval", 403, map[string]any{"message": "Must have admin rights to Repository."})
	h.f.json("GET", "/repos/acme/open/actions/runs?per_page=1", 200, map[string]any{})
	h.f.json("GET", "/repos/acme/open/actions/runners?per_page=1", 200, map[string]any{})
	info, err = h.c.CheckRepo(context.Background(), "acme/open")
	if err != nil {
		t.Fatal(err)
	}
	if info.ForkPolicy != "unknown" || len(info.Problems) != 1 || !strings.Contains(info.Problems[0], "fork pull request policy") {
		t.Fatalf("unreadable policy: %+v", info)
	}
	if _, err := h.c.CheckRepo(context.Background(), "not-a-repo"); err == nil {
		t.Fatal("accepted a bad repo name")
	}
}

func TestSetForkApprovalAndReadOnlyToken(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/acme/app", 200, map[string]any{"private": false, "visibility": "public"})
	h.f.on("PUT", "/repos/acme/app/actions/permissions/fork-pr-contributor-approval", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h.f.json("GET", "/repos/acme/priv", 200, map[string]any{"private": true, "visibility": "private"})
	h.f.on("PUT", "/repos/acme/priv/actions/permissions/fork-pr-workflows-private-repos", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h.f.on("PUT", "/repos/acme/app/actions/permissions/workflow", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ctx := context.Background()
	if err := h.c.SetForkApproval(ctx, "acme/app"); err != nil {
		t.Fatal(err)
	}
	if b := h.f.body("PUT", "/repos/acme/app/actions/permissions/fork-pr-contributor-approval"); b["approval_policy"] != "all_external_contributors" {
		t.Fatalf("public body: %v", b)
	}
	if err := h.c.SetForkApproval(ctx, "acme/priv"); err != nil {
		t.Fatal(err)
	}
	if b := h.f.body("PUT", "/repos/acme/priv/actions/permissions/fork-pr-workflows-private-repos"); b["run_workflows_from_fork_pull_requests"] != false {
		t.Fatalf("private body: %v", b)
	}
	if err := h.c.SetReadOnlyToken(ctx, "acme/app"); err != nil {
		t.Fatal(err)
	}
	if b := h.f.body("PUT", "/repos/acme/app/actions/permissions/workflow"); b["default_workflow_permissions"] != "read" || b["can_approve_pull_request_reviews"] != false {
		t.Fatalf("workflow body: %v", b)
	}
}

func TestTokenNeverLeaks(t *testing.T) {
	h := newHarness(t)
	// A hostile or buggy server that echoes the credential back.
	echo := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":"bad header %s"}`, r.Header.Get("Authorization"))
		}
	}
	h.f.on("GET", "/repos/acme/app/actions/runners/1", echo(500))
	h.f.on("GET", "/repos/acme/app/actions/runners/2", echo(422))
	h.f.on("GET", "/repos/acme/app/actions/jobs/3", echo(403))
	h.f.on("POST", "/repos/acme/app/actions/runners/generate-jitconfig", echo(400))
	ctx := context.Background()
	_, e1 := h.c.Runner(ctx, "acme/app", 1)
	_, e2 := h.c.Runner(ctx, "acme/app", 2)
	_, e3 := h.c.JobByID(ctx, "acme/app", 3)
	_, e4 := h.c.Grant(ctx, "acme/app", "tm-box-1", nil)
	e5 := h.c.PostStatus(ctx, "acme/app", "abc", "tentmaker/x", "bogus", "", "")
	for _, e := range []error{e1, e2, e3, e4} {
		if e == nil {
			t.Fatal("want errors")
		}
	}
	h.noLeak(t, e1, e2, e3, e4, e5)
	if !strings.Contains(e1.Error(), "[redacted]") {
		t.Fatalf("echoed credential not redacted: %v", e1)
	}
	// A 401 drops the cached credential and asks the source again.
	calls := 0
	h.c.tokenFn = func(context.Context) (string, error) { calls++; return "wrong-" + strconv.Itoa(calls), nil }
	h.c.token = ""
	_, e6 := h.c.Runner(ctx, "acme/app", 1)
	_, e7 := h.c.Runner(ctx, "acme/app", 1)
	if statusOf(e6) != 401 || statusOf(e7) != 401 || calls != 2 {
		t.Fatalf("401 handling: %v %v calls=%d", e6, e7, calls)
	}
	h.c.tokenFn = func(context.Context) (string, error) { return "", errors.New("gh is not logged in") }
	h.c.token = ""
	_, e8 := h.c.Runner(ctx, "acme/app", 1)
	if e8 == nil {
		t.Fatal("want an error without a credential")
	}
	h.noLeak(t, e6, e7, e8)
}
