package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// JobReport reads the job's conclusion and first failed step, then follows
// the log redirect to the signed URL without the token and keeps the tail.
func TestJobReport(t *testing.T) {
	h := newHarness(t)
	sawAuth := false
	body := strings.Repeat("noise\n", 1000) + "##[group]Run go test ./...\n--- FAIL: TestParse (0.00s)\n##[error]Process completed with exit code 1.\n"
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""
		w.Write([]byte(body))
	}))
	defer blob.Close()
	h.f.json("GET", "/repos/acme/app/actions/jobs/7", 200, map[string]any{"id": 7, "status": "completed", "conclusion": "failure",
		"steps": []map[string]any{{"name": "Set up job", "conclusion": "success"}, {"name": "Run go test ./...", "conclusion": "failure"},
			{"name": "Post", "conclusion": "skipped"}}})
	h.f.on("GET", "/repos/acme/app/actions/jobs/7/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blob.URL+"/signed?sig=abc", http.StatusFound)
	})
	rep, err := h.c.JobReport(context.Background(), "acme/app", 7, 200)
	if err != nil || rep.LogErr != nil {
		t.Fatalf("report: %v %v", err, rep.LogErr)
	}
	if rep.Conclusion != "failure" || rep.FailedStep != "Run go test ./..." {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Log) != 200 || !strings.Contains(string(rep.Log), "--- FAIL: TestParse") || !strings.HasSuffix(string(rep.Log), "exit code 1.\n") {
		t.Fatalf("log tail = %q", rep.Log)
	}
	if sawAuth {
		t.Fatal("the token went to the signed log URL")
	}

	// A log that can't be read leaves the conclusion; a job still running
	// has none.
	h.f.json("GET", "/repos/acme/app/actions/jobs/7/logs", 500, map[string]any{"message": "boom"})
	h.f.json("GET", "/repos/acme/app/actions/jobs/8", 200, map[string]any{"id": 8, "status": "in_progress"})
	rep, err = h.c.JobReport(context.Background(), "acme/app", 7, 200)
	if err != nil || rep.Conclusion != "failure" || rep.LogErr == nil || rep.Log != nil {
		t.Fatalf("unreadable log: %+v %v", rep, err)
	}
	rep, err = h.c.JobReport(context.Background(), "acme/app", 8, 200)
	if err != nil || rep.Conclusion != "" {
		t.Fatalf("running job: %+v %v", rep, err)
	}
	h.noLeak(t, err, rep.LogErr)
}
