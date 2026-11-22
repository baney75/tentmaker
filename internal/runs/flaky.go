package runs

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Flaky tests (docs/DESIGN.md, "Test results"). A test is flaky when it
//
//   - failed in one run and a later run of the same job on the same commit
//     reported its suite whole without it failing ("same-commit"), or
//   - changed between failing and passing at least FlakyFlips times in the
//     last FlakyWindow runs of the same job on the same ref ("flips").
//
// Both read the summaries on the records alone, so the pool view answers
// without fetching any device's test files. A run whose summary was cut
// (Truncated), or one that did not report a test's suite whole, says
// nothing about that test, so it counts for neither rule.

// Flaky rule limits.
const (
	FlakyWindow = 10
	FlakyFlips  = 2
)

// Flaky reasons.
const (
	FlakySameCommit = "same-commit"
	FlakyFlipped    = "flips"
)

// FlakyTest is one test that looks flaky in one job.
type FlakyTest struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow,omitempty"`
	Job      string `json:"job"`
	Test     string `json:"test"`
	Reason   string `json:"reason"` // FlakySameCommit or FlakyFlipped
	// Detail says what was seen, for a person.
	Detail string `json:"detail"`
	SHA    string `json:"sha,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Flips  int    `json:"flips,omitempty"`
	// Runs are the record ids that show it, oldest first.
	Runs     []string  `json:"runs"`
	LastSeen time.Time `json:"last_seen"`
}

// jobKey names one job of one workflow of one repo across runs.
type jobKey struct{ repo, workflow, job string }

func keyOf(r Record) jobKey { return jobKey{strings.ToLower(r.Repo), r.Workflow, r.Job} }

// Flaky finds the flaky tests in recs, most recently seen first.
func Flaky(recs []Record) []FlakyTest {
	groups := map[jobKey][]Record{}
	for _, r := range recs {
		if r.Tests != nil && r.Ended() && r.Job != "" {
			groups[keyOf(r)] = append(groups[keyOf(r)], r)
		}
	}
	out := []FlakyTest{}
	for _, g := range groups {
		sort.SliceStable(g, func(i, k int) bool { return g[i].at().Before(g[k].at()) })
		found := map[string]bool{}
		for _, f := range sameCommit(g) {
			found[f.Test] = true
			out = append(out, f)
		}
		for _, f := range flipped(g) {
			if !found[f.Test] {
				out = append(out, f)
			}
		}
	}
	sort.SliceStable(out, func(i, k int) bool {
		if !out[i].LastSeen.Equal(out[k].LastSeen) {
			return out[i].LastSeen.After(out[k].LastSeen)
		}
		return out[i].Repo+out[i].Job+out[i].Test < out[k].Repo+out[k].Job+out[k].Test
	})
	return out
}

// sameCommit applies the same-commit rule to one job's runs, oldest first.
func sameCommit(g []Record) []FlakyTest {
	var out []FlakyTest
	seen := map[string]bool{}
	for i, a := range g {
		if a.SHA == "" {
			continue
		}
		for _, t := range a.Tests.Failures {
			if seen[t] {
				continue
			}
			for _, b := range g[i+1:] {
				if SameSHA(a.SHA, b.SHA) && b.Tests.passed(t) {
					seen[t] = true
					out = append(out, FlakyTest{Repo: a.Repo, Workflow: a.Workflow, Job: a.Job, Test: t,
						Reason: FlakySameCommit, SHA: a.SHA, Ref: b.Ref, Runs: []string{a.ID, b.ID}, LastSeen: b.at(),
						Detail: fmt.Sprintf("failed in %s and passed in %s on the same commit %s", a.ID, b.ID, shortSHA(a.SHA))})
					break
				}
			}
		}
	}
	return out
}

// flipped applies the flips rule to one job's runs, oldest first.
func flipped(g []Record) []FlakyTest {
	byRef := map[string][]Record{}
	var refs []string
	for _, r := range g {
		if r.Ref == "" || r.Tests.Total == 0 {
			continue
		}
		if _, ok := byRef[r.Ref]; !ok {
			refs = append(refs, r.Ref)
		}
		byRef[r.Ref] = append(byRef[r.Ref], r)
	}
	var out []FlakyTest
	for _, ref := range refs {
		rs := byRef[ref]
		if len(rs) > FlakyWindow {
			rs = rs[len(rs)-FlakyWindow:]
		}
		var tests []string
		named := map[string]bool{}
		for _, r := range rs {
			for _, t := range r.Tests.Failures {
				if !named[t] {
					named[t] = true
					tests = append(tests, t)
				}
			}
		}
		for _, t := range tests {
			flips, last := 0, 0
			var ids []string
			var seen time.Time
			for _, r := range rs {
				state := 0
				switch {
				case r.Tests.hasFailure(t):
					state = -1
				case r.Tests.passed(t):
					state = 1
				default:
					continue // a cut summary, or one without t's suite, says nothing about t
				}
				if last != 0 && state != last {
					flips++
					seen = r.at()
				}
				last = state
				ids = append(ids, r.ID)
			}
			if flips >= FlakyFlips {
				out = append(out, FlakyTest{Repo: rs[0].Repo, Workflow: rs[0].Workflow, Job: rs[0].Job, Test: t,
					Reason: FlakyFlipped, Ref: ref, Flips: flips, Runs: ids, LastSeen: seen,
					Detail: fmt.Sprintf("changed between failing and passing %d times in the last %d runs on %s", flips, len(ids), ref)})
			}
		}
	}
	return out
}

// shortSHA is the first 7 characters of a commit id.
func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
