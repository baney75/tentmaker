package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/daemon"
	"github.com/baney75/tentmaker/internal/runs"
)

const (
	usageTests = "tentmaker tests [JOB-ID] [--repo R] [--failed] [-n 20] [--json]\n\n" +
		"With a job id, that job's test cases, failed first. Without one, recent jobs that reported tests."
	usageFlaky = "tentmaker flaky [--repo R] [--since 14d] [--json]\n\n" +
		"Tests that failed and passed on the same commit, or flipped between failing and passing on one branch."
	usageWhy = "tentmaker why JOB-ID | --repo R --sha SHA | --repo R --run N [--json]\n\n" +
		"Why a job failed: its failed step, failing tests with messages, the log around the first error,\n" +
		"and whether it looks flaky. With --repo, the newest failed job of that commit or run."
	usageDebug = "tentmaker debug JOB-ID [--exec CMD] [--timeout 60s] [--max-bytes N] [--release] [--json]\n\n" +
		"Opens a shell in a failed job's sandbox, kept on this device when the repo sets\n" +
		"[repos.debug] keep_failed. --exec runs one command, prints its output and exits with its\n" +
		"exit code. --release ends the grace now so the sandbox is cleaned up. Needs root."
	// flakyDefault is the window flaky and why read.
	flakyDefault = "14d"
)

// ---------------------------------------------------------------- tests

// testRunsView is tests without a job id: recent jobs with test counts.
type testRunsView struct {
	Service string    `json:"service"`
	Runs    []testRun `json:"runs"`
	Errors  []string  `json:"errors,omitempty"`
}

type testRun struct {
	ID         string            `json:"id"`
	Repo       string            `json:"repo"`
	Workflow   string            `json:"workflow,omitempty"`
	Job        string            `json:"job"`
	Device     string            `json:"device"`
	Conclusion string            `json:"conclusion"`
	EndedAt    time.Time         `json:"ended_at,omitzero"`
	Tests      *runs.TestSummary `json:"tests"`
}

// testsData is job id's test results, from the service or, without it,
// this device's history.
func (a *app) testsData(ctx context.Context, id string, failedOnly bool) (runs.TestsAnswer, error) {
	if !runs.ValidID(id) {
		return runs.TestsAnswer{}, usage("tentmaker runs lists job ids, like tm-laptop-1a2b3c", "%q is not a job id", id)
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return runs.TestsAnswer{}, err
	}
	t, err := a.controlClient(cfg).Tests(ctx, id)
	var ce *control.Error
	switch {
	case err == nil:
	case errors.As(err, &ce) && ce.Status == 404:
		return t, fail("tentmaker runs lists the jobs the pool kept", "no device has job %s", id)
	case errors.Is(err, control.ErrNotRunning):
		st := a.localRuns(cfg)
		rec, ok := st.Get(id)
		if !ok {
			return t, fail("start the service to see the pool's jobs: sudo tentmaker setup", "this device has no record of %s", id)
		}
		res, terr := st.Tests(id)
		if terr != nil && !errors.Is(terr, core.ErrNotFound) {
			return t, fail("run it with sudo; test results are readable by root only", "read the test results: %v", terr)
		}
		t = runs.TestsAnswer{Record: rec, Results: res}
	default:
		return t, serviceErr(err)
	}
	if failedOnly {
		t.Results.Cases = t.Results.Failed()
	}
	t.Results.Cases = nonNil(t.Results.Cases)
	return t, nil
}

func (a *app) cmdTests(args []string) (any, error) {
	f := a.flags("tests", usageTests)
	repo := f.String("repo", "", "only this repository (owner/name)")
	failed := f.Bool("failed", false, "only failed tests (with a job id) or jobs with failed tests")
	n := f.Int("n", 20, "how many jobs, without a job id")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 1 {
		return nil, usage("usage: "+firstLine(usageTests), "unexpected argument %q", pos[1])
	}
	if len(pos) == 1 {
		if *repo != "" {
			return nil, usage("usage: "+firstLine(usageTests), "--repo goes with no job id")
		}
		t, err := a.testsData(a.ctx, pos[0], *failed)
		if err != nil {
			return nil, err
		}
		if !a.json {
			a.printTests(t, *failed)
		}
		return t, nil
	}
	if *n <= 0 || *n > runs.MaxLimit {
		return nil, usage(fmt.Sprintf("use -n from 1 to %d", runs.MaxLimit), "-n %d is out of range", *n)
	}
	v, err := a.runsData(a.ctx, runs.Query{Repo: *repo, Failed: *failed, Limit: runs.MaxLimit})
	if err != nil {
		return nil, err
	}
	out := testRunsView{Service: v.Service, Runs: []testRun{}, Errors: v.Errors}
	for _, r := range v.Records {
		if r.Tests == nil || (*failed && r.Tests.Failed == 0) {
			continue
		}
		out.Runs = append(out.Runs, testRun{ID: r.ID, Repo: r.Repo, Workflow: r.Workflow, Job: r.Job, Device: r.Device,
			Conclusion: r.Conclusion, EndedAt: r.EndedAt, Tests: r.Tests})
		if len(out.Runs) == *n {
			break
		}
	}
	if !a.json {
		a.printTestRuns(out)
	}
	return out, nil
}

func countsLine(s *runs.TestSummary) string {
	if s == nil {
		return "no tests reported"
	}
	return fmt.Sprintf("%d tests: %d passed, %d failed, %d skipped", s.Total, s.Passed, s.Failed, s.Skipped)
}

func (a *app) printTests(t runs.TestsAnswer, failedOnly bool) {
	w := a.env.Stdout
	r := t.Record
	fmt.Fprintf(w, "%s  %s  %s  %s  on %s\n", r.ID, a.paint(conclusionStatus(r.Conclusion), r.Conclusion), r.Repo, jobName(r), r.Device)
	sum := runs.TestResults{Cases: t.Results.Cases, Dropped: t.Results.Dropped}.Summary()
	if failedOnly && r.Tests != nil {
		sum = r.Tests
	}
	fmt.Fprintln(w, countsLine(sum))
	failed := t.Results.Failed()
	for _, c := range failed {
		fmt.Fprintf(w, "\n%s %s\n", a.paint("fail", "FAIL"), c.ID())
		if c.Message != "" {
			fmt.Fprintf(w, "  %s\n", c.Message)
		}
		if c.Detail != "" {
			for _, l := range strings.Split(strings.TrimRight(c.Detail, "\n"), "\n") {
				fmt.Fprintf(w, "    %s\n", l)
			}
		}
	}
	if !failedOnly {
		var rest []string
		for _, c := range t.Results.Cases {
			if c.Status != runs.TestFailed {
				rest = append(rest, c.Status+"  "+c.ID())
			}
		}
		if len(rest) > 0 {
			fmt.Fprintln(w)
			for _, l := range rest {
				fmt.Fprintln(w, l)
			}
		}
	}
	if t.Results.Dropped > 0 {
		fmt.Fprintf(w, "\n%d more tests were not kept.\n", t.Results.Dropped)
	}
	for _, p := range t.Results.Problems {
		fmt.Fprintln(w, "note: "+p)
	}
}

func (a *app) printTestRuns(v testRunsView) {
	if len(v.Runs) == 0 {
		fmt.Fprintln(a.env.Stdout, "No jobs with test results yet.")
	} else {
		tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CONCLUSION\tREPO\tJOB\tTOTAL\tFAILED\tSKIPPED\tID")
		for _, r := range v.Runs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", a.paint(conclusionStatus(r.Conclusion), r.Conclusion), r.Repo,
				trunc(jobName(runs.Record{ID: r.ID, Workflow: r.Workflow, Job: r.Job}), 32), r.Tests.Total, r.Tests.Failed, r.Tests.Skipped, r.ID)
		}
		tw.Flush()
	}
	if v.Service != "running" {
		fmt.Fprintln(a.env.Stdout, "The service is not running: this is this device's history only.")
	}
	for _, e := range v.Errors {
		fmt.Fprintln(a.env.Stdout, "not counted: "+e)
	}
}

// ---------------------------------------------------------------- flaky

func (a *app) flakyData(ctx context.Context, repo, since string) (control.FlakyAnswer, error) {
	d, ok := runs.ParseSince(since)
	if !ok {
		return control.FlakyAnswer{}, usage("use a window like 24h, 7d or 30d", "--since %q is not a window", since)
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return control.FlakyAnswer{}, err
	}
	q := runs.Query{Repo: repo, Since: a.env.Now().Add(-d), Limit: runs.MaxLimit}
	v, err := a.controlClient(cfg).Flaky(ctx, q)
	switch {
	case err == nil:
	case errors.Is(err, control.ErrNotRunning):
		st := a.localRuns(cfg)
		v = control.FlakyAnswer{Since: q.Since, Tests: runs.Flaky(st.List(q)),
			Errors: []string{"service not running: this is this device's history only"}}
	default:
		return v, serviceErr(err)
	}
	v.Tests = nonNil(v.Tests)
	return v, nil
}

func (a *app) cmdFlaky(args []string) (any, error) {
	f := a.flags("flaky", usageFlaky)
	repo := f.String("repo", "", "only this repository (owner/name)")
	since := f.String("since", flakyDefault, "window, like 24h, 7d or 30d")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+firstLine(usageFlaky), "unexpected argument %q", pos[0])
	}
	v, err := a.flakyData(a.ctx, *repo, *since)
	if err != nil {
		return nil, err
	}
	if !a.json {
		if len(v.Tests) == 0 {
			fmt.Fprintf(a.env.Stdout, "No flaky tests in the last %s.\n", *since)
		}
		for _, t := range v.Tests {
			fmt.Fprintf(a.env.Stdout, "%s  %s  %s\n  %s: %s\n", a.paint(daemon.Warn, t.Test), t.Repo,
				jobName(runs.Record{Workflow: t.Workflow, Job: t.Job}), t.Reason, t.Detail)
		}
		for _, e := range v.Errors {
			fmt.Fprintln(a.env.Stdout, "not counted: "+e)
		}
	}
	return v, nil
}

// ---------------------------------------------------------------- why

func (a *app) whyData(ctx context.Context, req control.WhyRequest) (runs.WhyAnswer, error) {
	if err := req.Check(); err != nil {
		return runs.WhyAnswer{}, usage("usage: "+firstLine(usageWhy), "%v", err)
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return runs.WhyAnswer{}, err
	}
	v, err := a.controlClient(cfg).Why(ctx, req)
	var ce *control.Error
	switch {
	case err == nil:
		v.Failures = nonNil(v.Failures)
		v.Log = nonNil(v.Log)
		return v, nil
	case errors.As(err, &ce) && ce.Status == 404:
		return v, fail("tentmaker runs --failed lists the failed jobs the pool kept", "%s", ce.Message)
	case errors.Is(err, control.ErrNotRunning):
		return a.localWhy(cfg, req)
	default:
		return v, serviceErr(err)
	}
}

// localWhy answers why from this device's history alone.
func (a *app) localWhy(cfg config.Config, req control.WhyRequest) (runs.WhyAnswer, error) {
	st := a.localRuns(cfg)
	recs := st.List(runs.Query{ID: req.ID, Repo: req.Repo, SHA: req.SHA, RunID: req.RunID, Limit: runs.MaxLimit})
	var rec *runs.Record
	for i := range recs {
		if req.ID != "" || recs[i].Failed() {
			rec = &recs[i]
			break
		}
	}
	if rec == nil {
		return runs.WhyAnswer{}, fail("start the service to see the pool's jobs: sudo tentmaker setup", "this device has no failed job like that")
	}
	var tests *runs.TestResults
	if t, err := st.Tests(rec.ID); err == nil {
		tests = &t
	}
	log, _ := st.Log(rec.ID)
	hist := st.List(runs.Query{Repo: rec.Repo, Since: a.env.Now().Add(-14 * 24 * time.Hour), Limit: runs.MaxLimit})
	v := runs.Why(*rec, tests, log, hist)
	v.Note = strings.TrimSpace(v.Note + " (service not running: this device's history only)")
	return v, nil
}

func (a *app) cmdWhy(args []string) (any, error) {
	f := a.flags("why", usageWhy)
	repo := f.String("repo", "", "the repository (owner/name), with --sha or --run")
	sha := f.String("sha", "", "the commit (7 or more characters)")
	run := f.Int64("run", 0, "the run id (the number in the run's URL)")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 1 {
		return nil, usage("usage: "+firstLine(usageWhy), "unexpected argument %q", pos[1])
	}
	req := control.WhyRequest{Repo: *repo, SHA: *sha, RunID: *run}
	if len(pos) == 1 {
		if r, id, ok := runs.ParseRunURL(pos[0]); ok && *repo == "" {
			req.Repo, req.RunID = r, id
		} else {
			req.ID = pos[0]
		}
	}
	v, err := a.whyData(a.ctx, req)
	if err != nil {
		return nil, err
	}
	if !a.json {
		a.printWhy(v)
	}
	return v, nil
}

func (a *app) printWhy(v runs.WhyAnswer) {
	w := a.env.Stdout
	fmt.Fprintf(w, "%s  %s  %s  (job %s on %s)\n", a.paint(conclusionStatus(v.Conclusion), v.Conclusion), v.Repo,
		jobName(runs.Record{ID: v.ID, Workflow: v.Workflow, Job: v.Job}), v.ID, v.Device)
	var where []string
	if v.Ref != "" {
		where = append(where, "ref "+v.Ref)
	}
	if v.SHA != "" {
		where = append(where, "sha "+shortSHA(v.SHA))
	}
	if v.RunURL != "" {
		where = append(where, v.RunURL)
	}
	if len(where) > 0 {
		fmt.Fprintln(w, strings.Join(where, "  "))
	}
	if v.FailedStep != "" {
		fmt.Fprintln(w, "failed step: "+v.FailedStep)
	}
	if v.Note != "" {
		fmt.Fprintln(w, "note: "+v.Note)
	}
	if len(v.Failures) > 0 {
		fmt.Fprintln(w, "\nfailing tests:")
		for _, f := range v.Failures {
			line := "  " + a.paint("fail", f.Test)
			if f.Message != "" {
				line += ": " + f.Message
			}
			fmt.Fprintln(w, line)
			if f.Detail != "" && f.Detail != f.Message {
				for _, l := range strings.Split(f.Detail, "\n") {
					fmt.Fprintln(w, "      "+l)
				}
			}
		}
		if v.MoreFailures > 0 {
			fmt.Fprintf(w, "  and %d more (tentmaker tests %s --failed)\n", v.MoreFailures, v.ID)
		}
	}
	if len(v.Log) > 0 {
		if v.ErrorLine > 0 {
			fmt.Fprintln(w, "\nlog around the first error:")
		} else {
			fmt.Fprintln(w, "\nlog (last lines; no line looked like an error):")
		}
		for _, l := range v.Log {
			fmt.Fprintln(w, "  "+l)
		}
	}
	fmt.Fprintln(w)
	if v.Flaky {
		fmt.Fprintln(w, a.paint(daemon.Warn, "looks flaky: ")+v.FlakyReason)
	} else {
		fmt.Fprintln(w, "flaky: no sign of it in the last "+flakyDefault)
	}
	if !v.KeptUntil.IsZero() {
		fmt.Fprintf(w, "sandbox kept until %s: sudo tentmaker debug %s\n", v.KeptUntil.Local().Format("15:04"), v.ID)
	}
}

// ---------------------------------------------------------------- debug

// debugExecView is debug --exec's answer; the command's exit code is the
// command line's.
type debugExecView struct {
	control.DebugExecAnswer
}

func (v debugExecView) exitCode() int {
	if v.ExitCode < 0 || v.ExitCode > 255 {
		return ExitFailed
	}
	return v.ExitCode
}

type debugReleaseView struct {
	ID       string `json:"id"`
	Released bool   `json:"released"`
}

// debugErr turns a control API refusal into a command error with a fix.
func debugErr(id string, err error) error {
	var ce *control.Error
	switch {
	case errors.As(err, &ce) && ce.Status == 404:
		return fail("set [repos.debug] keep_failed for the repo (docs/config.md); tentmaker status lists kept sandboxes", "%s", ce.Message)
	case errors.As(err, &ce) && strings.Contains(ce.Message, "run tentmaker debug there"):
		return fail("run sudo tentmaker debug "+id+" on that device", "%s", ce.Message)
	case errors.As(err, &ce):
		return fail("tentmaker doctor", "%s", ce.Message)
	}
	return serviceErr(err)
}

func (a *app) debugExec(ctx context.Context, req control.DebugExecRequest) (control.DebugExecAnswer, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return control.DebugExecAnswer{}, err
	}
	v, err := a.controlClient(cfg).DebugExec(ctx, req)
	if err != nil {
		return v, debugErr(req.ID, err)
	}
	return v, nil
}

func (a *app) cmdDebug(args []string) (any, error) {
	f := a.flags("debug", usageDebug)
	execCmd := f.String("exec", "", "run one command and exit with its exit code")
	timeout := f.Duration("timeout", 0, "time limit for --exec, up to 10m (default 60s)")
	maxBytes := f.Int64("max-bytes", 0, "output --exec keeps, its last bytes, up to 1048576 (default 65536)")
	release := f.Bool("release", false, "end the grace now; the sandbox is cleaned up")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, usage("usage: "+firstLine(usageDebug), "name one job id")
	}
	id := pos[0]
	if !runs.ValidID(id) {
		return nil, usage("tentmaker runs --failed lists job ids", "%q is not a job id", id)
	}
	switch {
	case *release && (f.set("exec") || f.set("timeout") || f.set("max-bytes")):
		return nil, usage("usage: "+firstLine(usageDebug), "--release goes alone")
	case !f.set("exec") && (f.set("timeout") || f.set("max-bytes")):
		return nil, usage("usage: "+firstLine(usageDebug), "--timeout and --max-bytes go with --exec")
	case f.set("exec") && strings.TrimSpace(*execCmd) == "":
		return nil, usage("usage: "+firstLine(usageDebug), "--exec needs a command")
	case *timeout < 0 || *timeout > control.MaxDebugTimeoutS*time.Second || *timeout%time.Second != 0:
		return nil, usage("use whole seconds up to 10m, like 90s", "--timeout %s is out of range", *timeout)
	case *maxBytes < 0 || *maxBytes > control.MaxDebugBytes:
		return nil, usage("use 1 to "+strconv.Itoa(control.MaxDebugBytes), "--max-bytes %d is out of range", *maxBytes)
	case a.json && !*release && !f.set("exec"):
		return nil, usage("use --exec CMD for one command, or leave out --json for a shell", "an interactive shell has no JSON form")
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return nil, err
	}
	c := a.controlClient(cfg)
	switch {
	case *release:
		if err := c.DebugRelease(a.ctx, id); err != nil {
			return nil, debugErr(id, err)
		}
		a.out("Released job %s's sandbox; it is cleaned up now.\n", id)
		return debugReleaseView{ID: id, Released: true}, nil
	case f.set("exec"):
		v, err := a.debugExec(a.ctx, control.DebugExecRequest{ID: id, Command: *execCmd,
			TimeoutS: int64(*timeout / time.Second), MaxBytes: *maxBytes})
		if err != nil {
			return nil, err
		}
		if !a.json {
			fmt.Fprint(a.env.Stdout, v.Output)
			if v.Truncated {
				fmt.Fprintln(a.env.Stderr, "tentmaker: output cut to its last bytes; raise --max-bytes for more")
			}
			if v.TimedOut {
				fmt.Fprintln(a.env.Stderr, "tentmaker: the command ran past its time limit and was stopped")
			}
		}
		return debugExecView{v}, nil
	}
	restore := func() {}
	if a.env.StdinTTY && a.env.RawTerminal != nil {
		if r, err := a.env.RawTerminal(); err == nil {
			restore = r
		}
	}
	err = c.DebugShell(a.ctx, id, a.in, a.env.Stdout)
	restore()
	if err != nil && a.ctx.Err() == nil {
		return nil, debugErr(id, err)
	}
	return nil, nil
}
