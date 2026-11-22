package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baney75/tentmaker/internal/alerts"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

const (
	usageRuns   = "tentmaker runs [--repo R] [--sha SHA] [--failed] [-n 20] [--json]"
	usageRerun  = "tentmaker rerun JOB-ID|RUN-URL [--json]\n\nReruns the run's failed jobs through the forge with the broker's token."
	usageStats  = "tentmaker stats [--since 7d] [--repo R] [--json]"
	usageAlerts = "tentmaker alerts test [--json]\n\nSends a test message to every [[alerts]] webhook and says how each went."
	// followEvery is how often logs --follow asks for more.
	followEvery = time.Second
)

// runsView is the answer of runs, and of the MCP runs tool.
type runsView struct {
	Service string        `json:"service"` // "running", or "not running" (this device's history only)
	Records []runs.Record `json:"records"`
	Queued  []string      `json:"queued,omitempty"`
	Errors  []string      `json:"errors,omitempty"`
}

// runsData asks the service for the pool's history; without the service it
// reads this device's history file.
func (a *app) runsData(ctx context.Context, q runs.Query) (runsView, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return runsView{}, err
	}
	var ans runs.Answer
	err = a.controlClient(cfg).Get(ctx, "/v1/runs?"+q.Values().Encode(), &ans)
	switch {
	case err == nil:
		return runsView{Service: "running", Records: nonNil(ans.Records), Queued: ans.Queued, Errors: ans.Errors}, nil
	case errors.Is(err, control.ErrNotRunning):
		st := a.localRuns(cfg)
		v := runsView{Service: "not running", Records: nonNil(st.List(q))}
		if serr := st.Err(); serr != nil {
			v.Errors = []string{"run history: " + serr.Error()}
		}
		return v, nil
	default:
		return runsView{}, serviceErr(err)
	}
}

func (a *app) localRuns(cfg config.Config) *runs.Store {
	return runs.Open(filepath.Join(a.resolved(cfg).StateDir, "runs"), 0)
}

func (a *app) cmdRuns(args []string) (any, error) {
	f := a.flags("runs", usageRuns)
	repo := f.String("repo", "", "only this repository (owner/name)")
	sha := f.String("sha", "", "only this commit (7 or more characters)")
	failed := f.Bool("failed", false, "only failed jobs")
	n := f.Int("n", 20, "how many")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+usageRuns, "unexpected argument %q", pos[0])
	}
	if *n <= 0 || *n > runs.MaxLimit {
		return nil, usage(fmt.Sprintf("use -n from 1 to %d", runs.MaxLimit), "-n %d is out of range", *n)
	}
	v, err := a.runsData(a.ctx, runs.Query{Repo: *repo, SHA: *sha, Failed: *failed, Limit: *n})
	if err != nil {
		return nil, err
	}
	if !a.json {
		a.printRuns(v)
	}
	return v, nil
}

func (a *app) printRuns(v runsView) {
	if len(v.Records) == 0 {
		fmt.Fprintln(a.env.Stdout, "No jobs yet.")
	} else {
		now := a.env.Now()
		tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CONCLUSION\tREPO\tJOB\tDEVICE\tQUEUED\tDURATION\tPEAK MEM\tENDED\tID")
		for _, r := range v.Records {
			ended := "-"
			if !r.EndedAt.IsZero() {
				ended = ago(now.Sub(r.EndedAt))
			}
			concl := r.Conclusion
			if r.FailedStep != "" {
				concl += " (" + trunc(r.FailedStep, 24) + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.paint(conclusionStatus(r.Conclusion), concl), r.Repo,
				trunc(jobName(r), 32), r.Device, shortDur(r.QueueWait()), shortDur(r.Duration(now)), memSize(r.PeakMemoryBytes), ended, r.ID)
		}
		tw.Flush()
	}
	if v.Service != "running" {
		fmt.Fprintln(a.env.Stdout, "The service is not running: this is this device's history only.")
	}
	for _, q := range v.Queued {
		fmt.Fprintln(a.env.Stdout, "queued: "+q)
	}
	for _, e := range v.Errors {
		fmt.Fprintln(a.env.Stdout, "not counted: "+e)
	}
}

func conclusionStatus(c string) string {
	switch c {
	case runs.Success:
		return "ok"
	case runs.Failure, runs.Error:
		return "fail"
	case runs.Running:
		return "bold"
	}
	return "warn"
}

func jobName(r runs.Record) string {
	if r.Workflow != "" && r.Job != "" {
		return r.Workflow + " / " + r.Job
	}
	return firstNonEmpty(r.Job, r.Workflow, r.ID)
}

// shortDur prints a duration like 42s, 3m05s or 1h02m; 0 is "-".
func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func ago(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return shortDur(d.Truncate(time.Minute)) + " ago"
}

// memSize prints bytes like 812MiB or 3.1GiB; 0 is "-".
func memSize(b int64) string {
	switch {
	case b <= 0:
		return "-"
	case b < 1<<30:
		return fmt.Sprintf("%dMiB", (b+(1<<19))>>20)
	}
	return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// runLog asks the service for a job's log tail; without the service it
// reads this device's kept tail.
func (a *app) runLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error) {
	if !runs.ValidID(id) {
		return runs.LogAnswer{}, usage("tentmaker runs lists job ids, like tm-laptop-1a2b3c", "%q is not a job id", id)
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return runs.LogAnswer{}, err
	}
	var l runs.LogAnswer
	err = a.controlClient(cfg).Get(ctx, fmt.Sprintf("/v1/runs/log?id=%s&max=%d", id, max), &l)
	var ce *control.Error
	switch {
	case err == nil:
		return l, nil
	case errors.As(err, &ce) && ce.Status == 404:
		return l, fail("tentmaker runs lists the jobs the pool kept", "no device has job %s", id)
	case errors.Is(err, control.ErrNotRunning):
		data, lerr := a.localRuns(cfg).Log(id)
		if errors.Is(lerr, core.ErrNotFound) {
			return l, fail("start the service to see the pool's jobs: sudo tentmaker setup", "this device kept no log for %s", id)
		}
		if lerr != nil {
			return l, fail("run it with sudo; job logs are readable by root only", "read the log: %v", lerr)
		}
		return runs.LogAnswer{ID: id, Device: a.resolved(cfg).Device, Size: int64(len(data)), Data: runs.Tail(data, int(max))}, nil
	default:
		return l, serviceErr(err)
	}
}

type jobLogView struct {
	ID      string   `json:"id"`
	Device  string   `json:"device"`
	Running bool     `json:"running"`
	Lines   []string `json:"lines"`
}

// cmdJobLog prints a job's log tail, and with follow streams it until
// the job ends.
func (a *app) cmdJobLog(id string, n int, nSet, follow bool) (any, error) {
	l, err := a.runLog(a.ctx, id, runs.LogTailBytes)
	if err != nil {
		return nil, err
	}
	text := string(l.Data)
	if nSet {
		text = strings.Join(runs.TailLines(text, n), "\n")
		if text != "" {
			text += "\n"
		}
	}
	if a.json {
		return jobLogView{ID: l.ID, Device: l.Device, Running: l.Running, Lines: splitLines(text)}, nil
	}
	fmt.Fprint(a.env.Stdout, text)
	if !follow || !l.Running {
		if follow {
			fmt.Fprintf(a.env.Stdout, "(job %s has ended)\n", id)
		}
		return nil, nil
	}
	prev := l.Data
	for {
		if err := a.sleepCtx(followEvery); err != nil {
			return nil, nil // Ctrl-C
		}
		next, err := a.runLog(a.ctx, id, runs.LogTailBytes)
		if err != nil {
			if a.ctx.Err() != nil {
				return nil, nil
			}
			return nil, err
		}
		a.env.Stdout.Write(newBytes(prev, next.Data))
		prev = next.Data
		if !next.Running {
			fmt.Fprintf(a.env.Stdout, "(job %s has ended)\n", id)
			return nil, nil
		}
	}
}

// newBytes is what next holds past prev. Both are tails of a log that only
// grows, so the end of prev is found in next; when it isn't (the log moved
// past a whole tail between polls), all of next is new.
func newBytes(prev, next []byte) []byte {
	if len(prev) == 0 {
		return next
	}
	anchor := prev
	if len(anchor) > 4096 {
		anchor = anchor[len(anchor)-4096:]
	}
	if i := bytes.LastIndex(next, anchor); i >= 0 {
		return next[i+len(anchor):]
	}
	if bytes.Equal(prev, next) {
		return nil
	}
	return append([]byte("[... more output than one poll could show ...]\n"), next...)
}

func (a *app) sleepCtx(d time.Duration) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	a.env.Sleep(d)
	return a.ctx.Err()
}

// rerun reruns the failed jobs of the run a job id or run URL names.
func (a *app) rerun(ctx context.Context, target string) (control.RerunAnswer, error) {
	repo, runID, ok := runs.ParseRunURL(target)
	if !ok && !runs.ValidID(target) {
		return control.RerunAnswer{}, usage("usage: tentmaker rerun JOB-ID|RUN-URL", "%q is neither a job id nor a run URL", target)
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return control.RerunAnswer{}, err
	}
	if !ok {
		v, err := a.runsData(ctx, runs.Query{ID: target, Limit: 1})
		if err != nil {
			return control.RerunAnswer{}, err
		}
		if len(v.Records) == 0 {
			return control.RerunAnswer{}, fail("tentmaker runs lists job ids", "no device has job %s", target)
		}
		r := v.Records[0]
		if r.Kind == core.KindCheck || r.RunID == 0 {
			return control.RerunAnswer{}, fail("push again, or rerun from the forge", "job %s is a checks-mode job; it has no forge run to rerun", target)
		}
		repo, runID = r.Repo, r.RunID
	}
	var ans control.RerunAnswer
	if err := a.controlClient(cfg).PostFor(ctx, "/v1/rerun", control.RerunRequest{Repo: repo, RunID: runID}, &ans); err != nil {
		return ans, serviceErr(err)
	}
	return ans, nil
}

func (a *app) cmdRerun(args []string) (any, error) {
	f := a.flags("rerun", usageRerun)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, usage("usage: "+firstLine(usageRerun), "name one job id or run URL")
	}
	ans, err := a.rerun(a.ctx, pos[0])
	if err != nil {
		return nil, err
	}
	if !a.json {
		fmt.Fprintf(a.env.Stdout, "Asked the forge to rerun the failed jobs of run %d in %s (through %s).\n", ans.RunID, ans.Repo, ans.Broker)
	}
	return ans, nil
}

// statsData sums the pool's history since a window ago.
func (a *app) statsData(ctx context.Context, since, repo string) (runs.Report, error) {
	d, ok := runs.ParseSince(since)
	if !ok {
		return runs.Report{}, usage("use a window like 24h, 7d or 30d", "--since %q is not a window", since)
	}
	now := a.env.Now()
	v, err := a.runsData(ctx, runs.Query{Repo: repo, Since: now.Add(-d), Limit: runs.MaxLimit})
	if err != nil {
		return runs.Report{}, err
	}
	return runs.Stats(v.Records, now.Add(-d), now), nil
}

func (a *app) cmdStats(args []string) (any, error) {
	f := a.flags("stats", usageStats)
	since := f.String("since", "7d", "window, like 24h, 7d or 30d")
	repo := f.String("repo", "", "only this repository")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+usageStats, "unexpected argument %q", pos[0])
	}
	rep, err := a.statsData(a.ctx, *since, *repo)
	if err != nil {
		return nil, err
	}
	if !a.json {
		a.printStats(rep, *since)
	}
	return rep, nil
}

func (a *app) printStats(rep runs.Report, since string) {
	if rep.Total.Jobs == 0 {
		fmt.Fprintf(a.env.Stdout, "No jobs ended in the last %s.\n", since)
		return
	}
	tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tJOBS\tFAILED\tQUEUE P50\tQUEUE P95\tRUN P50\tRUN P95\tMINUTES\tEST. SAVED")
	row := func(s runs.RepoStats) {
		fmt.Fprintf(tw, "%s\t%d\t%.0f%%\t%s\t%s\t%s\t%s\t%.0f\t$%.2f\n", s.Repo, s.Jobs, s.FailureRate*100,
			secs(s.QueueP50S), secs(s.QueueP95S), secs(s.DurationP50), secs(s.DurationP95), s.Minutes, s.EstimatedSavingUSD)
	}
	for _, s := range rep.Repos {
		row(s)
	}
	if len(rep.Repos) > 1 {
		row(rep.Total)
	}
	tw.Flush()
	fmt.Fprintf(a.env.Stdout, "Last %s. %s\n", since, rep.Estimate)
}

func secs(s float64) string { return shortDur(time.Duration(s * float64(time.Second))) }

type alertsView struct {
	Results []alerts.Result `json:"results"`
}

func (a *app) alertsTest(ctx context.Context) (alertsView, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return alertsView{}, err
	}
	var res []alerts.Result
	if err := a.controlClient(cfg).PostFor(ctx, "/v1/alerts/test", nil, &res); err != nil {
		var ce *control.Error
		if errors.As(err, &ce) && strings.Contains(ce.Message, "no alerts") {
			return alertsView{}, fail("add an [[alerts]] table to the config (docs/config.md), then restart the service", "no alerts are configured")
		}
		return alertsView{}, serviceErr(err)
	}
	return alertsView{Results: nonNil(res)}, nil
}

func (a *app) cmdAlerts(args []string) (any, error) {
	f := a.flags("alerts", usageAlerts)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 || pos[0] != "test" {
		return nil, usage("usage: "+firstLine(usageAlerts), "the only alerts command is test")
	}
	res, err := a.alertsTest(a.ctx)
	if err != nil {
		return nil, err
	}
	failed := 0
	for _, r := range res.Results {
		state := "sent"
		if !r.OK {
			state, failed = "failed: "+r.Error, failed+1
		}
		if !a.json {
			fmt.Fprintf(a.env.Stdout, "alert %d (on %s, %s format) to %s: %s\n", r.Alert, r.On, r.Kind, r.Host, state)
		}
	}
	if failed > 0 && !a.json {
		return nil, fail("check the url or url_secret of each failed alert", "%d alert(s) did not send", failed)
	}
	return res, nil
}
