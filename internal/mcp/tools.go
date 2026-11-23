package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/runs"
)

// prop is one tool argument.
type prop struct {
	Name        string
	Type        string // "string", "number", "boolean", "array" (of strings)
	Description string
	Enum        []string
	Default     any
	Required    bool
	Check       func(v any) error // extra validation after the type check
}

type tool struct {
	Name        string
	Title       string
	Description string
	Props       []prop
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	OpenWorld   bool
	run         func(ctx context.Context, c Controller, a args) (any, error)
}

// args holds checked arguments with defaults applied.
type args map[string]any

func (a args) str(k string) string {
	s, _ := a[k].(string)
	return s
}

func (a args) num(k string) float64 {
	f, _ := a[k].(float64)
	return f
}

func (a args) flag(k string) bool {
	b, _ := a[k].(bool)
	return b
}

func (a args) strs(k string) []string {
	l, _ := a[k].([]any)
	out := make([]string, 0, len(l))
	for _, v := range l {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func ok(err error) (any, error) { return nil, err }

// SandboxController is a Controller that serves the sandbox tools.
type SandboxController interface {
	SandboxCreate(ctx context.Context, os, device string, cpus float64, memory, ttl, copyDir string) (any, error)
	SandboxExec(ctx context.Context, id string, argv []string, dir string, timeoutS float64, stdin string) (any, error)
	SandboxCopy(ctx context.Context, from, to string) (any, error)
	SandboxList(ctx context.Context) (any, error)
	SandboxRemove(ctx context.Context, id string) (any, error)
}

// sandboxes is c's SandboxController, or an error saying it has none.
func sandboxes(c Controller) (SandboxController, error) {
	sc, ok := c.(SandboxController)
	if !ok {
		return nil, fmt.Errorf("this tentmaker MCP server can't run sandboxes")
	}
	return sc, nil
}

func sandboxID(v any) error {
	s := v.(string)
	if !strings.HasPrefix(s, "sbx-") || len(s) > 64 {
		return fmt.Errorf("%q is not a sandbox ID (they look like sbx-1a2b3c4d5e6f); list them with sandbox_ls", s)
	}
	return nil
}

func duration(v any) error {
	s := v.(string)
	if s == "" {
		return nil
	}
	if d, err := time.ParseDuration(s); err != nil || d <= 0 {
		return fmt.Errorf("%q is not a duration like 30m or 2h", s)
	}
	return nil
}

func command(v any) error {
	l := v.([]any)
	if len(l) == 0 {
		return fmt.Errorf("give the command and its arguments, like [\"go\", \"test\", \"./...\"]")
	}
	if s, _ := l[0].(string); s == "" {
		return fmt.Errorf("the first item is the program to run")
	}
	return nil
}

// sizeRe matches the sizes config.Size accepts: "4GiB", "4G", "4GB",
// "512MiB", "0". The controller does the real parse.
var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9]+(\.[0-9]+)?)\s*([KMGT]i?B?)?\s*$`)

func memorySize(v any) error {
	s := v.(string)
	if s == "" {
		return nil
	}
	m := sizeRe.FindStringSubmatch(s)
	if m == nil || (m[3] == "" && strings.Trim(m[1], "0.") != "") {
		return fmt.Errorf("size %q: use a number with MiB or GiB, like 8GiB", s)
	}
	return nil
}

func cpuCount(v any) error {
	f := v.(float64)
	if f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("must be a positive number of CPUs (0 or omitted keeps the current value)")
	}
	return nil
}

func repoName(v any) error {
	s := v.(string)
	if strings.TrimSpace(s) != s || strings.ContainsAny(s, " \t\n") || strings.HasPrefix(s, "/") ||
		strings.HasSuffix(s, "/") || !strings.Contains(s, "/") || strings.Contains(s, "//") {
		return fmt.Errorf("%q is not a repository; use owner/name, like baney75/tentmaker", s)
	}
	return nil
}

func hostName(v any) error {
	s := v.(string)
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\n\"'`;|&$<>\\") {
		return fmt.Errorf("%q is not a host name; use the device's tailnet name, like laptop or user@laptop", s)
	}
	return nil
}

func nonEmpty(v any) error {
	if strings.TrimSpace(v.(string)) == "" {
		return fmt.Errorf("must not be empty")
	}
	return nil
}

func jobID(v any) error {
	if !runs.ValidID(v.(string)) {
		return fmt.Errorf("%q is not a job id; the runs tool lists them, like tm-laptop-1a2b3c", v)
	}
	return nil
}

func optJobID(v any) error {
	if v.(string) == "" {
		return nil
	}
	return jobID(v)
}

func optRepo(v any) error {
	if v.(string) == "" {
		return nil
	}
	return repoName(v)
}

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func commitSHA(v any) error {
	if s := v.(string); s != "" && !shaRe.MatchString(s) {
		return fmt.Errorf("%q is not a commit sha; use 7 to 40 hex characters", s)
	}
	return nil
}

func runURL(v any) error {
	if s := v.(string); s != "" {
		if _, _, ok := runs.ParseRunURL(s); !ok {
			return fmt.Errorf("%q is not a run URL like https://github.com/owner/name/actions/runs/123", s)
		}
	}
	return nil
}

func jobOrRun(v any) error {
	s := v.(string)
	if runs.ValidID(s) {
		return nil
	}
	if _, _, ok := runs.ParseRunURL(s); ok {
		return nil
	}
	return fmt.Errorf("%q is neither a job id (like tm-laptop-1a2b3c) nor a run URL", s)
}

func window(v any) error {
	if _, ok := runs.ParseSince(v.(string)); !ok {
		return fmt.Errorf("%q is not a window; use 24h, 7d or 30d", v)
	}
	return nil
}

func between(lo, hi float64) func(any) error {
	return func(v any) error {
		f := v.(float64)
		if f != math.Trunc(f) || f < lo || f > hi {
			return fmt.Errorf("must be a whole number from %g to %g", lo, hi)
		}
		return nil
	}
}

var memoryHelp = `Memory as a number with a unit, like "4GiB" or "512MiB".`

var tools = []tool{
	{
		Name:  "status",
		Title: "Device status",
		Description: "Show this device's Tentmaker state: the active budget profile (everyday, boost or off) and why, " +
			"free slots, running jobs, paused or draining, power and guard readings, and the installed version. " +
			"Read-only. Call this first.",
		ReadOnly: true, Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) { return c.Status(ctx) },
	},
	{
		Name:  "pool",
		Title: "Pool status",
		Description: "List every device in this device's pool (same tailnet, same pool name) with its budget, free " +
			"slots, running jobs, version and whether it is a broker. Read-only. Use it to see total CI capacity " +
			"or find a device that isn't serving jobs.",
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) { return c.Pool(ctx) },
	},
	{
		Name:  "doctor",
		Title: "Diagnose problems",
		Description: "Run every health check on this device (service running, executor installed, forge credentials, " +
			"tailnet reachable, disk and memory guards, config valid) and return each problem with the exact fix. " +
			"Read-only. Run it when jobs stay queued or a device is missing from the pool.",
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) { return c.Doctor(ctx) },
	},
	{
		Name:  "set_budget",
		Title: "Set the CI budget",
		Description: "Change how much of this device CI may use. Everyday applies all the time; boost applies when " +
			"the device is plugged in (and idle, if configured) and must be at least everyday. Pass only the values " +
			"to change; omitted ones keep their current setting. Running jobs are not stopped: the device drains " +
			"down to a smaller budget.",
		Props: []prop{
			{Name: "everyday_memory", Type: "string", Description: "Everyday memory budget. " + memoryHelp, Check: memorySize},
			{Name: "boost_memory", Type: "string", Description: "Boost memory budget, at least the everyday one. " + memoryHelp, Check: memorySize},
			{Name: "everyday_cpus", Type: "number", Description: "Everyday CPU budget in cores; fractions allowed, like 1.5.", Check: cpuCount},
			{Name: "boost_cpus", Type: "number", Description: "Boost CPU budget in cores; fractions allowed.", Check: cpuCount},
		},
		Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			if a.str("everyday_memory") == "" && a.str("boost_memory") == "" && a.num("everyday_cpus") == 0 && a.num("boost_cpus") == 0 {
				return nil, fmt.Errorf("nothing to change: pass at least one of everyday_memory, boost_memory, everyday_cpus, boost_cpus")
			}
			return ok(c.SetBudget(ctx, a.str("everyday_memory"), a.str("boost_memory"), a.num("everyday_cpus"), a.num("boost_cpus")))
		},
	},
	{
		Name:  "add_repo",
		Title: "Serve a repository",
		Description: "Let this device run CI jobs for a repository. Mode runner registers the forge's own runner " +
			"(GitHub, Forgejo); mode checks runs workflow jobs itself and posts commit statuses (any forge, required " +
			"for GitLab). Public repos are refused unless allow_public is true, which first makes the forge require " +
			"approval before outside contributors' workflows run. Workflows must ask for runs-on: [self-hosted, tentmaker, <os>].",
		Props: []prop{
			{Name: "repo", Type: "string", Required: true, Description: "Repository as owner/name.", Check: repoName},
			{Name: "forge", Type: "string", Enum: []string{"github", "forgejo", "gitlab"}, Default: "github", Description: "Forge that hosts the repository."},
			{Name: "mode", Type: "string", Enum: []string{"runner", "checks"}, Description: "runner (default for github) or checks (the default, and the only mode, for gitlab and forgejo)."},
			{Name: "allow_public", Type: "boolean", Default: false, Description: "Allow a public repository. Leave false unless the owner asked for it."},
		},
		Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			// An empty mode lets the controller pick the forge's default, the
			// same rule the CLI uses; filling in "runner" here broke every
			// Forgejo add (review r1 partB first-F3).
			return ok(c.AddRepo(ctx, a.str("repo"), a.str("forge"), a.str("mode"), a.flag("allow_public")))
		},
	},
	{
		Name:  "remove_repo",
		Title: "Stop serving a repository",
		Description: "Stop running CI for a repository on this device and remove its runners from the forge. Jobs " +
			"already running finish first. Queued jobs for it wait for another runner.",
		Props: []prop{
			{Name: "repo", Type: "string", Required: true, Description: "Repository as owner/name.", Check: repoName},
		},
		Destructive: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return ok(c.RemoveRepo(ctx, a.str("repo")))
		},
	},
	{
		Name:  "pause",
		Title: "Pause CI on this device",
		Description: "Stop taking new jobs on this device until resume is called. Running jobs finish. The pause " +
			"survives restarts. Use it before heavy work on the machine.",
		Props: []prop{
			{Name: "reason", Type: "string", Description: "Why, shown in status, like \"video call\"."},
		},
		Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return ok(c.Pause(ctx, a.str("reason")))
		},
	},
	{
		Name:        "resume",
		Title:       "Resume CI on this device",
		Description: "Undo pause: take new jobs again, within the budget.",
		Idempotent:  true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return ok(c.Resume(ctx))
		},
	},
	{
		Name:  "migrate_workflows",
		Title: "Point workflows at Tentmaker",
		Description: "Rewrite a local checkout's GitHub workflow runs-on lines to use Tentmaker runners for the given " +
			"platform, and report every change. With write false (the default) it only reports; with write true it " +
			"edits files under .github/workflows in place. Review the report before writing, and commit the result yourself.",
		Props: []prop{
			{Name: "repo_path", Type: "string", Required: true, Description: "Absolute path of a local git checkout.", Check: nonEmpty},
			{Name: "platform", Type: "string", Default: "linux", Description: "Target runner OS: linux, macos or windows.", Check: nonEmpty},
			{Name: "write", Type: "boolean", Default: false, Description: "Edit the files (true) or only report (false)."},
		},
		Destructive: true, Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.MigrateWorkflows(ctx, a.str("repo_path"), a.str("platform"), a.flag("write"))
		},
	},
	{
		Name:  "install_on_device",
		Title: "Install on another device",
		Description: "Install Tentmaker on another device on the same tailnet over tailscale ssh, run setup " +
			"non-interactively and join it to this pool. Safe to rerun. The host must allow tailscale ssh for this user. " +
			"Memory values default to the installer's (a quarter and half of RAM).",
		Props: []prop{
			{Name: "host", Type: "string", Required: true, Description: "Tailnet host name, like laptop or user@laptop.", Check: hostName},
			{Name: "channel", Type: "string", Enum: []string{"stable", "dev"}, Default: "stable", Description: "Release channel."},
			{Name: "memory", Type: "string", Description: "Everyday memory budget. " + memoryHelp, Check: memorySize},
			{Name: "boost_memory", Type: "string", Description: "Boost memory budget. " + memoryHelp, Check: memorySize},
		},
		Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.InstallOnDevice(ctx, a.str("host"), a.str("channel"), a.str("memory"), a.str("boost_memory"))
		},
	},
	{
		Name:  "update",
		Title: "Update Tentmaker",
		Description: "Update this device to the newest release on a channel: download, verify the sha256, health " +
			"check, swap the binary, restart the service, and roll back if it isn't healthy within 60 s. It first " +
			"pauses new jobs and waits up to 30 min for running ones to finish; if they don't, nothing changes and " +
			"new jobs resume. Omit channel to use the configured one; passing one also switches the device to it.",
		Props: []prop{
			{Name: "channel", Type: "string", Enum: []string{"stable", "dev"}, Description: "stable or dev; omit for the configured channel."},
		},
		Destructive: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Update(ctx, a.str("channel"))
		},
	},
	{
		Name:  "runs",
		Title: "Recent CI jobs",
		Description: "List recent Tentmaker CI jobs across the pool, newest first, running jobs on top: conclusion " +
			"(success, failure, cancelled, error, unknown or running), repo, workflow and job, device, queue wait, " +
			"duration, peak memory and CPU seconds (Linux), the failed step when known, the run URL and the job id " +
			"that logs and rerun take. Read-only. Filter by repo, commit or failed only.",
		Props: []prop{
			{Name: "repo", Type: "string", Description: "Only this repository, as owner/name.", Check: optRepo},
			{Name: "sha", Type: "string", Description: "Only this commit: the full sha or its first 7 or more characters.", Check: commitSHA},
			{Name: "failed", Type: "boolean", Default: false, Description: "Only failed or errored jobs."},
			{Name: "limit", Type: "number", Default: float64(20), Description: "How many jobs, 1 to 500.", Check: between(1, 500)},
		},
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Runs(ctx, a.str("repo"), a.str("sha"), a.flag("failed"), int(a.num("limit")))
		},
	},
	{
		Name:  "logs",
		Title: "A CI job's log",
		Description: "Return the last lines of one CI job's log, from whichever pool device ran it: the runner's own " +
			"log for runner-mode jobs, the step output for checks-mode jobs. Tokens are masked. A running job returns " +
			"what it has written so far, with running true; call again for more. Read-only.",
		Props: []prop{
			{Name: "job_id", Type: "string", Required: true, Description: "The job id from runs or wait_for_run.", Check: jobID},
			{Name: "lines", Type: "number", Default: float64(200), Description: "How many lines from the end, 1 to 5000.", Check: between(1, 5000)},
		},
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Logs(ctx, a.str("job_id"), int(a.num("lines")))
		},
	},
	{
		Name:  "rerun",
		Title: "Rerun failed jobs",
		Description: "Ask the forge to rerun the failed jobs of a GitHub Actions run, by a job id from runs or by the " +
			"run's URL. The broker that holds the forge token does it; the token never leaves that device. Checks-mode " +
			"jobs have no forge run and are refused: push again instead. Needs the control token (root).",
		Props: []prop{
			{Name: "target", Type: "string", Required: true, Description: "A job id (tm-...) or a run URL (https://github.com/owner/name/actions/runs/N).", Check: jobOrRun},
		},
		OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Rerun(ctx, a.str("target"))
		},
	},
	{
		Name:  "stats",
		Title: "CI statistics",
		Description: "Sum the pool's ended jobs over a window, per repo: jobs, failure rate, p50 and p95 queue wait " +
			"and duration, minutes served, and an estimated dollar saving against GitHub-hosted runners at list price " +
			"(an estimate, labeled so). Read-only.",
		Props: []prop{
			{Name: "since", Type: "string", Default: "7d", Description: "Window back from now, like 24h, 7d or 30d.", Check: window},
			{Name: "repo", Type: "string", Description: "Only this repository, as owner/name.", Check: optRepo},
		},
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Stats(ctx, a.str("since"), a.str("repo"))
		},
	},
	{
		Name:  "wait_for_run",
		Title: "Wait for CI to finish",
		Description: "Block until every Tentmaker job for a commit (repo and sha) or for one run (run_url) has ended " +
			"and nothing is queued for it, or until timeout_s passes. Returns done or timed_out, the overall " +
			"conclusion, every job's conclusion, and for each failed job its failed step and last 80 log lines. Use " +
			"it after a push instead of polling. Jobs on GitHub-hosted runners are not seen. Read-only.",
		Props: []prop{
			{Name: "repo", Type: "string", Description: "Repository as owner/name; needed with sha.", Check: optRepo},
			{Name: "sha", Type: "string", Description: "The pushed commit: full sha or 7 or more characters.", Check: commitSHA},
			{Name: "run_url", Type: "string", Description: "Instead of repo and sha: a run URL like https://github.com/owner/name/actions/runs/123.", Check: runURL},
			{Name: "timeout_s", Type: "number", Default: float64(600), Description: "Seconds to wait at most, 10 to 3600.", Check: between(10, 3600)},
		},
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			byCommit := a.str("repo") != "" && a.str("sha") != ""
			byRun := a.str("run_url") != ""
			if byCommit == byRun || (byRun && (a.str("repo") != "" || a.str("sha") != "")) {
				return nil, fmt.Errorf("pass repo and sha, or run_url alone")
			}
			return c.WaitForRun(ctx, a.str("repo"), a.str("sha"), a.str("run_url"), time.Duration(a.num("timeout_s"))*time.Second)
		},
	},
	{
		Name:  "tests",
		Title: "A CI job's test results",
		Description: "Return one job's test cases from its JUnit reports and go test -json output: counts, and each " +
			"failed test with its message and output. Failed only by default. Read-only.",
		Props: []prop{
			{Name: "id", Type: "string", Required: true, Description: "The job id from runs or wait_for_run.", Check: jobID},
			{Name: "failed", Type: "boolean", Default: true, Description: "Only failed tests (true), or every test, up to 5000 (false)."},
		},
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Tests(ctx, a.str("id"), a.flag("failed"))
		},
	},
	{
		Name:  "flaky",
		Title: "Flaky tests",
		Description: "List tests that look flaky across the pool: they failed and then passed on the same commit, or " +
			"flipped between failing and passing in a job's recent runs on one branch. Read-only.",
		Props: []prop{
			{Name: "repo", Type: "string", Description: "Only this repository, as owner/name.", Check: optRepo},
			{Name: "since", Type: "string", Default: "14d", Description: "Window back from now, like 24h, 7d or 30d.", Check: window},
		},
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Flaky(ctx, a.str("repo"), a.str("since"))
		},
	},
	{
		Name:  "why_failed",
		Title: "Why a CI job failed",
		Description: "Explain a failed job in a small answer: the failed step, up to 5 failing tests with their messages, " +
			"the log lines around the first error, and whether it looks flaky. Pass id, or repo with sha or run_id for " +
			"the newest failed job there. kept_until is set when its sandbox is kept for debug_exec. Read-only.",
		Props: []prop{
			{Name: "id", Type: "string", Description: "A job id from runs or wait_for_run.", Check: optJobID},
			{Name: "repo", Type: "string", Description: "Repository as owner/name, with sha or run_id.", Check: optRepo},
			{Name: "sha", Type: "string", Description: "The commit: full sha or 7 or more characters.", Check: commitSHA},
			{Name: "run_id", Type: "number", Description: "The run id, the number in the run's URL.", Check: between(0, 1<<53)},
		},
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			byID := a.str("id") != ""
			byRepo := a.str("repo") != "" && (a.str("sha") != "") != (a.num("run_id") != 0)
			if byID == byRepo || (byID && (a.str("repo") != "" || a.str("sha") != "" || a.num("run_id") != 0)) {
				return nil, fmt.Errorf("pass id alone, or repo with sha or run_id")
			}
			return c.WhyFailed(ctx, a.str("id"), a.str("repo"), a.str("sha"), int64(a.num("run_id")))
		},
	},
	{
		Name:  "debug_exec",
		Title: "Run a command in a failed job's sandbox",
		Description: "Run one shell command in a failed job's sandbox, kept on this device when the repo sets " +
			"[repos.debug] keep_failed, as the job's user with its limits and network fence. Returns exit_code, output " +
			"(redacted, its last max_bytes), truncated and timed_out. Use it to rerun a failing test or read a file " +
			"the job left. Needs the MCP server to run as root.",
		Props: []prop{
			{Name: "id", Type: "string", Required: true, Description: "The failed job's id; why_failed says whether its sandbox is kept.", Check: jobID},
			{Name: "command", Type: "string", Required: true, Description: "A shell command, run with /bin/sh -c in the job's workspace.", Check: nonEmpty},
			{Name: "timeout_s", Type: "number", Default: float64(60), Description: "Seconds the command may run, 1 to 600.", Check: between(1, 600)},
			{Name: "max_bytes", Type: "number", Default: float64(64 << 10), Description: "Output bytes to return, its last ones, 1 to 1048576.", Check: between(1, 1<<20)},
		},
		Destructive: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.DebugExec(ctx, a.str("id"), a.str("command"), int64(a.num("timeout_s")), int64(a.num("max_bytes")))
		},
	},
	{
		Name:  "sandbox_create",
		Title: "Create a sandbox",
		Description: "Create a throwaway sandbox on this device and return its id. It has the isolation a CI job gets " +
			"(on Linux its own user, cgroup limits, network namespace and the job fence) and counts against the " +
			"device's budget like a job; it fails with the reason when there is no room. It is deleted when its ttl " +
			"runs out, on sandbox_rm, or when the service restarts. copy_dir copies a local folder's working tree " +
			"into the sandbox's work folder first (uncommitted changes and untracked, non-ignored files; no .git).",
		Props: []prop{
			{Name: "os", Type: "string", Enum: []string{"linux", "macos", "windows"}, Default: "linux", Description: "The sandbox's OS."},
			{Name: "device", Type: "string", Description: "The device to run it on; omit for this one.",
				Check: func(v any) error {
					if v.(string) == "" {
						return nil
					}
					return hostName(v)
				}},
			{Name: "cpus", Type: "number", Description: "CPUs; omit for one job's.", Check: cpuCount},
			{Name: "memory", Type: "string", Description: "Memory; omit for one job's. " + memoryHelp, Check: memorySize},
			{Name: "ttl", Type: "string", Default: "30m", Description: "Time to live, like 30m or 2h.", Check: duration},
			{Name: "copy_dir", Type: "string", Description: "An absolute local folder to copy in, like the repository you are working in."},
		},
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			sc, err := sandboxes(c)
			if err != nil {
				return nil, err
			}
			return sc.SandboxCreate(ctx, a.str("os"), a.str("device"), a.num("cpus"), a.str("memory"), a.str("ttl"), a.str("copy_dir"))
		},
	},
	{
		Name:  "sandbox_exec",
		Title: "Run a command in a sandbox",
		Description: "Run one command in a sandbox and return its exit_code, stdout and stderr (each capped at 1 MiB; " +
			"*_truncated says when). The command is argv, not a shell line: use [\"sh\", \"-c\", \"...\"] for pipes. " +
			"It runs in the sandbox's work folder unless dir says otherwise, and stops at timeout_s or when the " +
			"sandbox expires. A non-zero exit_code is a normal result, not a tool error.",
		Props: []prop{
			{Name: "id", Type: "string", Required: true, Description: "The sandbox id from sandbox_create.", Check: sandboxID},
			{Name: "command", Type: "array", Required: true, Description: "The program and its arguments.", Check: command},
			{Name: "dir", Type: "string", Description: "The folder to run in, relative to the work folder or absolute."},
			{Name: "timeout_s", Type: "number", Description: "Stop the command after this many seconds; omit to run until the sandbox expires.",
				Check: func(v any) error {
					if f := v.(float64); f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
						return fmt.Errorf("must be 0 or more")
					}
					return nil
				}},
			{Name: "stdin", Type: "string", Description: "Text to pass on stdin."},
		},
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			sc, err := sandboxes(c)
			if err != nil {
				return nil, err
			}
			return sc.SandboxExec(ctx, a.str("id"), a.strs("command"), a.str("dir"), a.num("timeout_s"), a.str("stdin"))
		},
	},
	{
		Name:  "sandbox_cp",
		Title: "Copy files to or from a sandbox",
		Description: "Copy a file or folder between this machine and a sandbox. Write the sandbox side as ID:PATH. " +
			"Into a sandbox: from is a local path and to is ID:FOLDER; it lands in FOLDER under its own name. Out of " +
			"a sandbox: from is ID:PATH and to is a local folder. Out of a sandbox, only regular files and folders " +
			"are written, never links.",
		Props: []prop{
			{Name: "from", Type: "string", Required: true, Description: "A local path, or ID:PATH in a sandbox.", Check: nonEmpty},
			{Name: "to", Type: "string", Required: true, Description: "ID:FOLDER in a sandbox, or a local folder.", Check: nonEmpty},
		},
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			sc, err := sandboxes(c)
			if err != nil {
				return nil, err
			}
			return sc.SandboxCopy(ctx, a.str("from"), a.str("to"))
		},
	},
	{
		Name:        "sandbox_ls",
		Title:       "List sandboxes",
		Description: "List your sandboxes on this device (every sandbox when the server runs as root) with platform, limits, expiry and state. Read-only.",
		ReadOnly:    true, Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			sc, err := sandboxes(c)
			if err != nil {
				return nil, err
			}
			return sc.SandboxList(ctx)
		},
	},
	{
		Name:        "sandbox_rm",
		Title:       "Remove a sandbox",
		Description: "Stop a sandbox and delete everything in it. Copy out what you need first with sandbox_cp.",
		Props: []prop{
			{Name: "id", Type: "string", Required: true, Description: "The sandbox id.", Check: sandboxID},
		},
		Destructive: true, Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			sc, err := sandboxes(c)
			if err != nil {
				return nil, err
			}
			return sc.SandboxRemove(ctx, a.str("id"))
		},
	},
}

func toolByName(name string) (tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

// check validates raw arguments against t's schema and applies defaults.
func (t tool) check(raw map[string]any) (args, error) {
	known := map[string]prop{}
	for _, p := range t.Props {
		known[p.Name] = p
	}
	var unknown []string
	for k := range raw {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown argument %s; this tool takes %s", strings.Join(unknown, ", "), t.argNames())
	}
	a := args{}
	for _, p := range t.Props {
		v, present := raw[p.Name]
		if !present || v == nil {
			if p.Required {
				return nil, fmt.Errorf("%s is required: %s", p.Name, p.Description)
			}
			switch {
			case p.Default != nil:
				v = p.Default
			case p.Type == "string":
				v = ""
			case p.Type == "number":
				v = float64(0)
			case p.Type == "boolean":
				v = false
			case p.Type == "array":
				v = []any{}
			}
		}
		if !typeOK(p.Type, v) {
			return nil, fmt.Errorf("%s must be a %s", p.Name, p.Type)
		}
		if len(p.Enum) > 0 && present {
			if !contains(p.Enum, v.(string)) {
				return nil, fmt.Errorf("%s must be one of %s", p.Name, strings.Join(p.Enum, ", "))
			}
		}
		if p.Check != nil {
			if err := p.Check(v); err != nil {
				return nil, fmt.Errorf("%s: %v", p.Name, err)
			}
		}
		a[p.Name] = v
	}
	return a, nil
}

func (t tool) argNames() string {
	if len(t.Props) == 0 {
		return "no arguments"
	}
	var n []string
	for _, p := range t.Props {
		n = append(n, p.Name)
	}
	return strings.Join(n, ", ")
}

func typeOK(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		l, ok := v.([]any)
		if !ok {
			return false
		}
		for _, x := range l {
			if _, ok := x.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// toolList is the tools/list payload.
func toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		props := map[string]any{}
		var required []string
		for _, p := range t.Props {
			s := map[string]any{"type": p.Type, "description": p.Description}
			if p.Type == "array" {
				s["items"] = map[string]any{"type": "string"}
			}
			if len(p.Enum) > 0 {
				s["enum"] = p.Enum
			}
			if p.Default != nil {
				s["default"] = p.Default
			}
			props[p.Name] = s
			if p.Required {
				required = append(required, p.Name)
			}
		}
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": schema,
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
				"idempotentHint":  t.Idempotent,
				"openWorldHint":   t.OpenWorld,
			},
		})
	}
	return out
}

// normalizeID makes 1, 1.0 and "1" compare by their JSON value, so a
// cancellation finds its request regardless of spacing.
func normalizeID(id json.RawMessage) string {
	var v any
	if json.Unmarshal(id, &v) != nil {
		return string(id)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
