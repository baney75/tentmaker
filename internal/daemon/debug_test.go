package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/runs"
)

// The control API's debug limits are the worker's.
func TestDebugLimitsMatch(t *testing.T) {
	if control.MaxDebugTimeoutS*time.Second != dispatch.MaxDebugTimeout || control.MaxDebugBytes != dispatch.MaxDebugOutput {
		t.Fatalf("control allows %ds and %d bytes, the worker %s and %d bytes",
			control.MaxDebugTimeoutS, control.MaxDebugBytes, dispatch.MaxDebugTimeout, dispatch.MaxDebugOutput)
	}
}

// A failed job in a repo with keep_failed: why explains it from its JUnit
// tests and its log, and debug exec runs a command in its kept sandbox,
// both end to end through the control client. Status lists the sandbox,
// a job that ran elsewhere is refused with where to go, and release ends
// the grace.
func TestWhyAndDebugExecTranscript(t *testing.T) {
	r := newRig(t, func(c *config.Config) {
		c.Mesh.Enabled = false
		c.Repos[0].Debug.KeepFailed = config.Duration(10 * time.Minute)
	})
	r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "test", RunID: 81, HeadSHA: "abcdef1234567",
		Labels: []string{"self-hosted", "tentmaker", "linux"}})
	r.start()
	defer r.stop()
	r.eventually("a runner to start", func() bool { return len(r.ex.Running()) == 1 })
	name := r.ex.Running()[0]
	r.ex.SetTests(name, runs.TestResults{Files: []string{"report.xml"}, Cases: []runs.TestCase{
		{Suite: "app/parse", Name: "TestParse", Status: runs.TestFailed, Message: "want 2, got 3",
			Detail: "parse_test.go:12: want 2, got 3", File: "report.xml", Source: runs.SourceJUnit},
		{Suite: "app/parse", Name: "TestLex", Status: runs.TestPassed, Source: runs.SourceJUnit},
	}})
	r.ex.SetLog(name, []byte("[x] Processing step: DisplayName='go test'\n=== RUN   TestParse\n"+
		"--- FAIL: TestParse (0.00s)\n    parse_test.go:12: want 2, got 3\nFAIL\tapp/parse\t0.01s\n"+
		"[x] Step result: Failed\n[x] Job result after all job steps finish: Failed\n"))
	r.ex.SetDebugExec(func(_, command string) (core.DebugResult, error) {
		return core.DebugResult{ExitCode: 1, Output: []byte("$ " + command + "\nparse_test.go:12: want 2, got 3\n" +
			"GITHUB_TOKEN=ghp_" + strings.Repeat("Z", 36) + "\n")}, nil
	})
	r.ex.Exit(name)

	ctx := context.Background()
	c := r.client()
	r.eventually("the failed job's sandbox to be kept", func() bool {
		var a runs.Answer
		return c.Get(ctx, "/v1/runs?id="+name, &a) == nil && len(a.Records) == 1 && a.Records[0].Failed() &&
			!a.Records[0].KeptUntil.IsZero() && a.Records[0].Tests != nil
	})

	why, err := c.Why(ctx, control.WhyRequest{ID: name})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(why, "", "  ")
	t.Logf("why %s:\n%s", name, b)
	if why.Conclusion != runs.Failure || why.Repo != "baney75/app" || len(why.Failures) != 1 ||
		why.Failures[0].Test != "app/parse.TestParse" || why.Failures[0].Message != "want 2, got 3" ||
		why.ErrorLine == 0 || !strings.Contains(strings.Join(why.Log, "\n"), "--- FAIL: TestParse") || why.KeptUntil.IsZero() {
		t.Fatalf("why = %s", b)
	}
	if byRun, err := c.Why(ctx, control.WhyRequest{Repo: "baney75/app", RunID: 81}); err != nil || byRun.ID != name {
		t.Fatalf("why by run = %+v %v", byRun, err)
	}
	var e *control.Error
	if _, err := c.Why(ctx, control.WhyRequest{Repo: "baney75/app", RunID: 999}); !errors.As(err, &e) || e.Status != 404 {
		t.Fatalf("why of a run with no jobs: %v", err)
	}
	ta, err := c.Tests(ctx, name)
	if err != nil || len(ta.Results.Cases) != 2 || ta.Results.Cases[0].Status != runs.TestFailed || ta.Record.Tests.Failed != 1 {
		t.Fatalf("tests = %+v %v", ta, err)
	}

	ans, err := c.DebugExec(ctx, control.DebugExecRequest{ID: name, Command: "go test -run TestParse ./parse", TimeoutS: 30, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.MarshalIndent(ans, "", "  ")
	t.Logf("debug exec %s:\n%s", name, b)
	if ans.ExitCode != 1 || !strings.Contains(ans.Output, "want 2, got 3") || strings.Contains(ans.Output, "ghp_") {
		t.Fatalf("debug exec = %s", b)
	}
	if calls := r.ex.DebugCalls(); len(calls) != 1 || calls[0].Command != "go test -run TestParse ./parse" || calls[0].Spec.Timeout != 30*time.Second {
		t.Fatalf("debug calls = %+v", calls)
	}
	var out strings.Builder
	if err := c.DebugShell(ctx, name, strings.NewReader("ls\n"), &out); err != nil || out.String() != "fake shell in "+name+"\nls\n" {
		t.Fatalf("shell = %q %v", out.String(), err)
	}

	var st Status
	if err := c.Get(ctx, "/v1/status", &st); err != nil || st.Worker == nil || len(st.Worker.Kept) != 1 || st.Worker.Kept[0].Name != name {
		t.Fatalf("status kept = %+v %v", st.Worker, err)
	}

	// A job this device didn't run is refused with the device that did.
	r.d.currentHistory().ForgeReport(runs.ForgeReport{ID: "tm-other-1", Device: "other", Repo: "baney75/app", Conclusion: "failure"})
	if _, err := c.DebugExec(ctx, control.DebugExecRequest{ID: "tm-other-1", Command: "true"}); err == nil ||
		!strings.Contains(err.Error(), "job tm-other-1 ran on device other; run tentmaker debug there") {
		t.Fatalf("debug of another device's job: %v", err)
	}
	if _, err := c.DebugExec(ctx, control.DebugExecRequest{ID: "tm-box-nobody", Command: "true"}); !errors.As(err, &e) || e.Status != 404 {
		t.Fatalf("debug of an unknown job: %v", err)
	}

	if err := c.DebugRelease(ctx, name); err != nil {
		t.Fatal(err)
	}
	r.eventually("the kept sandbox to be cleaned up", func() bool {
		var st Status
		cleaned := false
		for _, call := range r.ex.Calls("Clean") {
			cleaned = cleaned || call.Name == name
		}
		return cleaned && c.Get(ctx, "/v1/status", &st) == nil && st.Worker != nil && len(st.Worker.Kept) == 0
	})
	if _, err := c.DebugExec(ctx, control.DebugExecRequest{ID: name, Command: "true"}); !errors.As(err, &e) || e.Status != 404 {
		t.Fatalf("debug after release: %v", err)
	}
}

// flaky reads the pool's records: a test that failed and then passed on
// the same commit is flaky, and why says so.
func TestHistoryFlakyAndWhy(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	r.start()
	defer r.stop()
	h := r.d.currentHistory()
	now := time.Now()
	add := func(id, concl string, failed bool, at time.Time) {
		rec := runs.Record{ID: id, Kind: core.KindRunner, Device: "box", Repo: "o/r", Workflow: "ci", Job: "test", Ref: "main",
			SHA: "abcdef1234567", StartedAt: at.Add(-time.Minute), EndedAt: at, Conclusion: concl}
		status := runs.TestPassed
		if failed {
			status = runs.TestFailed
		}
		h.JobEnded(rec, []byte("--- FAIL: TestFlaky\n"), &runs.TestResults{Cases: []runs.TestCase{{Suite: "pkg", Name: "TestFlaky", Status: status, Source: runs.SourceJUnit}}})
	}
	add("tm-box-a", runs.Failure, true, now.Add(-2*time.Hour))
	add("tm-box-b", runs.Success, false, now.Add(-time.Hour))
	c := r.client()
	ctx := context.Background()
	fa, err := c.Flaky(ctx, runs.Query{Repo: "o/r"})
	if err != nil || len(fa.Tests) != 1 || fa.Tests[0].Test != "pkg.TestFlaky" || fa.Tests[0].Reason != runs.FlakySameCommit ||
		now.Sub(fa.Since) < FlakyWindow-time.Minute {
		t.Fatalf("flaky = %+v %v", fa, err)
	}
	why, err := c.Why(ctx, control.WhyRequest{Repo: "o/r", SHA: "abcdef1"})
	if err != nil || why.ID != "tm-box-a" || !why.Flaky || why.FlakyReason == "" {
		t.Fatalf("why = %+v %v", why, err)
	}
	// Old records are out of the default window.
	if fa, err := c.Flaky(ctx, runs.Query{Repo: "o/r", Since: now.Add(-30 * time.Minute)}); err != nil || len(fa.Tests) != 0 {
		t.Fatalf("flaky in the last 30m = %+v %v", fa, err)
	}
}
