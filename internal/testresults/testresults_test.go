package testresults

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/runs"
)

const pytestReport = `<?xml version="1.0" encoding="utf-8"?>
<testsuites><testsuite name="pytest" errors="0" failures="1" skipped="1" tests="3" time="0.42">
<testcase classname="tests.test_math" name="test_add" time="0.001"/>
<testcase classname="tests.test_math" name="test_div" time="0.120">
<failure message="ZeroDivisionError: division by zero">def test_div():
&gt;       1 / 0
E       ZeroDivisionError: division by zero</failure>
</testcase>
<testcase classname="tests.test_math" name="test_slow" time="0.000">
<skipped type="pytest.skip" message="needs a GPU">tests/test_math.py:20: needs a GPU</skipped>
</testcase>
</testsuite></testsuites>
`

const surefireReport = `<?xml version="1.0" encoding="UTF-8"?>
<!-- written by maven -->
<testsuites name="all">
  <testsuite name="com.x.FooTest" tests="3" failures="0" errors="1" skipped="0" time="1,234.5">
    <properties><property name="java.version" value="21"/></properties>
    <testsuite name="com.x.FooTest$Inner" tests="1">
      <testcase name="innerWorks" time="0.5"/>
    </testsuite>
    <testcase name="works" classname="com.x.FooTest" time="0.25">
      <flakyFailure message="first try failed" type="java.lang.AssertionError">flaky trace</flakyFailure>
      <system-out>hello</system-out>
    </testcase>
    <testcase name="breaks" time="0.75">
      <error type="java.lang.NullPointerException">java.lang.NullPointerException
	at com.x.Foo.bar(Foo.java:12)</error>
    </testcase>
  </testsuite>
</testsuites>
`

const jestReport = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="jest tests" tests="2" failures="1" errors="0" time="2.1">
  <testsuite name="Button" errors="0" failures="1" skipped="0" timestamp="2026-11-22T10:00:00" time="1.9" tests="2">
    <testcase classname="Button renders" name="Button renders" time="0.05">
    </testcase>
    <testcase classname="Button clicks" name="Button clicks" time="0.12">
      <failure>Error: expect(received).toBe(expected)

Expected: 2
Received: 1</failure>
    </testcase>
  </testsuite>
</testsuites>
`

func byName(t *testing.T, cases []runs.TestCase) map[string]runs.TestCase {
	t.Helper()
	m := map[string]runs.TestCase{}
	for _, c := range cases {
		if _, dup := m[c.Name]; dup {
			t.Fatalf("case %q appears twice", c.Name)
		}
		m[c.Name] = c
	}
	return m
}

func TestParseJUnitPytest(t *testing.T) {
	cases, err := ParseJUnit(strings.NewReader(pytestReport), "out/junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("got %d cases, want 3", len(cases))
	}
	m := byName(t, cases)
	if c := m["test_add"]; c.Status != runs.TestPassed || c.Suite != "tests.test_math" || c.Seconds != 0.001 || c.File != "out/junit.xml" {
		t.Errorf("test_add = %+v", c)
	}
	c := m["test_div"]
	if c.Status != runs.TestFailed || c.Message != "ZeroDivisionError: division by zero" || c.Seconds != 0.12 {
		t.Errorf("test_div = %+v", c)
	}
	if !strings.Contains(c.Detail, "> ") || !strings.Contains(c.Detail, "E       ZeroDivisionError") {
		t.Errorf("test_div detail = %q", c.Detail)
	}
	if c := m["test_slow"]; c.Status != runs.TestSkipped || c.Message != "needs a GPU" {
		t.Errorf("test_slow = %+v", c)
	}
}

func TestParseJUnitSurefire(t *testing.T) {
	cases, err := ParseJUnit(strings.NewReader(surefireReport), "TEST-com.x.FooTest.xml")
	if err != nil {
		t.Fatal(err)
	}
	m := byName(t, cases)
	if len(m) != 3 {
		t.Fatalf("got %d cases, want 3: %+v", len(cases), cases)
	}
	if c := m["innerWorks"]; c.Status != runs.TestPassed || c.Suite != "com.x.FooTest$Inner" || c.Seconds != 0.5 {
		t.Errorf("innerWorks = %+v", c)
	}
	// A flaky failure that passed on rerun is a pass.
	if c := m["works"]; c.Status != runs.TestPassed || c.Message != "" || c.Detail != "" {
		t.Errorf("works = %+v", c)
	}
	// No classname: the enclosing suite (after the nested one closed).
	c := m["breaks"]
	if c.Status != runs.TestFailed || c.Suite != "com.x.FooTest" || c.Seconds != 0.75 {
		t.Errorf("breaks = %+v", c)
	}
	if c.Message != "java.lang.NullPointerException" || !strings.Contains(c.Detail, "Foo.java:12") {
		t.Errorf("breaks message %q detail %q", c.Message, c.Detail)
	}
}

func TestParseJUnitJest(t *testing.T) {
	cases, err := ParseJUnit(strings.NewReader(jestReport), "junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	m := byName(t, cases)
	if c := m["Button renders"]; c.Status != runs.TestPassed || c.Suite != "Button renders" || c.Seconds != 0.05 {
		t.Errorf("renders = %+v", c)
	}
	c := m["Button clicks"]
	if c.Status != runs.TestFailed || c.Message != "Error: expect(received).toBe(expected)" || !strings.Contains(c.Detail, "Received: 1") {
		t.Errorf("clicks = %+v", c)
	}
}

func TestParseJUnitRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"other root": `<project><testcase name="x"/></project>`,
		"truncated":  `<testsuite name="a"><testcase name="x">`,
		"charset":    `<?xml version="1.0" encoding="shift_jis"?><testsuite name="a"/>`,
		"empty":      ``,
	} {
		if _, err := ParseJUnit(strings.NewReader(in), "x.xml"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	cases, err := ParseJUnit(strings.NewReader("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><testsuite name=\"a\"><testcase name=\"caf\xe9\"/></testsuite>"), "x.xml")
	if err != nil || len(cases) != 1 || cases[0].Name != "café" {
		t.Errorf("latin1: %+v %v", cases, err)
	}
}

const goTestLog = `{"Time":"2026-11-22T10:00:00Z","Action":"start","Package":"example.com/m/a"}
{"Action":"run","Package":"example.com/m/a","Test":"TestOK"}
{"Action":"output","Package":"example.com/m/a","Test":"TestOK","Output":"=== RUN   TestOK\n"}
{"Action":"pass","Package":"example.com/m/a","Test":"TestOK","Elapsed":0.01}
this is not json at all
{"not":"an event"}
{"Action":"bogus","Package":"example.com/m/a","Test":"TestBogus"}
{"Action":"run","Package":"example.com/m/a","Test":"TestBad"}
{"Action":"output","Package":"example.com/m/a","Test":"TestBad","Output":"=== RUN   TestBad\n"}
{"Action":"output","Package":"example.com/m/a","Test":"TestBad","Output":"    setup done\n"}
{"Action":"output","Package":"example.com/m/a","Test":"TestBad","Output":"    bad_test.go:12: got 1, want 2\n"}
{"Action":"output","Package":"example.com/m/a","Test":"TestBad","Output":"--- FAIL: TestBad (0.02s)\n"}
{"Action":"fail","Package":"example.com/m/a","Test":"TestBad","Elapsed":0.02}
{"Action":"run","Package":"example.com/m/a","Test":"TestSkip"}
{"Action":"skip","Package":"example.com/m/a","Test":"TestSkip","Elapsed":0}
{"Action":"run","Package":"example.com/m/a","Test":"TestParent/sub_one"}
{"Action":"output","Package":"example.com/m/a","Test":"TestParent/sub_one","Output":"        x_test.go:40: nope\n"}
{"Action":"fail","Package":"example.com/m/a","Test":"TestParent/sub_one","Elapsed":0.001}
{"Action":"fail","Package":"example.com/m/a","Test":"TestParent","Elapsed":0.003}
{"Action":"run","Package":"example.com/m/a","Test":"TestNeverEnds"}
{"Action":"output","Package":"example.com/m/a","Output":"FAIL\n"}
{"Action":"fail","Package":"example.com/m/a","Elapsed":0.5}
`

func checkGoCases(t *testing.T, cases []runs.TestCase) {
	t.Helper()
	m := byName(t, cases)
	if len(m) != 5 {
		t.Fatalf("got %d cases, want 5: %+v", len(cases), cases)
	}
	for _, c := range cases {
		if c.Suite != "example.com/m/a" || c.Source != runs.SourceGoTestJSON || c.File != "log" || c.Partial {
			t.Errorf("case %+v: wrong suite, source or file", c)
		}
	}
	if c := m["TestOK"]; c.Status != runs.TestPassed || c.Seconds != 0.01 || c.Detail != "" {
		t.Errorf("TestOK = %+v", c)
	}
	c := m["TestBad"]
	if c.Status != runs.TestFailed || c.Message != "bad_test.go:12: got 1, want 2" {
		t.Errorf("TestBad = %+v", c)
	}
	if c.Detail != "    setup done\n    bad_test.go:12: got 1, want 2" {
		t.Errorf("TestBad detail = %q", c.Detail)
	}
	if c := m["TestSkip"]; c.Status != runs.TestSkipped {
		t.Errorf("TestSkip = %+v", c)
	}
	if c := m["TestParent/sub_one"]; c.Status != runs.TestFailed || c.Message != "x_test.go:40: nope" {
		t.Errorf("subtest = %+v", c)
	}
	if c := m["TestParent"]; c.Status != runs.TestFailed || c.Message != "" {
		t.Errorf("TestParent = %+v", c)
	}
	if _, ok := m[""]; ok {
		t.Error("a package-level event became a case")
	}
	if _, ok := m["TestNeverEnds"]; ok {
		t.Error("a test with no final action became a case")
	}
	if _, ok := m["TestBogus"]; ok {
		t.Error("an event with an unknown action became a case")
	}
}

func TestParseGoTestJSONRaw(t *testing.T) {
	checkGoCases(t, ParseGoTestJSON([]byte(goTestLog)))
	// A log tail that lost the package's start says its cases are partial.
	_, tail, _ := strings.Cut(goTestLog, "\n")
	for _, c := range ParseGoTestJSON([]byte(tail)) {
		if !c.Partial {
			t.Errorf("case %s from a cut log is not partial", c.Name)
		}
	}
}

func TestParseGoTestJSONTimestamped(t *testing.T) {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSuffix(goTestLog, "\n"), "\n") {
		b.WriteString("2026-11-22T10:00:00.1234567Z " + l + "\r\n")
	}
	checkGoCases(t, ParseGoTestJSON([]byte(b.String())))
}

// write puts content at rel under dir, making folders.
func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noHostPath(t *testing.T, res runs.TestResults, dir string) {
	t.Helper()
	for _, s := range append(append([]string{}, res.Problems...), res.Files...) {
		if strings.Contains(s, dir) || filepath.IsAbs(s) || strings.HasPrefix(s, "/") {
			t.Errorf("%q names a host path", s)
		}
	}
	for _, c := range res.Cases {
		if strings.Contains(c.File, dir) {
			t.Errorf("case file %q names a host path", c.File)
		}
	}
}

func TestCollectFindsReports(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "runner/_work/repo/repo/build/test-results/test/TEST-com.x.FooTest.xml", surefireReport)
	write(t, dir, "runner/_work/repo/repo/web/junit.xml", jestReport)
	write(t, dir, "runner/_work/repo/repo/py/out/pytest-results.xml", pytestReport)
	// Not reports: a dependency's report, a non-JUnit xml in a reports
	// folder, a JUnit-looking name holding something else, a tool folder.
	write(t, dir, "runner/_work/repo/repo/web/node_modules/pkg/junit.xml", jestReport)
	write(t, dir, "runner/_work/repo/repo/build/reports/checkstyle.xml", `<?xml version="1.0"?><checkstyle version="10"/>`)
	write(t, dir, "runner/_work/repo/repo/results.xml", `<results><testsuite name="x"/></results>`)
	write(t, dir, "runner/_work/_tool/go/TEST-x.xml", jestReport)
	write(t, dir, "runner/_work/_temp/_runner_file_commands/TEST-x.xml", jestReport)
	write(t, dir, "runner/_work/repo/repo/.git/TEST-x.xml", jestReport)
	// Outside the walked sub-folder.
	write(t, dir, "runner/_diag/TEST-x.xml", jestReport)

	res := Collect(dir, []string{"runner/_work", "work"}, Limits{})
	want := []string{
		"runner/_work/repo/repo/build/test-results/test/TEST-com.x.FooTest.xml",
		"runner/_work/repo/repo/py/out/pytest-results.xml",
		"runner/_work/repo/repo/web/junit.xml",
	}
	if strings.Join(res.Files, ",") != strings.Join(want, ",") {
		t.Errorf("files = %q, want %q", res.Files, want)
	}
	if len(res.Problems) != 0 {
		t.Errorf("problems = %q", res.Problems)
	}
	if len(res.Cases) != 8 {
		t.Errorf("got %d cases, want 8", len(res.Cases))
	}
	if res.Cases[0].Status != runs.TestFailed {
		t.Errorf("failures do not come first: %+v", res.Cases[0])
	}
	for _, c := range res.Cases {
		if c.Source != runs.SourceJUnit || !strings.HasPrefix(c.File, "runner/_work/repo/repo/") {
			t.Errorf("case %+v: wrong source or file", c)
		}
	}
	noHostPath(t, res, dir)
}

func TestCollectMissingFolder(t *testing.T) {
	dir := t.TempDir()
	res := Collect(dir, []string{"work", "../escape"}, Limits{})
	if len(res.Cases) != 0 || len(res.Files) != 0 {
		t.Errorf("got %+v", res)
	}
	res = Collect(filepath.Join(dir, "gone"), []string{"work"}, Limits{})
	if len(res.Cases) != 0 || len(res.Problems) != 0 {
		t.Errorf("missing job folder: %+v", res)
	}
}

func TestCollectRefusesSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write(t, outside, "TEST-secret.xml", jestReport)
	write(t, outside, "test-results/TEST-other.xml", jestReport)
	write(t, dir, "runner/_work/repo/keep/TEST-ok.xml", pytestReport)
	links := map[string]string{
		"runner/_work/repo/TEST-link.xml":    filepath.Join(outside, "TEST-secret.xml"),
		"runner/_work/repo/test-results":     filepath.Join(outside, "test-results"),
		"runner/_work/repo/keep/TEST-in.xml": filepath.Join(dir, "runner/_work/repo/keep/TEST-ok.xml"),
		// A relative link that stays inside the folder is not followed either.
		"runner/_work/repo/keep/TEST-rel.xml": "TEST-ok.xml",
		"runner/_work/repo/reports":           "keep",
	}
	for link, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(link))); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
	res := Collect(dir, []string{"runner/_work"}, Limits{})
	if strings.Join(res.Files, ",") != "runner/_work/repo/keep/TEST-ok.xml" {
		t.Errorf("files = %q", res.Files)
	}
	for _, c := range res.Cases {
		if strings.HasPrefix(c.Name, "Button") {
			t.Errorf("read a case through a symlink: %+v", c)
		}
	}
	// A symlinked root is refused too.
	if err := os.Symlink(filepath.Join(outside), filepath.Join(dir, "work")); err != nil {
		t.Fatal(err)
	}
	res = Collect(dir, []string{"work"}, Limits{})
	if len(res.Files) != 0 {
		t.Errorf("followed a symlinked root: %q", res.Files)
	}
	noHostPath(t, res, dir)
	noHostPath(t, res, outside)
}

func TestCollectSkipsFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on Windows")
	}
	dir := t.TempDir()
	write(t, dir, "work/TEST-ok.xml", pytestReport)
	if err := os.MkdirAll(filepath.Join(dir, "work/test-results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(dir, "work/test-results/TEST-fifo.xml")); err != nil {
		t.Fatal(err)
	}
	done := make(chan runs.TestResults, 1)
	go func() { done <- Collect(dir, []string{"work"}, Limits{}) }()
	select {
	case res := <-done:
		if strings.Join(res.Files, ",") != "work/TEST-ok.xml" {
			t.Errorf("files = %q", res.Files)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Collect hung on a FIFO")
	}
}

func TestCollectLimits(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		write(t, dir, "work/reports/"+n+".xml", pytestReport)
	}
	write(t, dir, "work/big/TEST-huge.xml", pytestReport+strings.Repeat("<!-- pad -->\n", 200))

	res := Collect(dir, []string{"work"}, Limits{MaxFiles: 2, MaxFileBytes: 2000})
	want := "work/reports/a.xml,work/reports/b.xml"
	if strings.Join(res.Files, ",") != want {
		t.Errorf("files = %q, want %q", res.Files, want)
	}
	probs := strings.Join(res.Problems, "\n")
	if !strings.Contains(probs, "skipped work/big/TEST-huge.xml: ") || !strings.Contains(probs, "is over the 2000 bytes limit") {
		t.Errorf("no file size problem in %q", probs)
	}
	if !strings.Contains(probs, "skipped 3 test reports past the 2 file limit") {
		t.Errorf("no file count problem in %q", probs)
	}
	noHostPath(t, res, dir)

	res = Collect(dir, []string{"work"}, Limits{MaxTotal: int64(2*len(pytestReport) + 10)})
	if len(res.Files) != 2 || !strings.Contains(strings.Join(res.Problems, "\n"), "the reports together are over the") {
		t.Errorf("total limit: files %q problems %q", res.Files, res.Problems)
	}

	res = Collect(dir, []string{"work"}, Limits{MaxEntries: 3})
	if !strings.Contains(strings.Join(res.Problems, "\n"), "after 3 folder entries") {
		t.Errorf("entry limit: problems %q", res.Problems)
	}

	write(t, dir, "deep/a/b/c/TEST-deep.xml", pytestReport)
	res = Collect(dir, []string{"deep"}, Limits{MaxDepth: 2})
	if len(res.Files) != 0 || !strings.Contains(strings.Join(res.Problems, "\n"), "more than 2 levels deep") {
		t.Errorf("depth limit: files %q problems %q", res.Files, res.Problems)
	}
}

func TestCollectParseProblem(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "work/reports/x.xml", `<testsuite name="a"><testcase name="x">`)
	write(t, dir, "work/reports/y.xml", pytestReport)
	res := Collect(dir, []string{"work"}, Limits{})
	if strings.Join(res.Files, ",") != "work/reports/y.xml" {
		t.Errorf("files = %q", res.Files)
	}
	if strings.Join(res.Problems, "\n") != "work/reports/x.xml does not parse as JUnit XML" {
		t.Errorf("problems = %q", res.Problems)
	}
}

func TestIsCandidate(t *testing.T) {
	for name, want := range map[string]bool{
		"TEST-com.x.FooTest.xml": true,
		"test-foo.XML":           true,
		"junit.xml":              true,
		"my-junit-report.xml":    true,
		"test_results.xml":       true,
		"unittest-result.xml":    true,
		"api.tests.xml":          true,
		"report.xml":             true,
		"results.xml":            true,
		"pom.xml":                false,
		"checkstyle.xml":         false,
		"junit.json":             false,
		"result-test.xml":        false,
	} {
		if got := isCandidate(name, false); got != want {
			t.Errorf("isCandidate(%q) = %v, want %v", name, got, want)
		}
	}
	if !isCandidate("pom.xml", true) {
		t.Error("an xml under a reports folder is not a candidate")
	}
}

func TestLooksLikeJUnit(t *testing.T) {
	for in, want := range map[string]bool{
		"\xef\xbb\xbf<?xml version=\"1.0\"?>\n<!-- hi -->\n  <testsuites>": true,
		"<testsuite name=\"x\">": true,
		"<testsuite/>":           true,
		"<testsuitex>":           false,
		"<project>":              false,
		"<!-- unterminated":      false,
		"":                       false,
	} {
		if got := looksLikeJUnit([]byte(in)); got != want {
			t.Errorf("looksLikeJUnit(%q) = %v, want %v", in, got, want)
		}
	}
}
