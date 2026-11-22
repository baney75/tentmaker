package runs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/baney75/tentmaker/internal/config"
)

// Test statuses.
const (
	TestPassed  = "passed"
	TestFailed  = "failed"
	TestSkipped = "skipped"
)

// Where test results came from.
const (
	SourceJUnit      = "junit"
	SourceGoTestJSON = "go-test-json"
)

// Caps on what a run keeps of its tests, so one run's file stays small and
// a record's summary fits the pool view.
const (
	// MaxCases is how many test cases a run keeps: every failure first (up
	// to MaxFailedCases), then passed and skipped ones.
	MaxCases = 5000
	// MaxFailedCases caps the failed cases a run keeps.
	MaxFailedCases = 500
	// MaxSummaryFailures is how many failing test ids a record carries.
	MaxSummaryFailures = 50
	// MaxSummarySuites is how many whole suites a record names.
	MaxSummarySuites = 100
	// MaxMessage and MaxDetail cap a failure's message and its text (the
	// stack or output), in bytes.
	MaxMessage = 1 << 10
	MaxDetail  = 4 << 10
	// testsFileMax caps a stored tests file when it is read back.
	testsFileMax = 8 << 20
)

// TestCase is one test in one run.
type TestCase struct {
	// Suite is the JUnit classname (or suite name) or the Go package.
	Suite   string  `json:"suite,omitempty"`
	Name    string  `json:"name"`
	Status  string  `json:"status"` // TestPassed, TestFailed or TestSkipped
	Seconds float64 `json:"seconds,omitempty"`
	// Message is the failure's one-line message, Detail its text (a stack
	// trace or the test's output). Both are capped and redacted.
	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// File is the report the case came from, relative to the job's
	// workspace, or "log" for go test -json output in the job's log.
	File   string `json:"file,omitempty"`
	Source string `json:"source,omitempty"` // SourceJUnit or SourceGoTestJSON
	// Partial says the case's suite was not seen whole (a go test package
	// cut from the log tail), so its absent tests say nothing.
	Partial bool `json:"partial,omitempty"`
}

// ID names a test across runs: "suite.name", or the name alone.
func (c TestCase) ID() string {
	if c.Suite == "" {
		return c.Name
	}
	return c.Suite + "." + c.Name
}

// TestSummary is what a record carries about its tests.
type TestSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Failures are the ids (TestCase.ID) of failed tests, at most
	// MaxSummaryFailures; Truncated says there were more.
	Failures  []string `json:"failures,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	// Suites are the suites reported whole, at most MaxSummarySuites. Only
	// for a test in one of them does its absence from Failures mean passed.
	Suites []string `json:"suites,omitempty"`
}

// TestResults is every test a run reported, as the device keeps it.
type TestResults struct {
	ID     string     `json:"id"`
	Device string     `json:"device,omitempty"`
	Cases  []TestCase `json:"cases"`
	// Files are the reports read, relative to the job's workspace.
	Files []string `json:"files,omitempty"`
	// Problems say what was skipped: a report too large, too many files, a
	// report that did not parse. Never a secret or a host path.
	Problems []string `json:"problems,omitempty"`
	// Dropped counts cases past MaxCases or MaxFailedCases.
	Dropped int `json:"dropped,omitempty"`
}

// Summary counts r's cases.
func (r TestResults) Summary() *TestSummary {
	if len(r.Cases) == 0 && r.Dropped == 0 {
		return nil
	}
	s := &TestSummary{}
	src, suites := map[string]bool{}, map[string]bool{}
	for _, c := range r.Cases {
		if c.Suite != "" {
			suites[c.Suite] = suites[c.Suite] || c.Partial
		}
		s.Total++
		switch c.Status {
		case TestFailed:
			s.Failed++
			if len(s.Failures) < MaxSummaryFailures {
				s.Failures = append(s.Failures, c.ID())
			} else {
				s.Truncated = true
			}
		case TestSkipped:
			s.Skipped++
		default:
			s.Passed++
		}
		if c.Source != "" {
			src[c.Source] = true
		}
	}
	if r.Dropped > 0 {
		s.Truncated = true
	}
	for k := range src {
		s.Sources = append(s.Sources, k)
	}
	sort.Strings(s.Sources)
	for k, partial := range suites {
		if !partial {
			s.Suites = append(s.Suites, k)
		}
	}
	sort.Strings(s.Suites)
	s.Suites = s.Suites[:min(len(s.Suites), MaxSummarySuites)]
	return s
}

// Merge adds b's cases to a. A test a already has (same suite, name and
// status source) is kept from a; JUnit cases win over go test -json ones
// for the same test, since they carry the report's own message.
func (a TestResults) Merge(b TestResults) TestResults {
	seen := map[string]int{}
	for i, c := range a.Cases {
		seen[c.ID()] = i
	}
	for _, c := range b.Cases {
		if i, ok := seen[c.ID()]; ok {
			if a.Cases[i].Source != SourceJUnit && c.Source == SourceJUnit {
				a.Cases[i] = c
			}
			continue
		}
		seen[c.ID()] = len(a.Cases)
		a.Cases = append(a.Cases, c)
	}
	a.Files = append(a.Files, b.Files...)
	a.Problems = append(a.Problems, b.Problems...)
	a.Dropped += b.Dropped
	return a.Capped()
}

// Capped orders r's cases failed first (in report order) and cuts them to
// MaxFailedCases failures and MaxCases in all, counting what it drops. It
// also caps each message and detail.
func (r TestResults) Capped() TestResults {
	var failed, rest []TestCase
	for _, c := range r.Cases {
		c.Message = capText(c.Message, MaxMessage)
		c.Detail = capText(c.Detail, MaxDetail)
		if c.Status == TestFailed {
			failed = append(failed, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(failed) > MaxFailedCases {
		r.Dropped += len(failed) - MaxFailedCases
		failed = failed[:MaxFailedCases]
	}
	out := append(failed, rest...)
	if len(out) > MaxCases {
		r.Dropped += len(out) - MaxCases
		out = out[:MaxCases]
	}
	r.Cases = out
	return r
}

// capText cuts s to n bytes on a rune boundary and marks the cut.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len(" [cut]")
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + " [cut]"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// Redacted is r with every message and detail passed through Redact.
func (r TestResults) Redacted() TestResults {
	cases := make([]TestCase, len(r.Cases))
	for i, c := range r.Cases {
		c.Message = string(Redact([]byte(c.Message)))
		c.Detail = string(Redact([]byte(c.Detail)))
		cases[i] = c
	}
	r.Cases = cases
	return r
}

// Failed lists r's failed cases.
func (r TestResults) Failed() []TestCase {
	var out []TestCase
	for _, c := range r.Cases {
		if c.Status == TestFailed {
			out = append(out, c)
		}
	}
	return out
}

const testsDir = "tests"

// TestsPath is where id's test results live.
func (s *Store) TestsPath(id string) string { return filepath.Join(s.dir, testsDir, id+".json") }

// PutTests keeps id's test results, redacted and capped, and puts their
// summary on id's record. A record must exist.
func (s *Store) PutTests(id string, t TestResults) error {
	if !ValidID(id) {
		return fmt.Errorf("record id %q is not a runner name", trunc(id, 80))
	}
	t.ID = id
	t = t.Capped().Redacted()
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	i := -1
	for k := len(s.recs) - 1; k >= 0; k-- {
		if s.recs[k].ID == id {
			i = k
			break
		}
	}
	if i < 0 {
		return ErrNotFound
	}
	if err := config.WriteAtomic(s.TestsPath(id), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("keep the job's test results: %w", err)
	}
	s.recs[i].Tests = t.Summary()
	return s.saveLocked()
}

// Tests reads id's kept test results, or ErrNotFound.
func (s *Store) Tests(id string) (TestResults, error) {
	if !ValidID(id) {
		return TestResults{}, ErrNotFound
	}
	f, err := os.Open(s.TestsPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return TestResults{}, ErrNotFound
	}
	if err != nil {
		return TestResults{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, testsFileMax))
	if err != nil {
		return TestResults{}, err
	}
	var t TestResults
	if err := json.Unmarshal(b, &t); err != nil {
		return TestResults{}, fmt.Errorf("the kept test results of %s don't parse: %w", id, err)
	}
	return t, nil
}

// TestsAnswer is a run's test results as the control API and the mesh
// serve them.
type TestsAnswer struct {
	Record  Record      `json:"record"`
	Results TestResults `json:"results"`
}

// hasFailure reports whether the summary names test id as failed.
// passed says s positively reports test id as passed: it is not among the
// failures of a whole summary that covers its suite.
func (s *TestSummary) passed(id string) bool {
	if s == nil || s.Truncated || s.hasFailure(id) {
		return false
	}
	for _, su := range s.Suites {
		if strings.HasPrefix(id, su+".") {
			return true
		}
	}
	return false
}

func (s *TestSummary) hasFailure(id string) bool {
	if s == nil {
		return false
	}
	for _, f := range s.Failures {
		if f == id {
			return true
		}
	}
	return false
}
