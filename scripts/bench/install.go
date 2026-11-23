package main

// bench install: the time from the install one-liner to the first green
// job, per phase, for one e2e job (.github/workflows/e2e.yml).
//
// Inputs:
//
//   - -marks: lines "NAME begin|end UNIXSECONDS" the workflow wrote around
//     the one-line install command, which also runs setup (command), and
//     around connect + repo add (connect).
//   - -setup: the setup --json line the command printed; its timings become
//     setup.<phase> rows, and the rest of the command is download_verify
//     (download, sha256 check, moving the binary into place).
//   - -statuses (optional): the commit's GitHub statuses as the API lists
//     them; the first "pending" and the "success" of -ctx after connect ends
//     give first_job_pickup and first_job_green (to the second).
//
// install_ms is the whole command: from its start until setup returns,
// which is after the service answers and doctor ran.
// to_green_ms adds connect, pickup and green: what a person waits from the
// command to the first green job. With -budget-ms, install_ms over the
// budget exits 1. -budget-excludes names phases the budget leaves out
// (budgeted_ms is install_ms without them): on macOS the guest image pull,
// whose time is the registry's and swings 32 to 144 s on hosted runners.

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// InstallPhase is one row of the install report.
type InstallPhase struct {
	Phase string `json:"phase"`
	MS    int64  `json:"ms"`
}

// InstallReport is one OS's install-to-green timing.
type InstallReport struct {
	OS           string         `json:"os"`
	Phases       []InstallPhase `json:"phases"`
	InstallMS    int64          `json:"install_ms"`
	ToGreenMS    int64          `json:"to_green_ms,omitempty"` // 0 when no job ran
	BudgetMS     int64          `json:"budget_ms,omitempty"`
	BudgetedMS   int64          `json:"budgeted_ms"` // install_ms minus the excluded phases
	Excluded     []string       `json:"budget_excludes,omitempty"`
	WithinBudget bool           `json:"within_budget"`
}

type span struct{ begin, end float64 }

// parseMarks reads "NAME begin|end SECONDS" lines into spans.
func parseMarks(r io.Reader) (map[string]*span, error) {
	out := map[string]*span{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 || (f[1] != "begin" && f[1] != "end") {
			return nil, fmt.Errorf("marks line %d: want NAME begin|end SECONDS, got %q", n, sc.Text())
		}
		t, err := strconv.ParseFloat(f[2], 64)
		if err != nil {
			return nil, fmt.Errorf("marks line %d: %v", n, err)
		}
		s := out[f[0]]
		if s == nil {
			s = &span{}
			out[f[0]] = s
		}
		if f[1] == "begin" {
			s.begin = t
		} else {
			s.end = t
		}
	}
	return out, sc.Err()
}

func (s *span) ms() (int64, bool) {
	if s == nil || s.begin == 0 || s.end < s.begin {
		return 0, false
	}
	return int64(math.Round((s.end - s.begin) * 1000)), true
}

type commitStatus struct {
	State     string    `json:"state"`
	Context   string    `json:"context"`
	CreatedAt time.Time `json:"created_at"`
}

// jobTimes finds when ctx went pending and then success, after since.
func jobTimes(statuses []commitStatus, ctx string, since time.Time) (pending, success time.Time, err error) {
	for _, s := range statuses {
		if s.Context != ctx || s.CreatedAt.Before(since.Truncate(time.Second)) {
			continue
		}
		switch s.State {
		case "pending":
			if pending.IsZero() || s.CreatedAt.Before(pending) {
				pending = s.CreatedAt
			}
		case "success":
			if success.IsZero() || s.CreatedAt.Before(success) {
				success = s.CreatedAt
			}
		}
	}
	if success.IsZero() {
		return pending, success, fmt.Errorf("no success status for %s", ctx)
	}
	if pending.IsZero() || pending.After(success) {
		pending = success
	}
	return pending, success, nil
}

func nonNeg(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// BuildInstall assembles the report from the parsed inputs.
func BuildInstall(osName string, marks map[string]*span, setup []byte, statuses []commitStatus, ctx string, budget int64, excludes []string) (InstallReport, error) {
	rep := InstallReport{OS: osName, BudgetMS: budget, Excluded: excludes}
	cmd, ok := marks["command"].ms()
	if !ok {
		return rep, errors.New("no command marks")
	}
	var sv struct {
		Timings []InstallPhase `json:"timings"`
		TotalMS int64          `json:"total_ms"`
	}
	if err := json.Unmarshal(setup, &sv); err != nil {
		return rep, fmt.Errorf("setup output: %v", err)
	}
	if len(sv.Timings) == 0 {
		return rep, errors.New("setup output has no timings")
	}
	if sv.TotalMS > cmd {
		return rep, fmt.Errorf("setup took %d ms, longer than the whole command (%d ms)", sv.TotalMS, cmd)
	}
	rep.Phases = append(rep.Phases, InstallPhase{"download_verify", cmd - sv.TotalMS})
	for _, p := range sv.Timings {
		rep.Phases = append(rep.Phases, InstallPhase{"setup." + p.Phase, p.MS})
	}
	rep.InstallMS = cmd
	rep.BudgetedMS = cmd
	for _, x := range excludes {
		found := false
		for _, p := range rep.Phases {
			if p.Phase == x {
				rep.BudgetedMS -= p.MS
				found = true
			}
		}
		if !found {
			return rep, fmt.Errorf("budget excludes %s, which the report has no phase for", x)
		}
	}
	rep.WithinBudget = budget <= 0 || rep.BudgetedMS <= budget
	if statuses == nil {
		return rep, nil
	}
	c := marks["connect"]
	cms, ok := c.ms()
	if !ok {
		return rep, errors.New("statuses given but no connect marks")
	}
	connectEnd := time.Unix(0, int64(c.end*1e9)).UTC()
	pending, success, err := jobTimes(statuses, ctx, time.Unix(0, int64(c.begin*1e9)).UTC())
	if err != nil {
		return rep, err
	}
	pickup := nonNeg(pending.Sub(connectEnd))
	green := nonNeg(success.Sub(pending))
	rep.Phases = append(rep.Phases, InstallPhase{"connect", cms}, InstallPhase{"first_job_pickup", pickup}, InstallPhase{"first_job_green", green})
	rep.ToGreenMS = rep.InstallMS + cms + pickup + green
	return rep, nil
}

// installHeading starts the hand-kept Install section, the last in
// docs/BENCHMARKS.md.
const installHeading = "\n## Install\n"

// keepInstall carries the Install section of old over to the end of text.
func keepInstall(text, old string) string {
	i := strings.Index(old, installHeading)
	if i < 0 || strings.Contains(text, installHeading) {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n" + old[i:]
}

// InstallMarkdown is the job summary table.
func InstallMarkdown(rep InstallReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Install to green: %s\n\n| phase | seconds |\n| --- | ---: |\n", rep.OS)
	for _, p := range rep.Phases {
		fmt.Fprintf(&b, "| %s | %.1f |\n", p.Phase, float64(p.MS)/1000)
	}
	fmt.Fprintf(&b, "| **install (command to setup done)** | **%.1f** |\n", float64(rep.InstallMS)/1000)
	if rep.ToGreenMS > 0 {
		fmt.Fprintf(&b, "| **command to first green job** | **%.1f** |\n", float64(rep.ToGreenMS)/1000)
	}
	what := "install"
	if len(rep.Excluded) > 0 {
		what = fmt.Sprintf("install without %s (%.1f s)", strings.Join(rep.Excluded, ", "), float64(rep.BudgetedMS)/1000)
	}
	switch {
	case rep.BudgetMS <= 0:
		b.WriteString("\nNo install budget set.\n")
	case rep.WithinBudget:
		fmt.Fprintf(&b, "\nThe %s is within the budget of %.1f s.\n", what, float64(rep.BudgetMS)/1000)
	default:
		fmt.Fprintf(&b, "\n**The %s is over the budget of %.1f s.**\n", what, float64(rep.BudgetMS)/1000)
	}
	return b.String()
}

func runInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	osName := fs.String("os", "", "linux, macos or windows")
	marksPath := fs.String("marks", "", "timing marks file")
	setupPath := fs.String("setup", "", "setup --json output")
	statusPath := fs.String("statuses", "", "GitHub statuses JSON (optional)")
	ctx := fs.String("ctx", "", "status context of the first job")
	budget := fs.Int64("budget-ms", 0, "install budget in ms; 0 reports only")
	excludes := fs.String("budget-excludes", "", "comma-separated phases the budget leaves out")
	js := fs.String("json", "", "JSON output")
	md := fs.String("md", "", "Markdown output (appended)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *osName == "" || *marksPath == "" || *setupPath == "" || *js == "" || *md == "" || (*statusPath != "" && *ctx == "") {
		fmt.Fprintln(stderr, "bench install: -os, -marks, -setup, -json and -md are required, and -ctx with -statuses")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "bench install:", err); return 1 }
	mf, err := os.Open(*marksPath)
	if err != nil {
		return fail(err)
	}
	marks, err := parseMarks(mf)
	mf.Close()
	if err != nil {
		return fail(err)
	}
	setup, err := os.ReadFile(*setupPath)
	if err != nil {
		return fail(err)
	}
	var statuses []commitStatus
	if *statusPath != "" {
		if err := readJSON(*statusPath, &statuses); err != nil {
			return fail(err)
		}
		if statuses == nil {
			statuses = []commitStatus{}
		}
	}
	var ex []string
	for _, x := range strings.Split(*excludes, ",") {
		if x = strings.TrimSpace(x); x != "" {
			ex = append(ex, x)
		}
	}
	rep, err := BuildInstall(*osName, marks, setup, statuses, *ctx, *budget, ex)
	if err != nil {
		return fail(err)
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(*js), 0o755); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(*js, append(data, '\n'), 0o644); err != nil {
		return fail(err)
	}
	text := InstallMarkdown(rep)
	f, err := os.OpenFile(*md, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fail(err)
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprint(stdout, text)
	if !rep.WithinBudget {
		fmt.Fprintf(stdout, "bench install: FAIL: install took %.1f s (%.1f s budgeted), over the %.1f s budget\n", float64(rep.InstallMS)/1000, float64(rep.BudgetedMS)/1000, float64(rep.BudgetMS)/1000)
		return 1
	}
	return 0
}
