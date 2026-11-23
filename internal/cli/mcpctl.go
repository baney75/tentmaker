package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/forge/github"
	"github.com/baney75/tentmaker/internal/mcp"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/runs"
	"github.com/baney75/tentmaker/internal/sandbox"
)

// controller is the MCP Controller. Every tool calls the same functions
// as the matching command; output goes only through MCP.
type controller struct{ a *app }

var _ mcp.Controller = controller{}

// mcpErr flattens a command error into one line with its fix.
func mcpErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *cliError
	if errors.As(err, &ce) && ce.fix != "" {
		return fmt.Errorf("%s (fix: %s)", ce.msg, ce.fix)
	}
	return err
}

// mutating refuses a change when the MCP server is not root.
func (c controller) mutating() error {
	if c.a.env.IsRoot() {
		return nil
	}
	how := "sudo"
	if c.a.env.GOOS == "windows" {
		how = "an Administrator shell"
	}
	return fmt.Errorf("this tool changes the device, and this tentmaker MCP server is not running as root/Administrator; run the matching tentmaker command with %s, or start the server that way", how)
}

func (c controller) Status(ctx context.Context) (any, error) {
	v, err := c.a.statusData(ctx)
	return v, mcpErr(err)
}

func (c controller) Pool(ctx context.Context) (any, error) {
	v, err := c.a.poolData(ctx)
	return v, mcpErr(err)
}

func (c controller) Doctor(ctx context.Context) (any, error) {
	return c.a.doctorData(ctx), nil
}

func (c controller) SetBudget(ctx context.Context, everyday, boost string, cpus, boostCPUs float64) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.setBudget(budgetChange{Memory: everyday, BoostMemory: boost, CPUs: cpus, BoostCPUs: boostCPUs})
	return mcpErr(err)
}

func (c controller) AddRepo(ctx context.Context, repo, forge, mode string, allowPublic bool) error {
	if err := c.mutating(); err != nil {
		return err
	}
	// allow_public promises the fork approval step, which only Enable runs;
	// without it a public repo was saved with its fork policy unchanged and
	// the broker refused it (review r1 partB first-F4).
	_, err := c.a.repoAdd(ctx, repoAddOpts{Name: repo, Forge: forge, Mode: mode, AllowPublic: allowPublic, Enable: allowPublic})
	return mcpErr(err)
}

func (c controller) RemoveRepo(ctx context.Context, repo string) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.repoRemove(repo)
	return mcpErr(err)
}

func (c controller) Pause(ctx context.Context, reason string) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.pause(ctx, reason, 0)
	return mcpErr(err)
}

func (c controller) Resume(ctx context.Context) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.resume(ctx)
	return mcpErr(err)
}

func (c controller) MigrateWorkflows(ctx context.Context, path, platform string, write bool) (any, error) {
	v, err := c.a.migrate(path, platform, write, github.MigrateOptions{})
	return v, mcpErr(err)
}

func (c controller) InstallOnDevice(ctx context.Context, host, channel, memory, boost string) (any, error) {
	v, err := c.a.installOnDevice(ctx, host, mesh.InstallArgs{Channel: channel, Memory: memory, BoostMemory: boost})
	return v, mcpErr(err)
}

func (c controller) Update(ctx context.Context, channel string) (any, error) {
	if err := c.mutating(); err != nil {
		return nil, err
	}
	v, err := c.a.update(ctx, channel, false, false)
	if err != nil {
		return v, mcpErr(err)
	}
	// The tool promises that passing a channel switches the device to it,
	// including when this binary is already the newest on that channel
	// (review r1 partB sol-F7).
	if channel != "" && v.OK && !v.RolledBack && (v.Applied || !v.Available) {
		cfg, _, lerr := c.a.loadConfig()
		if lerr == nil && cfg.Update.Channel != channel {
			cfg.Update.Channel = channel
			if serr := c.a.saveConfig(cfg); serr != nil {
				v.Note += "; the channel was not saved: " + serr.Error()
			} else {
				v.Note += "; switched to the " + channel + " channel (" + c.a.reload() + ")"
			}
		}
	}
	return v, nil
}

func (c controller) Runs(ctx context.Context, repo, sha string, failed bool, limit int) (any, error) {
	v, err := c.a.runsData(ctx, runs.Query{Repo: repo, SHA: sha, Failed: failed, Limit: limit})
	return v, mcpErr(err)
}

func (c controller) Logs(ctx context.Context, jobID string, lines int) (any, error) {
	l, err := c.a.runLog(ctx, jobID, runs.LogTailBytes)
	if err != nil {
		return nil, mcpErr(err)
	}
	return jobLogView{ID: l.ID, Device: l.Device, Running: l.Running, Lines: nonNil(runs.TailLines(string(l.Data), lines))}, nil
}

func (c controller) Rerun(ctx context.Context, target string) (any, error) {
	if err := c.mutating(); err != nil {
		return nil, err
	}
	v, err := c.a.rerun(ctx, target)
	return v, mcpErr(err)
}

func (c controller) Stats(ctx context.Context, since, repo string) (any, error) {
	v, err := c.a.statsData(ctx, since, repo)
	return v, mcpErr(err)
}

func (c controller) Tests(ctx context.Context, jobID string, failedOnly bool) (any, error) {
	v, err := c.a.testsData(ctx, jobID, failedOnly)
	if err != nil {
		return nil, mcpErr(err)
	}
	return v, nil
}

func (c controller) Flaky(ctx context.Context, repo, since string) (any, error) {
	v, err := c.a.flakyData(ctx, repo, since)
	if err != nil {
		return nil, mcpErr(err)
	}
	return v, nil
}

func (c controller) WhyFailed(ctx context.Context, jobID, repo, sha string, runID int64) (any, error) {
	v, err := c.a.whyData(ctx, control.WhyRequest{ID: jobID, Repo: repo, SHA: sha, RunID: runID})
	if err != nil {
		return nil, mcpErr(err)
	}
	return v, nil
}

func (c controller) DebugExec(ctx context.Context, jobID, command string, timeoutS, maxBytes int64) (any, error) {
	if err := c.mutating(); err != nil {
		return nil, err
	}
	v, err := c.a.debugExec(ctx, control.DebugExecRequest{ID: jobID, Command: command, TimeoutS: timeoutS, MaxBytes: maxBytes})
	if err != nil {
		return nil, mcpErr(err)
	}
	return v, nil
}

// waitLogLines is how much of a failed job's log wait_for_run returns.
const waitLogLines = 80

// waitView is the answer of wait_for_run.
type waitView struct {
	Done       bool      `json:"done"`
	TimedOut   bool      `json:"timed_out"`
	WaitedS    float64   `json:"waited_s"`
	Conclusion string    `json:"conclusion,omitempty"`
	Jobs       []waitJob `json:"jobs"`
	Queued     []string  `json:"queued,omitempty"`
	Note       string    `json:"note,omitempty"`
}

type waitJob struct {
	ID         string   `json:"id"`
	Repo       string   `json:"repo"`
	Workflow   string   `json:"workflow,omitempty"`
	Job        string   `json:"job"`
	Device     string   `json:"device"`
	Conclusion string   `json:"conclusion"`
	FailedStep string   `json:"failed_step,omitempty"`
	RunURL     string   `json:"run_url,omitempty"`
	DurationS  float64  `json:"duration_s"`
	LogTail    []string `json:"log_tail,omitempty"`
}

// waitPoll is how often wait_for_run looks at the pool.
var waitPoll = 5 * time.Second

func (c controller) WaitForRun(ctx context.Context, repo, sha, runURL string, timeout time.Duration) (any, error) {
	q := runs.Query{Repo: repo, SHA: sha, Limit: runs.MaxLimit}
	if runURL != "" {
		r, id, ok := runs.ParseRunURL(runURL)
		if !ok {
			return nil, fmt.Errorf("%q is not a run URL like https://github.com/owner/repo/actions/runs/123", runURL)
		}
		q = runs.Query{Repo: r, RunID: id, Limit: runs.MaxLimit}
	}
	a := c.a
	look := func(ctx context.Context) (runs.Snapshot, error) {
		v, err := a.runsData(ctx, q)
		if err != nil {
			return runs.Snapshot{}, err
		}
		return runs.Snapshot{Records: v.Records, Queued: v.Queued}, nil
	}
	res, err := runs.Wait(ctx, look, runs.WaitOptions{
		Timeout: timeout,
		Poll:    waitPoll,
		Now:     a.env.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.env.Sleep(d)
			return ctx.Err()
		},
	})
	if err != nil {
		return nil, mcpErr(err)
	}
	now := a.env.Now()
	v := waitView{Done: res.Done, TimedOut: res.TimedOut, WaitedS: res.Waited, Conclusion: res.Conclusion, Queued: res.Queued, Jobs: []waitJob{}}
	for _, r := range res.Records {
		j := waitJob{ID: r.ID, Repo: r.Repo, Workflow: r.Workflow, Job: r.Job, Device: r.Device, Conclusion: r.Conclusion,
			FailedStep: r.FailedStep, RunURL: r.RunURL, DurationS: float64(r.Duration(now).Round(time.Second)) / float64(time.Second)}
		if r.Failed() {
			if l, lerr := a.runLog(ctx, r.ID, runs.LogTailBytes); lerr == nil {
				j.LogTail = runs.TailLines(string(l.Data), waitLogLines)
			}
		}
		v.Jobs = append(v.Jobs, j)
	}
	switch {
	case len(v.Jobs) == 0 && len(v.Queued) == 0:
		v.Note = "no Tentmaker job was seen for this commit; the forge may have run it elsewhere, or not queued it yet"
	case v.TimedOut:
		v.Note = "still running when the timeout passed; call wait_for_run again to keep waiting"
	case v.Conclusion == runs.Failure:
		var names []string
		for _, j := range v.Jobs {
			if j.Conclusion == runs.Failure || j.Conclusion == runs.Error {
				names = append(names, j.ID)
			}
		}
		v.Note = "failed: " + strings.Join(names, ", ") + "; read more with the logs tool, fix, push, and wait again, or rerun"
	}
	return v, nil
}

func (a *app) cmdMCP(args []string) (any, error) {
	f := a.flags("mcp", commands["mcp"].usage)
	rootFlag := f.String("root", "", "the only folder sandbox_cp may copy files out of a sandbox into (default: the current folder)")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker mcp [--root DIR]", "unexpected argument %q", pos[0])
	}
	root := *rootFlag
	if root == "" {
		if a.env.Getwd == nil {
			return nil, usage("pass --root DIR", "the current folder is unknown")
		}
		if root, err = a.env.Getwd(); err != nil {
			return nil, usage("pass --root DIR", "the current folder: %v", err)
		}
	}
	if a.mcpRoot, err = resolveRoot(root); err != nil {
		return nil, usage("pass --root with a folder that exists", "--root %s: %v", root, err)
	}
	// stdout is the protocol: nothing else may print there.
	a.json = true
	if err := mcp.Serve(a.ctx, a.in, a.env.Stdout, controller{a}, a.env.Version); err != nil && a.ctx.Err() == nil {
		fmt.Fprintln(a.env.Stderr, "tentmaker mcp:", err)
		return silentExit(ExitFailed), nil
	}
	return nil, nil
}

// silentExit ends a command with a code and prints nothing more, for
// commands whose stdout is a protocol (mcp).
type silentExit int

func (s silentExit) exitCode() int { return int(s) }

var _ mcp.SandboxController = controller{}

// The sandbox tools need no root here: the daemon decides who may use
// sandboxes from the connection.

func (c controller) SandboxCreate(ctx context.Context, os, device string, cpus float64, memory, ttl, copyDir string) (any, error) {
	v, err := c.a.sandboxCreate(ctx, sandboxOpts{OS: os, Device: device, CPUs: cpus, Memory: memory, TTL: ttl, Copy: copyDir})
	return v, mcpErr(err)
}

func (c controller) SandboxExec(ctx context.Context, id string, argv []string, dir string, timeoutS float64, stdin string) (any, error) {
	// The tool checks this too; a fraction or a negative must never reach
	// the request, where 0.5 would truncate to 0, which means no limit.
	if timeoutS < 0 || timeoutS > mcp.MaxSandboxTimeoutS || timeoutS != math.Trunc(timeoutS) {
		return nil, fmt.Errorf("timeout_s must be a whole number from 0 to %d, not %g", mcp.MaxSandboxTimeoutS, timeoutS)
	}
	var in io.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	}
	start := time.Now()
	out, errs := &tailBuffer{max: mcpMaxOutput}, &tailBuffer{max: mcpMaxOutput}
	e, err := c.a.sandboxExec(ctx, id, sandbox.ExecRequest{Argv: argv, Dir: dir, TimeoutS: int64(timeoutS)}, in, out, errs)
	if err != nil {
		return nil, mcpErr(err)
	}
	v := mcpExecView{ID: id, ExitCode: e.ExitCode, Stdout: out.String(), Stderr: errs.String(),
		StdoutCutBytes: out.cut(), StderrCutBytes: errs.cut(), Seconds: time.Since(start).Seconds()}
	if v.StdoutCutBytes > 0 || v.StderrCutBytes > 0 {
		v.Note = fmt.Sprintf("output was cut to the last %d KiB of each stream (stdout_cut_bytes and stderr_cut_bytes say how much was dropped from the start); "+
			"write long output to a file in the sandbox and read the part you need, or copy it out with sandbox_cp", mcpMaxOutput>>10)
	}
	return v, nil
}

// mcpMaxOutput caps each stream sandbox_exec returns to an agent: enough
// for a test failure or a build error, small enough for a model's context.
const mcpMaxOutput = 64 << 10

// mcpExecView is sandbox_exec's answer.
type mcpExecView struct {
	ID             string  `json:"id"`
	ExitCode       int     `json:"exit_code"`
	Stdout         string  `json:"stdout"`
	Stderr         string  `json:"stderr"`
	StdoutCutBytes int64   `json:"stdout_cut_bytes,omitempty"`
	StderrCutBytes int64   `json:"stderr_cut_bytes,omitempty"`
	Seconds        float64 `json:"seconds"`
	Note           string  `json:"note,omitempty"`
}

// tailBuffer keeps the last max bytes written to it and counts the rest:
// the end of a command's output is where its error usually is.
type tailBuffer struct {
	max   int
	b     []byte
	total int64
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.total += int64(len(p))
	t.b = append(t.b, p...)
	// Trim in batches so a stream of small writes stays linear.
	if len(t.b) > 2*t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

// tail is what the buffer keeps, starting on a UTF-8 boundary.
func (t *tailBuffer) tail() []byte {
	b := t.b
	if len(b) > t.max {
		b = b[len(b)-t.max:]
		for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
			b = b[1:]
		}
	}
	return b
}

func (t *tailBuffer) String() string { return string(t.tail()) }

// cut is how many bytes from the start were dropped.
func (t *tailBuffer) cut() int64 { return t.total - int64(len(t.tail())) }

// SandboxCopy confines a copy out of a sandbox to the server's --root: an
// agent steered by text inside a sandbox must not be able to write host
// files, least of all when the server runs as root.
func (c controller) SandboxCopy(ctx context.Context, from, to string) (any, error) {
	_, _, fromSandbox := splitRemote(from)
	_, _, toSandbox := splitRemote(to)
	if fromSandbox && !toSandbox {
		dst, err := underRoot(c.a.mcpRoot, to)
		if err != nil {
			return nil, err
		}
		to = dst
	}
	v, err := c.a.sandboxCopy(ctx, from, to)
	return v, mcpErr(err)
}

// resolveRoot is dir made absolute with its symlinks resolved; it must be
// a folder.
func resolveRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("not a folder")
	}
	return real, nil
}

// underRoot resolves dst (relative to root when it isn't absolute) with
// the symlinks of the part that exists, and refuses it unless it is root
// or a folder under it. What doesn't exist yet can't hold a symlink, so
// the answer is where the copy lands; the copy itself then writes only
// below it, through os.Root, and replaces an existing file there instead
// of writing through it.
func underRoot(root, dst string) (string, error) {
	if root == "" {
		return "", errors.New("this MCP server has no folder to copy into; start it with tentmaker mcp --root DIR")
	}
	refuse := func() error {
		return fmt.Errorf("sandbox_cp writes only under %s, the folder this MCP server was started with (--root); %q is outside it", root, dst)
	}
	p := dst
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	var rest []string
	for {
		_, err := os.Lstat(p)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s: %v", dst, err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", refuse()
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%s: %v", dst, err)
	}
	full := filepath.Join(append([]string{real}, rest...)...)
	rel, err := filepath.Rel(root, full)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return "", refuse()
	}
	return full, nil
}

func (c controller) SandboxList(ctx context.Context) (any, error) {
	v, err := c.a.sandboxList(ctx)
	return v, mcpErr(err)
}

func (c controller) SandboxRemove(ctx context.Context, id string) (any, error) {
	v, err := c.a.sandboxRemove(ctx, []string{id})
	return v, mcpErr(err)
}
