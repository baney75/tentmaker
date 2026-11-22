package runs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func tested(id, sha, ref, concl string, at int, total int, failures ...string) Record {
	r := Record{ID: id, Kind: "runner", Repo: "o/r", Workflow: "ci", Job: "test", Device: "box", SHA: sha, Ref: ref,
		Conclusion: concl, EndedAt: t0.Add(time.Duration(at) * time.Minute)}
	r.Tests = &TestSummary{Total: total, Failed: len(failures), Passed: total - len(failures), Failures: failures, Suites: []string{"pkg"}}
	return r
}

func TestStoreTests(t *testing.T) {
	s := Open(t.TempDir(), 10)
	if err := s.PutTests("tm-box-1", TestResults{Cases: []TestCase{{Name: "A", Status: TestFailed}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PutTests before the record = %v", err)
	}
	if err := s.Add(Record{ID: "tm-box-1", Repo: "o/r", Device: "box", Conclusion: Failure}, nil); err != nil {
		t.Fatal(err)
	}
	cases := []TestCase{{Suite: "pkg", Name: "TestOK", Status: TestPassed}}
	for i := 0; i < MaxFailedCases+3; i++ {
		cases = append(cases, TestCase{Suite: "pkg", Name: "TestBad" + string(rune('a'+i%26)) + strings.Repeat("x", i/26),
			Status: TestFailed, Message: "token ghp_" + strings.Repeat("A", 36) + " " + strings.Repeat("m", 2*MaxMessage)})
	}
	if err := s.PutTests("tm-box-1", TestResults{Cases: cases}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Tests("tm-box-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cases) != MaxFailedCases+1 || got.Dropped != 3 || got.Cases[0].Status != TestFailed {
		t.Fatalf("cases = %d, dropped = %d", len(got.Cases), got.Dropped)
	}
	m := got.Cases[0].Message
	if strings.Contains(m, "ghp_") || !strings.Contains(m, Mask) || len(m) > MaxMessage {
		t.Fatalf("message not redacted and capped: %d %q", len(m), m[:60])
	}
	r, _ := s.Get("tm-box-1")
	if r.Tests == nil || r.Tests.Failed != MaxFailedCases || r.Tests.Passed != 1 || len(r.Tests.Failures) != MaxSummaryFailures || !r.Tests.Truncated {
		t.Fatalf("summary = %+v", r.Tests)
	}
	if _, err := s.Tests("../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id = %v", err)
	}
}

func TestMergePrefersJUnit(t *testing.T) {
	a := TestResults{Cases: []TestCase{{Suite: "p", Name: "T", Status: TestFailed, Source: SourceGoTestJSON}}}
	b := TestResults{Cases: []TestCase{{Suite: "p", Name: "T", Status: TestFailed, Message: "boom", Source: SourceJUnit},
		{Suite: "p", Name: "U", Status: TestPassed, Source: SourceJUnit}}}
	m := a.Merge(b)
	if len(m.Cases) != 2 || m.Cases[0].Message != "boom" {
		t.Fatalf("merged = %+v", m.Cases)
	}
	s := m.Summary()
	if s.Total != 2 || s.Failed != 1 || len(s.Sources) != 1 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestCappedNames(t *testing.T) {
	long := strings.Repeat("n", 200<<10)
	c := TestResults{Cases: []TestCase{{Suite: long, Name: long, File: long, Status: TestFailed}}}.Capped().Cases[0]
	if len(c.Suite) > MaxName || len(c.Name) > MaxName || len(c.File) > MaxName {
		t.Fatalf("suite %d, name %d, file %d bytes", len(c.Suite), len(c.Name), len(c.File))
	}
}

func TestFlakySameCommit(t *testing.T) {
	recs := []Record{
		tested("tm-box-1", "abcdef1234", "refs/heads/main", Failure, 1, 3, "pkg.TestNet"),
		tested("tm-box-2", "abcdef1234", "refs/heads/main", Success, 5, 3),
		// Another job on the same commit doesn't count.
		func() Record {
			r := tested("tm-box-3", "abcdef1234", "", Failure, 6, 3, "pkg.TestDisk")
			r.Job = "lint"
			return r
		}(),
		func() Record { r := tested("tm-box-4", "abcdef1234", "", Success, 7, 3); r.Job = "build"; return r }(),
	}
	got := Flaky(recs)
	if len(got) != 1 || got[0].Test != "pkg.TestNet" || got[0].Reason != FlakySameCommit || got[0].Runs[1] != "tm-box-2" {
		t.Fatalf("flaky = %+v", got)
	}
	// A cut summary says nothing about the test.
	recs[1].Tests.Truncated = true
	if got := Flaky(recs[:2]); len(got) != 0 {
		t.Fatalf("flaky from a cut summary = %+v", got)
	}
	// Nor does a rerun that did not report the test's package whole: here
	// TestNet's events fell out of the log tail.
	rerun := TestResults{Cases: []TestCase{{Suite: "pkg", Name: "TestOther", Status: TestPassed, Partial: true},
		{Suite: "other", Name: "TestY", Status: TestPassed}}}
	recs[1].Tests = rerun.Summary()
	if got := Flaky(recs[:2]); len(got) != 0 || recs[1].Tests.Truncated {
		t.Fatalf("flaky from a rerun without pkg = %+v", got)
	}
}

func TestFlakyFlips(t *testing.T) {
	var recs []Record
	for i, f := range []bool{false, true, false, true, false} {
		var fails []string
		concl := Success
		if f {
			fails, concl = []string{"pkg.TestRace"}, Failure
		}
		recs = append(recs, tested("tm-box-"+string(rune('a'+i)), "sha000000"+string(rune('a'+i)), "refs/heads/main", concl, i, 4, fails...))
	}
	got := Flaky(recs)
	if len(got) != 1 || got[0].Reason != FlakyFlipped || got[0].Flips != 4 || got[0].Ref != "refs/heads/main" {
		t.Fatalf("flaky = %+v", got)
	}
	// A test that broke once and stayed broken is not flaky.
	steady := []Record{
		tested("tm-box-1", "1111111a", "refs/heads/main", Success, 1, 2),
		tested("tm-box-2", "1111111b", "refs/heads/main", Failure, 2, 2, "pkg.TestX"),
		tested("tm-box-3", "1111111c", "refs/heads/main", Failure, 3, 2, "pkg.TestX"),
	}
	if got := Flaky(steady); len(got) != 0 {
		t.Fatalf("steady failure called flaky: %+v", got)
	}
}

func TestLogExcerpt(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		b.WriteString("2026-11-22T10:00:00.1234567Z line\n")
	}
	b.WriteString("2026-11-22T10:00:01.0000000Z --- FAIL: TestParse (0.01s)\n")
	for i := 1; i <= 40; i++ {
		b.WriteString("after\n")
	}
	lines, at := LogExcerpt([]byte(b.String()))
	if at != 41 || len(lines) != WhyBefore+1+WhyAfter || lines[WhyBefore] != "--- FAIL: TestParse (0.01s)" {
		t.Fatalf("at = %d, %d lines, error line %q", at, len(lines), lines[WhyBefore])
	}
	lines, at = LogExcerpt([]byte(strings.Repeat("nothing wrong\n", 50)))
	if at != 0 || len(lines) != WhyFallback {
		t.Fatalf("fallback at = %d, %d lines", at, len(lines))
	}
}

func TestWhy(t *testing.T) {
	rec := tested("tm-box-9", "abcdef1234", "refs/heads/main", Failure, 10, 8,
		"pkg.T1", "pkg.T2", "pkg.T3", "pkg.T4", "pkg.T5", "pkg.T6")
	rec.FailedStep, rec.RunID = "go test", 77
	var tr TestResults
	for i := 1; i <= 6; i++ {
		tr.Cases = append(tr.Cases, TestCase{Suite: "pkg", Name: "T" + string(rune('0'+i)), Status: TestFailed,
			Message: "want 1, got 2 token=ghp_" + strings.Repeat("B", 36), Detail: strings.Repeat("stack\n", 30)})
	}
	log := []byte("setup\n##[group]Run go test\n--- FAIL: T1\n    x_test.go:9: want 1, got 2\nFAIL\n")
	a := Why(rec, &tr, log, nil)
	if a.FailedStep != "go test" || a.SHA != "abcdef1234" || a.Ref != "refs/heads/main" || a.RunID != 77 {
		t.Fatalf("answer = %+v", a)
	}
	if len(a.Failures) != WhyMaxFailures || a.MoreFailures != 1 || a.Failures[0].Test != "pkg.T1" {
		t.Fatalf("failures = %+v more %d", a.Failures, a.MoreFailures)
	}
	if strings.Contains(a.Failures[0].Message, "ghp_") || strings.Count(a.Failures[0].Detail, "\n") > WhyDetailLines {
		t.Fatalf("failure not redacted or cut: %+v", a.Failures[0])
	}
	if a.ErrorLine != 3 || a.Log[0] != "setup" || a.Flaky {
		t.Fatalf("log = %q at %d, flaky %v", a.Log, a.ErrorLine, a.Flaky)
	}
	// The same job passed on the same commit: it looks flaky.
	pass := tested("tm-box-10", "abcdef1234", "refs/heads/main", Success, 12, 8)
	if a := Why(rec, &tr, log, []Record{pass}); !a.Flaky || !strings.Contains(a.FlakyReason, "tm-box-10") {
		t.Fatalf("flaky = %v %q", a.Flaky, a.FlakyReason)
	}
	// Without the kept cases, the record's summary names the failures.
	if a := Why(rec, nil, nil, nil); len(a.Failures) != 5 || a.Failures[0].Test != "pkg.T1" || a.Log == nil {
		t.Fatalf("summary failures = %+v", a)
	}
}
