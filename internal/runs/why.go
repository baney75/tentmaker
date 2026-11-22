package runs

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// why_failed limits: the answer stays small enough for an agent's context.
const (
	WhyMaxFailures = 5
	WhyMessage     = 300
	WhyDetail      = 600
	WhyDetailLines = 8
	WhyBefore      = 10
	WhyAfter       = 20
	WhyFallback    = 30
	WhyLineMax     = 300
)

// WhyFailure is one failing test in a WhyAnswer.
type WhyFailure struct {
	Test    string `json:"test"`
	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
	File    string `json:"file,omitempty"`
}

// WhyAnswer says why a job failed, small and structured.
type WhyAnswer struct {
	ID         string `json:"id"`
	Device     string `json:"device"`
	Repo       string `json:"repo"`
	Workflow   string `json:"workflow,omitempty"`
	Job        string `json:"job"`
	Ref        string `json:"ref,omitempty"`
	SHA        string `json:"sha,omitempty"`
	RunID      int64  `json:"run_id,omitempty"`
	RunURL     string `json:"run_url,omitempty"`
	Conclusion string `json:"conclusion"`
	FailedStep string `json:"failed_step,omitempty"`
	Note       string `json:"note,omitempty"`
	// Failures are the first WhyMaxFailures failing tests; MoreFailures
	// counts the rest.
	Failures     []WhyFailure `json:"failures"`
	MoreFailures int          `json:"more_failures,omitempty"`
	// Log is the log around the first error line (ErrorLine, 1-based in
	// the kept tail), or the tail's last lines when no line looks like
	// one (ErrorLine 0). Timestamps are cut and the text is redacted.
	Log       []string `json:"log"`
	ErrorLine int      `json:"error_line,omitempty"`
	// Flaky says the failure looks flaky, and FlakyReason why.
	Flaky       bool   `json:"flaky"`
	FlakyReason string `json:"flaky_reason,omitempty"`
	// KeptUntil is set while the job's sandbox is kept for debugging.
	KeptUntil time.Time `json:"kept_until,omitzero"`
}

// errorLine matches a log line that reports an error.
var errorLine = regexp.MustCompile(`##\[error\]|^\s*--- FAIL|^panic: |^FAIL\s|(^|\s)Error: |npm ERR!|^error(\[E\d+\])?: |Process completed with exit code [1-9]`)

// stamp is the timestamp the forge puts before each log line.
var stamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)

// LogExcerpt cuts log to the lines around its first error: WhyBefore lines
// before it and WhyAfter after. With no error line it is the last
// WhyFallback lines and at is 0.
func LogExcerpt(log []byte) (lines []string, at int) {
	s := strings.TrimRight(strings.ReplaceAll(string(Redact(log)), "\r\n", "\n"), "\n")
	if s == "" {
		return []string{}, 0
	}
	all := strings.Split(s, "\n")
	for i, l := range all {
		all[i] = capText(stamp.ReplaceAllString(l, ""), WhyLineMax)
	}
	for i, l := range all {
		if errorLine.MatchString(l) {
			from, to := max(0, i-WhyBefore), min(len(all), i+WhyAfter+1)
			return all[from:to], i + 1
		}
	}
	if len(all) > WhyFallback {
		all = all[len(all)-WhyFallback:]
	}
	return all, 0
}

// Why explains rec: tests are its kept results (nil if none), log its kept
// log tail, and history the pool's records, read for flakiness.
func Why(rec Record, tests *TestResults, log []byte, history []Record) WhyAnswer {
	a := WhyAnswer{ID: rec.ID, Device: rec.Device, Repo: rec.Repo, Workflow: rec.Workflow, Job: rec.Job,
		Ref: rec.Ref, SHA: rec.SHA, RunID: rec.RunID, RunURL: rec.RunURL, Conclusion: rec.Conclusion,
		FailedStep: rec.FailedStep, Note: rec.Note, KeptUntil: rec.KeptUntil, Failures: []WhyFailure{}}
	var failed []string
	if tests != nil {
		for _, c := range tests.Failed() {
			failed = append(failed, c.ID())
			if len(a.Failures) == WhyMaxFailures {
				a.MoreFailures++
				continue
			}
			a.Failures = append(a.Failures, WhyFailure{Test: c.ID(), File: c.File,
				Message: RedactString(capText(c.Message, WhyMessage)), Detail: RedactString(shortDetail(c.Detail))})
		}
		a.MoreFailures += tests.Dropped
	} else if rec.Tests != nil {
		for _, t := range rec.Tests.Failures {
			failed = append(failed, t)
			if len(a.Failures) == WhyMaxFailures {
				a.MoreFailures++
				continue
			}
			a.Failures = append(a.Failures, WhyFailure{Test: t})
		}
	}
	a.Log, a.ErrorLine = LogExcerpt(log)
	a.Flaky, a.FlakyReason = looksFlaky(rec, failed, history)
	return a
}

// shortDetail is a failure's first WhyDetailLines lines, at most WhyDetail
// bytes.
func shortDetail(s string) string {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", WhyDetailLines+1)
	if len(lines) > WhyDetailLines {
		lines = append(lines[:WhyDetailLines], "...")
	}
	return capText(strings.Join(lines, "\n"), WhyDetail)
}

// looksFlaky says whether rec's failure looks flaky: the same job passed on
// the same commit, or one of its failing tests is flaky in history.
func looksFlaky(rec Record, failed []string, history []Record) (bool, string) {
	if !rec.Failed() {
		return false, ""
	}
	k := keyOf(rec)
	for _, h := range history {
		if h.ID != rec.ID && h.Conclusion == Success && keyOf(h) == k && SameSHA(h.SHA, rec.SHA) {
			return true, fmt.Sprintf("the same job passed on the same commit in %s", h.ID)
		}
	}
	if len(failed) == 0 {
		return false, ""
	}
	want := map[string]bool{}
	for _, t := range failed {
		want[t] = true
	}
	for _, f := range Flaky(append(append([]Record(nil), history...), rec)) {
		if want[f.Test] && strings.EqualFold(f.Repo, rec.Repo) && f.Job == rec.Job && f.Workflow == rec.Workflow {
			return true, f.Test + " is flaky: " + f.Detail
		}
	}
	return false, ""
}
