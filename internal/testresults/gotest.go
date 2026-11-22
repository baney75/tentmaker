package testresults

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/baney75/tentmaker/internal/runs"
)

// event is one test2json event.
type event struct {
	Action  string   `json:"Action"`
	Package string   `json:"Package"`
	Test    string   `json:"Test"`
	Output  string   `json:"Output"`
	Elapsed *float64 `json:"Elapsed"`
}

// actions are the test2json actions; a JSON object with any other Action is
// not a test2json event.
var actions = map[string]bool{
	"start": true, "run": true, "pause": true, "cont": true, "pass": true,
	"bench": true, "fail": true, "output": true, "skip": true,
	"build-output": true, "build-fail": true, "attr": true,
}

// logFile is TestCase.File for a case read from the job's log.
const logFile = "log"

// ParseGoTestJSON reads `go test -json` events from a job's log. Lines may
// carry a prefix before the JSON object, such as GitHub's
// "2026-11-22T10:00:00.1234567Z " timestamp; anything that is not a test2json
// event is ignored. Returns one case per (Package, Test) with a final
// pass/fail/skip action; a failed test's Detail is its own output lines
// (Action "output"), capped. Message is the first output line that looks like
// an error (contains "_test.go:" or "Error" or "panic:"), else empty. Package-level
// events (no Test) only say whether a package was seen whole, from its
// "start" to its pass or fail; a case of any other package is Partial.
// Subtests ("TestX/sub") are their own cases.
func ParseGoTestJSON(log []byte) []runs.TestCase {
	type key struct{ pkg, test string }
	type state struct {
		out     strings.Builder
		status  string
		seconds float64
	}
	var order []key
	tests := map[key]*state{}
	started, whole := map[string]bool{}, map[string]bool{}
	for len(log) > 0 {
		line := log
		if i := bytes.IndexByte(log, '\n'); i >= 0 {
			line, log = log[:i], log[i+1:]
		} else {
			log = nil
		}
		ev, ok := parseEvent(line)
		if ok && ev.Test == "" {
			started[ev.Package] = started[ev.Package] || ev.Action == "start"
			whole[ev.Package] = whole[ev.Package] || started[ev.Package] && (ev.Action == "pass" || ev.Action == "fail")
		}
		if !ok || ev.Test == "" {
			continue
		}
		k := key{ev.Package, ev.Test}
		st := tests[k]
		if st == nil {
			st = &state{}
			tests[k] = st
			order = append(order, k)
		}
		switch ev.Action {
		case "output":
			if room := maxText - st.out.Len(); room > 0 {
				o := ev.Output
				if len(o) > room {
					o = o[:room]
				}
				st.out.WriteString(o)
			}
		case "pass":
			st.status = runs.TestPassed
		case "fail":
			st.status = runs.TestFailed
		case "skip":
			st.status = runs.TestSkipped
		}
		if ev.Elapsed != nil && *ev.Elapsed >= 0 && (ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip") {
			st.seconds = *ev.Elapsed
		}
	}
	var cases []runs.TestCase
	for _, k := range order {
		st := tests[k]
		if st.status == "" {
			continue
		}
		c := runs.TestCase{
			Suite:   k.pkg,
			Name:    k.test,
			Status:  st.status,
			Seconds: st.seconds,
			File:    logFile,
			Source:  runs.SourceGoTestJSON,
			Partial: !whole[k.pkg],
		}
		if st.status == runs.TestFailed {
			lines := ownLines(st.out.String())
			c.Detail = strings.Join(lines, "\n")
			c.Message = errorLine(lines)
		}
		cases = append(cases, c)
	}
	return cases
}

// parseEvent finds a test2json event in one log line, after any prefix.
func parseEvent(line []byte) (event, bool) {
	line = bytes.TrimRight(line, " \t\r")
	i := bytes.IndexByte(line, '{')
	if i < 0 || !bytes.HasSuffix(line, []byte("}")) || !bytes.Contains(line[i:], []byte(`"Action"`)) {
		return event{}, false
	}
	var ev event
	if err := json.Unmarshal(line[i:], &ev); err != nil || !actions[ev.Action] {
		return event{}, false
	}
	return ev, true
}

// ownLines is a test's output without go test's framing lines (=== RUN,
// --- FAIL and the like) and without blank lines.
func ownLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, " \t\r")
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "=== ") || isResultLine(t) {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

func isResultLine(t string) bool {
	for _, p := range []string{"--- FAIL: ", "--- PASS: ", "--- SKIP: "} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// errorLine is the first line that looks like an error, trimmed.
func errorLine(lines []string) string {
	for _, l := range lines {
		if strings.Contains(l, "_test.go:") || strings.Contains(l, "Error") || strings.Contains(l, "panic:") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}
