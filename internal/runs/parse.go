package runs

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The actions/runner writes these lines to its diag logs (_diag/Runner_*.log
// and _diag/Worker_*.log): JobDispatcher's "Job <name> completed with
// result: <Result>", StepsRunner's "Processing step: DisplayName='<name>'"
// and "Step result: <Result>", and the worker's "Job result after all job
// steps finish: <Result>".
var (
	jobResultRe  = regexp.MustCompile(`(?:completed with result|Job result after all job steps finish): (\w+)`)
	stepNameRe   = regexp.MustCompile(`Processing step: DisplayName='([^'\n]*)'`)
	stepResultRe = regexp.MustCompile(`Step result: (\w+)`)
	// check-run's descriptions: "failed at step X, 12s" and "timed out at
	// step X, 12s".
	checkStepRe = regexp.MustCompile(`(?:failed|timed out) at step (.+?), \d+s`)
)

// ParseRunnerLog reads a runner job's conclusion and failed step from the
// runner's diag logs. Either is "" when the log doesn't say.
func ParseRunnerLog(log string) (conclusion, failedStep string) {
	if m := jobResultRe.FindAllStringSubmatch(log, -1); len(m) > 0 {
		conclusion = runnerResult(m[len(m)-1][1])
	}
	step := ""
	for _, line := range strings.Split(log, "\n") {
		if m := stepNameRe.FindStringSubmatch(line); m != nil {
			step = m[1]
			continue
		}
		if m := stepResultRe.FindStringSubmatch(line); m != nil && step != "" && failedStep == "" {
			if r := runnerResult(m[1]); r == Failure {
				failedStep = step
			}
		}
	}
	return conclusion, failedStep
}

// runnerResult maps the runner's TaskResult names to conclusions.
func runnerResult(s string) string {
	switch strings.ToLower(s) {
	case "succeeded", "succeededwithissues", "skipped":
		return Success
	case "failed":
		return Failure
	case "canceled", "cancelled", "abandoned":
		return Cancelled
	}
	return ""
}

// CheckStep reads the failed step from a check job's description.
func CheckStep(description string) string {
	if m := checkStepRe.FindStringSubmatch(description); m != nil {
		return m[1]
	}
	return ""
}

// CheckConclusion maps a check result's state to a conclusion.
func CheckConclusion(state string) string {
	switch state {
	case "success":
		return Success
	case "failure":
		return Failure
	case "error":
		return Error
	}
	return Unknown
}

// TailLines returns the last n lines of s.
func TailLines(s string, n int) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" || n <= 0 {
		return []string{}
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ParseRunURL reads a forge run link, like
// https://github.com/owner/name/actions/runs/123 or .../runs/123/job/456
// (any host, for GitHub Enterprise), into the repo and run id.
func ParseRunURL(s string) (repo string, runID int64, ok bool) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", 0, false
	}
	before, after, found := strings.Cut(strings.Trim(u.Path, "/"), "/actions/runs/")
	if !found || strings.Count(before, "/") != 1 || strings.HasPrefix(before, "/") {
		return "", 0, false
	}
	id, _, _ := strings.Cut(after, "/")
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return before, n, true
}
