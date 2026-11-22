package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// fakeDebug is what the fake daemon's test and debug routes serve and saw.
type fakeDebug struct {
	mu       sync.Mutex
	tests    map[string]runs.TestResults
	logs     map[string]string
	kept     map[string]bool
	execs    []control.DebugExecRequest
	releases []string
}

var debugs sync.Map // *fakeDaemon -> *fakeDebug

func (d *fakeDaemon) dbg() *fakeDebug {
	v, _ := debugs.LoadOrStore(d, &fakeDebug{tests: map[string]runs.TestResults{}, logs: map[string]string{}, kept: map[string]bool{}})
	return v.(*fakeDebug)
}

func (d *fakeDaemon) record(id string) (runs.Record, bool) {
	h := d.hist()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.ID == id {
			return r, true
		}
	}
	return runs.Record{}, false
}

func (d *fakeDaemon) Tests(ctx context.Context, id string) (runs.TestsAnswer, error) {
	rec, ok := d.record(id)
	if !ok {
		return runs.TestsAnswer{}, core.ErrNotFound
	}
	g := d.dbg()
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.tests[id]
	t.Cases = nonNil(t.Cases)
	return runs.TestsAnswer{Record: rec, Results: t}, nil
}

func (d *fakeDaemon) Flaky(ctx context.Context, q runs.Query) (control.FlakyAnswer, error) {
	recs, _ := d.Runs(ctx, q)
	return control.FlakyAnswer{Since: q.Since, Tests: runs.Flaky(recs.Records)}, nil
}

func (d *fakeDaemon) Why(ctx context.Context, req control.WhyRequest) (runs.WhyAnswer, error) {
	a, _ := d.Runs(ctx, runs.Query{ID: req.ID, Repo: req.Repo, SHA: req.SHA, RunID: req.RunID, Limit: runs.MaxLimit})
	for _, r := range a.Records {
		if req.ID != "" || r.Failed() {
			g := d.dbg()
			g.mu.Lock()
			t, ok := g.tests[r.ID]
			log := g.logs[r.ID]
			g.mu.Unlock()
			var tp *runs.TestResults
			if ok {
				tp = &t
			}
			all, _ := d.Runs(ctx, runs.Query{Repo: r.Repo, Limit: runs.MaxLimit})
			return runs.Why(r, tp, []byte(log), all.Records), nil
		}
	}
	return runs.WhyAnswer{}, fmt.Errorf("no failed job like that: %w", core.ErrNotFound)
}

func (d *fakeDaemon) DebugExec(ctx context.Context, req control.DebugExecRequest) (control.DebugExecAnswer, error) {
	g := d.dbg()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.execs = append(g.execs, req)
	if !g.kept[req.ID] {
		return control.DebugExecAnswer{}, fmt.Errorf("no kept sandbox for job %s on this device: %w", req.ID, core.ErrNotFound)
	}
	return control.DebugExecAnswer{ID: req.ID, ExitCode: 7, Output: "ran " + req.Command + "\n", Truncated: true}, nil
}

func (d *fakeDaemon) DebugShell(ctx context.Context, id string, stdin io.Reader, stdout io.Writer) error {
	g := d.dbg()
	g.mu.Lock()
	kept := g.kept[id]
	g.mu.Unlock()
	if !kept {
		return fmt.Errorf("no kept sandbox for job %s on this device: %w", id, core.ErrNotFound)
	}
	io.WriteString(stdout, "$ ")
	_, err := io.Copy(stdout, stdin)
	return err
}

func (d *fakeDaemon) DebugRelease(ctx context.Context, id string) error {
	g := d.dbg()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releases = append(g.releases, id)
	if !g.kept[id] {
		return fmt.Errorf("no kept sandbox for job %s on this device: %w", id, core.ErrNotFound)
	}
	delete(g.kept, id)
	return nil
}

// debugFixture has a failed job tm-box-1 with JUnit tests, a log with a
// go test failure, and a sandbox kept for it.
func debugFixture(t *testing.T, root bool) (*fixture, *fakeDebug) {
	f := newFixture(t)
	f.setUp()
	// Set before the server starts: its handler reads isRoot.
	f.isRoot = root
	f.serveControl()
	h := f.daemon.hist()
	now := time.Now()
	h.mu.Lock()
	h.recs = []runs.Record{
		{ID: "tm-box-1", Repo: "baney75/site", Workflow: "ci", Job: "test", Device: "box", RunID: 41, Platform: "linux/x64",
			StartedAt: now.Add(-9 * time.Minute), EndedAt: now.Add(-5 * time.Minute), Conclusion: runs.Failure, FailedStep: "go test"},
		{ID: "tm-box-2", Repo: "baney75/site", Workflow: "ci", Job: "lint", Device: "box", RunID: 41, Platform: "linux/x64",
			StartedAt: now.Add(-9 * time.Minute), EndedAt: now.Add(-8 * time.Minute), Conclusion: runs.Success},
	}
	h.recs[0].SHA = "abcdef1234567"
	h.recs[0].Ref = "main"
	h.recs[0].KeptUntil = time.Now().Add(10 * time.Minute)
	h.recs[0].Tests = &runs.TestSummary{Total: 2, Passed: 1, Failed: 1, Failures: []string{"site/parse.TestParse"}}
	h.mu.Unlock()
	g := f.daemon.dbg()
	g.tests["tm-box-1"] = runs.TestResults{ID: "tm-box-1", Cases: []runs.TestCase{
		{Suite: "site/parse", Name: "TestParse", Status: runs.TestFailed, Message: "want 2, got 3", Detail: "parse_test.go:12: want 2, got 3"},
		{Suite: "site/parse", Name: "TestLex", Status: runs.TestPassed},
	}}
	g.logs["tm-box-1"] = "Run go test ./...\n--- FAIL: TestParse (0.00s)\n    parse_test.go:12: want 2, got 3\nFAIL\n"
	g.kept["tm-box-1"] = true
	return f, g
}

func TestTestsCommand(t *testing.T) {
	f, _ := debugFixture(t, true)
	if code := f.run("", "tests", "tm-box-1"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	out := f.stdout.String()
	for _, want := range []string{"tm-box-1", "2 tests: 1 passed, 1 failed, 0 skipped", "FAIL site/parse.TestParse", "want 2, got 3",
		"parse_test.go:12", "passed  site/parse.TestLex"} {
		if !strings.Contains(out, want) {
			t.Errorf("tests output misses %q:\n%s", want, out)
		}
	}
	if code := f.run("", "tests", "tm-box-1", "--failed", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	var ta runs.TestsAnswer
	if err := json.Unmarshal(f.stdout.Bytes(), &ta); err != nil || len(ta.Results.Cases) != 1 || ta.Results.Cases[0].Name != "TestParse" {
		t.Fatalf("tests --failed --json = %+v %v", ta, err)
	}
	if code := f.run("", "tests", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	var list testRunsView
	if err := json.Unmarshal(f.stdout.Bytes(), &list); err != nil || len(list.Runs) != 1 || list.Runs[0].ID != "tm-box-1" || list.Runs[0].Tests.Failed != 1 {
		t.Fatalf("tests --json = %+v %v", list, err)
	}
	if code := f.run("", "tests", "tm-nobody-1"); code != ExitFailed || !strings.Contains(f.stderr.String(), "no device has job") {
		t.Fatalf("tests of a missing job: exit %d %s", code, f.stderr)
	}
	if code := f.run("", "tests", "../x"); code != ExitUsage {
		t.Fatalf("tests of a bad id: exit %d", code)
	}
}

func TestWhyCommand(t *testing.T) {
	f, _ := debugFixture(t, true)
	if code := f.run("", "why", "tm-box-1"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	out := f.stdout.String()
	for _, want := range []string{"failure", "baney75/site", "ci / test", "ref main", "sha abcdef1", "failed step: go test",
		"failing tests:", "site/parse.TestParse: want 2, got 3", "log around the first error:", "--- FAIL: TestParse",
		"flaky: no sign of it", "sudo tentmaker debug tm-box-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("why output misses %q:\n%s", want, out)
		}
	}
	if code := f.run("", "why", "--repo", "baney75/site", "--sha", "abcdef1", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	var w runs.WhyAnswer
	if err := json.Unmarshal(f.stdout.Bytes(), &w); err != nil || w.ID != "tm-box-1" || len(w.Failures) != 1 || w.ErrorLine != 2 {
		t.Fatalf("why --repo --sha = %+v %v", w, err)
	}
	if code := f.run("", "why", "--repo", "baney75/site", "--run", "41", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	if code := f.run("", "why", "https://github.com/baney75/site/actions/runs/41", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	for _, bad := range [][]string{{"why"}, {"why", "--repo", "o/r"}, {"why", "tm-box-1", "--repo", "o/r"}, {"why", "--repo", "o/r", "--sha", "abcdef1", "--run", "4"}} {
		if code := f.run("", bad...); code != ExitUsage {
			t.Errorf("%v: exit %d, want usage", bad, code)
		}
	}
	if code := f.run("", "why", "--repo", "baney75/app", "--sha", "1234567"); code != ExitFailed {
		t.Errorf("why with no failed job: exit %d", code)
	}
}

func TestFlakyCommand(t *testing.T) {
	f, h := histFixture(t)
	now := time.Now()
	h.mu.Lock()
	for i, failed := range []bool{true, false} {
		s := &runs.TestSummary{Total: 1, Passed: 1}
		concl := runs.Success
		if failed {
			s = &runs.TestSummary{Total: 1, Failed: 1, Failures: []string{"pkg.TestFlaky"}}
			concl = runs.Failure
		}
		h.recs = append(h.recs, runs.Record{ID: fmt.Sprintf("tm-box-f%d", i), Repo: "o/r", Workflow: "ci", Job: "test", Device: "box",
			SHA: "abcdef1234567", Ref: "main", StartedAt: now.Add(time.Duration(i-3) * time.Hour), EndedAt: now.Add(time.Duration(i-3)*time.Hour + time.Minute),
			Conclusion: concl, Tests: s})
	}
	h.mu.Unlock()
	if code := f.run("", "flaky"); code != ExitOK || !strings.Contains(f.stdout.String(), "pkg.TestFlaky") ||
		!strings.Contains(f.stdout.String(), "same-commit") {
		t.Fatalf("flaky exit %d: %s %s", code, f.stdout, f.stderr)
	}
	if code := f.run("", "flaky", "--repo", "o/r", "--since", "2h", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	var v control.FlakyAnswer
	if err := json.Unmarshal(f.stdout.Bytes(), &v); err != nil || len(v.Tests) != 0 {
		t.Fatalf("flaky --since 2h = %+v %v", v, err)
	}
	if code := f.run("", "flaky", "--since", "forever"); code != ExitUsage {
		t.Fatalf("flaky --since forever: exit %d", code)
	}
}

func TestDebugCommand(t *testing.T) {
	f, g := debugFixture(t, true)
	if code := f.run("", "debug", "tm-box-1", "--exec", "cat out.txt", "--timeout", "90s", "--max-bytes", "100"); code != 7 {
		t.Fatalf("debug --exec exit %d, want the command's 7: %s", code, f.stderr)
	}
	if f.stdout.String() != "ran cat out.txt\n" || !strings.Contains(f.stderr.String(), "output cut") {
		t.Fatalf("debug --exec printed %q / %q", f.stdout, f.stderr)
	}
	g.mu.Lock()
	if len(g.execs) != 1 || g.execs[0].TimeoutS != 90 || g.execs[0].MaxBytes != 100 || g.execs[0].Command != "cat out.txt" {
		t.Fatalf("execs = %+v", g.execs)
	}
	g.mu.Unlock()
	if code := f.run("", "debug", "tm-box-1", "--exec", "ls", "--json"); code != 7 {
		t.Fatalf("debug --exec --json exit %d", code)
	}
	m := f.jsonObject(nil)
	if m["exit_code"] != float64(7) || m["output"] != "ran ls\n" || m["truncated"] != true {
		t.Fatalf("debug --exec --json = %v", m)
	}

	// The shell copies stdin to the sandbox and its output back.
	if code := f.run("echo hi\n", "debug", "tm-box-1"); code != ExitOK || f.stdout.String() != "$ echo hi\n" {
		t.Fatalf("debug shell exit %d: %q %s", code, f.stdout, f.stderr)
	}

	if code := f.run("", "debug", "tm-box-2", "--exec", "ls"); code != ExitFailed || !strings.Contains(f.stderr.String(), "no kept sandbox") ||
		!strings.Contains(f.stderr.String(), "keep_failed") {
		t.Fatalf("debug of a job with no kept sandbox: exit %d %s", code, f.stderr)
	}
	if code := f.run("", "debug", "tm-box-1", "--release", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	if m := f.jsonObject(nil); m["released"] != true {
		t.Fatalf("release = %v", m)
	}
	for _, bad := range [][]string{{"debug"}, {"debug", "../x"}, {"debug", "tm-box-1", "--release", "--exec", "ls"},
		{"debug", "tm-box-1", "--timeout", "5s"}, {"debug", "tm-box-1", "--exec", "ls", "--timeout", "11m"},
		{"debug", "tm-box-1", "--exec", "ls", "--max-bytes", "2097152"}, {"debug", "tm-box-1", "--exec", " "}, {"debug", "tm-box-1", "--json"}} {
		if code := f.run("", bad...); code != ExitUsage {
			t.Errorf("%v: exit %d, want usage", bad, code)
		}
	}
	f.env.IsRoot = func() bool { return false }
	if code := f.run("", "debug", "tm-box-1", "--exec", "ls"); code != ExitRoot {
		t.Fatalf("debug as a user: exit %d", code)
	}
}

// The MCP tools reach the same code: why_failed and tests read, and
// debug_exec needs the server to run as root.
func TestMCPWhyAndDebugExec(t *testing.T) {
	f, g := debugFixture(t, false)
	call := mcpSession(t, f)
	res, ok := call("why_failed", map[string]any{"id": "tm-box-1"})
	if !ok || res["structuredContent"].(map[string]any)["failed_step"] != "go test" {
		t.Fatalf("why_failed: %s", mustJSON(res))
	}
	res, ok = call("tests", map[string]any{"id": "tm-box-1"})
	if !ok || len(res["structuredContent"].(map[string]any)["results"].(map[string]any)["cases"].([]any)) != 1 {
		t.Fatalf("tests: %s", mustJSON(res))
	}
	res, ok = call("debug_exec", map[string]any{"id": "tm-box-1", "command": "ls"})
	if ok || !strings.Contains(mustJSON(res), "not running as root") {
		t.Fatalf("debug_exec as a user: %s", mustJSON(res))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.execs) != 0 {
		t.Fatalf("a user's debug_exec reached the daemon: %+v", g.execs)
	}
}

func TestMCPDebugExecAsRoot(t *testing.T) {
	f, _ := debugFixture(t, true)
	call := mcpSession(t, f)
	res, ok := call("debug_exec", map[string]any{"id": "tm-box-1", "command": "ls"})
	if !ok || res["structuredContent"].(map[string]any)["exit_code"] != float64(7) {
		t.Fatalf("debug_exec: %s", mustJSON(res))
	}
}
