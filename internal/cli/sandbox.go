package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/sandbox"
)

const usageSandbox = `tentmaker sandbox create [--os linux|macos|windows] [--device D] [--cpus N] [--memory SIZE] [--ttl 30m] [--copy DIR] [--json]
tentmaker sandbox exec ID [--dir D] [--timeout D] [-i] [--json] -- COMMAND [ARGS...]
tentmaker sandbox cp LOCAL ID:FOLDER       copy a file or folder into the sandbox's FOLDER
tentmaker sandbox cp ID:PATH LOCAL_FOLDER  copy a file or folder out of the sandbox
tentmaker sandbox ls [--json]
tentmaker sandbox rm ID... [--json]

A sandbox is a throwaway machine for an agent on one of your devices: the
isolation a CI job gets (on Linux its own user, cgroup, network namespace and
the job fence), counted against the device's budget like a job. It ends when
its time to live runs out (default 30m), on rm, or when the service restarts.

create prints the sandbox's ID. --copy DIR copies DIR's working tree into the
sandbox's work folder first: in a Git work tree, uncommitted changes and
untracked files Git doesn't ignore, and no .git.

exec streams the command's output and exits with its exit code. Commands run
in the sandbox's work folder unless --dir says otherwise. -i passes stdin.
With --json it prints one object with the exit code and the output (each
stream capped at 1 MiB).

Root (or an Administrator) may use every sandbox. Other accounts need to be
listed in users in the [sandbox] section of the config, and see only their own.`

// sandboxTimeouts bound the calls that don't run a command.
const (
	sandboxCallTimeout   = 2 * time.Minute
	sandboxCreateTimeout = 12 * time.Minute
	sandboxCopyTimeout   = 31 * time.Minute
	// sandboxExecLimit bounds an exec with no --timeout: the longest time
	// to live the config allows, and a minute.
	sandboxExecLimit = 7*24*time.Hour + time.Minute
	// maxJSONOutput caps each stream an exec with --json (or MCP) keeps.
	maxJSONOutput = 1 << 20
)

func (a *app) cmdSandbox(args []string) (any, error) {
	if len(args) == 0 {
		return nil, usage("usage: "+firstLine(usageSandbox), "sandbox needs a subcommand: create, exec, cp, ls or rm")
	}
	switch args[0] {
	case "create":
		return a.cmdSandboxCreate(args[1:])
	case "exec":
		return a.cmdSandboxExec(args[1:])
	case "cp":
		return a.cmdSandboxCopy(args[1:])
	case "ls", "list":
		return a.cmdSandboxList(args[1:])
	case "rm", "remove":
		return a.cmdSandboxRemove(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(a.env.Stdout, usageSandbox)
		return nil, nil
	}
	return nil, usage("usage: "+firstLine(usageSandbox), "unknown sandbox subcommand %q", args[0])
}

// sandboxOpts is a create request as the CLI and MCP take it.
type sandboxOpts struct {
	OS, Device, Memory, TTL, Copy string
	CPUs                          float64
}

type sandboxView struct {
	sandbox.Info
	Copied *copyView `json:"copied,omitempty"`
}

type copyView struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Files   int    `json:"files,omitempty"`
	Bytes   int64  `json:"bytes"`
	Skipped int    `json:"skipped,omitempty"` // links and special files not written here
}

func (a *app) cmdSandboxCreate(args []string) (any, error) {
	f := a.flags("sandbox create", usageSandbox)
	var o sandboxOpts
	f.StringVar(&o.OS, "os", "linux", "linux, macos or windows")
	f.StringVar(&o.Device, "device", "", "the device to run it on (this one)")
	f.Float64Var(&o.CPUs, "cpus", 0, "CPUs (default: one job's)")
	f.StringVar(&o.Memory, "memory", "", "memory like 4GiB (default: one job's)")
	f.StringVar(&o.TTL, "ttl", "30m", "time to live")
	f.StringVar(&o.Copy, "copy", "", "copy this folder's working tree in")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+firstLine(usageSandbox), "unexpected argument %q", pos[0])
	}
	v, err := a.sandboxCreate(a.ctx, o)
	if err != nil {
		return nil, err
	}
	if a.json {
		return v, nil
	}
	a.out("%s\n", v.ID)
	fmt.Fprintf(a.env.Stderr, "sandbox %s on %s (%s, %g CPUs, %s), expires %s\n", v.ID, v.Device, v.Platform, v.CPUs,
		config.Size(v.MemoryBytes), v.Expires.Local().Format("15:04"))
	if v.Copied != nil {
		fmt.Fprintf(a.env.Stderr, "copied %d files (%s) from %s\n", v.Copied.Files, config.Size(v.Copied.Bytes), v.Copied.From)
	}
	return nil, nil
}

func (a *app) sandboxCreate(ctx context.Context, o sandboxOpts) (sandboxView, error) {
	req := sandbox.CreateRequest{Device: o.Device, OS: o.OS, CPUs: o.CPUs}
	if o.Memory != "" {
		s, err := config.ParseSize(o.Memory)
		if err != nil || s <= 0 {
			return sandboxView{}, usage("write it like 4GiB or 512MiB", "--memory %q is not a size", o.Memory)
		}
		req.MemoryBytes = int64(s)
	}
	if o.CPUs < 0 || o.CPUs != o.CPUs {
		return sandboxView{}, usage("write it like --cpus 2", "--cpus can't be negative")
	}
	if o.TTL != "" {
		d, err := time.ParseDuration(o.TTL)
		if err != nil || d <= 0 {
			return sandboxView{}, usage("write it like 30m or 2h", "--ttl %q is not a duration", o.TTL)
		}
		req.TTLSeconds = int64(d / time.Second)
	}
	var dir string
	if o.Copy != "" {
		var err error
		if dir, err = filepath.Abs(o.Copy); err == nil {
			var st os.FileInfo
			if st, err = os.Stat(dir); err == nil && !st.IsDir() {
				err = errors.New("not a folder")
			}
		}
		if err != nil {
			return sandboxView{}, usage("name a folder, like --copy .", "--copy %s: %v", o.Copy, err)
		}
	}
	c, err := a.sandboxClient()
	if err != nil {
		return sandboxView{}, err
	}
	var v sandboxView
	if err := c.SandboxCall(ctx, http.MethodPost, "/v1/sandboxes", req, &v.Info, sandboxCreateTimeout); err != nil {
		return sandboxView{}, sandboxErr(err)
	}
	if dir == "" {
		return v, nil
	}
	files := 0
	cv, err := a.copyIn(ctx, c, v.ID, ".", dir, func(w io.Writer) error {
		n, err := sandbox.PackWorkTree(ctx, w, dir, a.gitLister())
		files = n
		return err
	})
	if err != nil {
		// A sandbox without the code it was made for is no use; don't
		// leave it holding a slot.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sandboxCallTimeout)
		defer cancel()
		_ = c.SandboxCall(rctx, http.MethodDelete, "/v1/sandboxes/"+v.ID, nil, nil, sandboxCallTimeout)
		return sandboxView{}, err
	}
	cv.Files = files
	v.Copied = &cv
	return v, nil
}

// gitLister lists a Git work tree's files with the host's git.
func (a *app) gitLister() sandbox.GitLister {
	if a.env.GitLsFiles != nil {
		return a.env.GitLsFiles
	}
	return func(ctx context.Context, dir string) ([]byte, error) {
		git, err := lookTool("git")
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		cmd := execCommand(ctx, git, "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
}

// execView is an exec's answer with --json or over MCP.
type execView struct {
	ID              string  `json:"id"`
	ExitCode        int     `json:"exit_code"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	StdoutTruncated bool    `json:"stdout_truncated,omitempty"`
	StderrTruncated bool    `json:"stderr_truncated,omitempty"`
	Seconds         float64 `json:"seconds"`
}

func (v execView) exitCode() int { return v.ExitCode }

// codeOnly ends a streamed exec with the command's exit code.
type codeOnly int

func (c codeOnly) exitCode() int { return int(c) }

func (a *app) cmdSandboxExec(args []string) (any, error) {
	var argv []string
	for i, s := range args {
		if s == "--" {
			args, argv = args[:i], args[i+1:]
			break
		}
	}
	f := a.flags("sandbox exec", usageSandbox)
	dir := f.String("dir", "", "the folder to run in (default: the work folder)")
	timeout := f.Duration("timeout", 0, "stop the command after this long (default: when the sandbox expires)")
	stdin := f.Bool("i", false, "pass stdin to the command")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) == 0 {
		return nil, usage("usage: tentmaker sandbox exec ID -- COMMAND [ARGS...]", "name the sandbox")
	}
	argv = append(pos[1:], argv...)
	if len(argv) == 0 {
		return nil, usage("put the command after --, like tentmaker sandbox exec "+pos[0]+" -- go test ./...", "no command to run")
	}
	if *timeout < 0 {
		return nil, usage("write it like 10m", "--timeout can't be negative")
	}
	req := sandbox.ExecRequest{Argv: argv, Dir: *dir, TimeoutS: int64(*timeout / time.Second)}
	var in io.Reader
	if *stdin {
		in = a.env.Stdin
	}
	if a.json {
		return a.sandboxExecCollect(a.ctx, pos[0], req, in)
	}
	e, err := a.sandboxExec(a.ctx, pos[0], req, in, a.env.Stdout, a.env.Stderr)
	if err != nil {
		return nil, err
	}
	return codeOnly(e.ExitCode), nil
}

// sandboxExecCollect runs a command and keeps its output.
func (a *app) sandboxExecCollect(ctx context.Context, id string, req sandbox.ExecRequest, stdin io.Reader) (execView, error) {
	start := time.Now()
	var out, errs capWriter
	e, err := a.sandboxExec(ctx, id, req, stdin, &out, &errs)
	if err != nil {
		return execView{}, err
	}
	return execView{ID: id, ExitCode: e.ExitCode, Stdout: out.b.String(), Stderr: errs.b.String(),
		StdoutTruncated: out.cut, StderrTruncated: errs.cut, Seconds: time.Since(start).Seconds()}, nil
}

// sandboxExec runs one command, streaming its output to the writers.
func (a *app) sandboxExec(ctx context.Context, id string, req sandbox.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (sandbox.Exit, error) {
	if !sandbox.ValidID(id) {
		return sandbox.Exit{}, usage("list them with tentmaker sandbox ls", "%q is not a sandbox ID", id)
	}
	c, err := a.sandboxClient()
	if err != nil {
		return sandbox.Exit{}, err
	}
	line, err := json.Marshal(req)
	if err != nil {
		return sandbox.Exit{}, err
	}
	body := io.Reader(bytes.NewReader(append(line, '\n')))
	if stdin != nil {
		body = io.MultiReader(body, stdin)
	}
	limit := sandboxExecLimit
	if req.TimeoutS > 0 {
		limit = time.Duration(req.TimeoutS)*time.Second + time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	resp, err := c.Stream(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/exec", body)
	if err != nil {
		return sandbox.Exit{}, sandboxErr(err)
	}
	defer resp.Body.Close()
	e, err := sandbox.ReadFrames(resp.Body, stdout, stderr)
	if err != nil {
		return sandbox.Exit{}, fail("check that the sandbox still runs: tentmaker sandbox ls", "%v", err)
	}
	if e.Error != "" {
		fix := e.Fix
		if fix == "" {
			fix = "check that the sandbox still runs: tentmaker sandbox ls"
		}
		return e, fail(fix, "%s", e.Error)
	}
	return e, nil
}

// capWriter keeps the first maxJSONOutput bytes.
type capWriter struct {
	b   bytes.Buffer
	cut bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	room := maxJSONOutput - c.b.Len()
	if len(p) > room {
		c.cut = true
		if room > 0 {
			c.b.Write(p[:room])
		}
		return len(p), nil
	}
	c.b.Write(p)
	return len(p), nil
}

func (a *app) cmdSandboxCopy(args []string) (any, error) {
	f := a.flags("sandbox cp", usageSandbox)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 2 {
		return nil, usage("usage: tentmaker sandbox cp LOCAL ID:FOLDER, or tentmaker sandbox cp ID:PATH LOCAL_FOLDER", "cp takes a source and a destination")
	}
	v, err := a.sandboxCopy(a.ctx, pos[0], pos[1])
	if err != nil {
		return nil, err
	}
	if a.json {
		return v, nil
	}
	a.out("copied %s from %s to %s\n", config.Size(v.Bytes), v.From, v.To)
	return nil, nil
}

// splitRemote reads "ID:PATH".
func splitRemote(s string) (id, p string, ok bool) {
	id, p, ok = strings.Cut(s, ":")
	if !ok || !sandbox.ValidID(id) {
		return "", "", false
	}
	return id, p, true
}

func (a *app) sandboxCopy(ctx context.Context, src, dst string) (copyView, error) {
	sid, spath, sRemote := splitRemote(src)
	did, dpath, dRemote := splitRemote(dst)
	c, err := a.sandboxClient()
	if err != nil {
		return copyView{}, err
	}
	switch {
	case sRemote && dRemote:
		return copyView{}, usage("copy out to a local folder, then in", "copying from one sandbox straight to another is not supported")
	case dRemote:
		local, err := filepath.Abs(src)
		if err == nil {
			_, err = os.Lstat(local)
		}
		if err != nil {
			return copyView{}, usage("name a local file or folder", "%s: %v", src, err)
		}
		return a.copyIn(ctx, c, did, dpath, local, func(w io.Writer) error { return sandbox.PackPath(w, local) })
	case sRemote:
		if spath == "" {
			return copyView{}, usage("name the path in the sandbox, like "+sid+":out", "no path to copy")
		}
		return a.copyOut(ctx, c, sid, spath, dst)
	}
	return copyView{}, usage("write the sandbox side as ID:PATH, like sbx-1a2b3c4d5e6f:src", "neither %s nor %s names a sandbox", src, dst)
}

// copyIn streams what pack writes into folder dst of sandbox id.
func (a *app) copyIn(ctx context.Context, c *control.Client, id, dst, from string, pack func(io.Writer) error) (copyView, error) {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(pack(pw)) }()
	defer pr.Close()
	ctx, cancel := context.WithTimeout(ctx, sandboxCopyTimeout)
	defer cancel()
	resp, err := c.Stream(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/files?path="+url.QueryEscape(dst), pr)
	if err != nil {
		var ce *control.Error
		if !errors.As(err, &ce) && !errors.Is(err, control.ErrNotRunning) {
			return copyView{}, fail("check the local path and that the sandbox still runs", "copy %s into %s: %v", from, id, err)
		}
		return copyView{}, sandboxErr(err)
	}
	defer resp.Body.Close()
	var res sandbox.CopyResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil {
		return copyView{}, fail("tentmaker doctor", "read the copy's answer: %v", err)
	}
	return copyView{ID: id, From: from, To: id + ":" + res.Path, Bytes: res.Bytes}, nil
}

// copyOut unpacks src from sandbox id into the local folder dst.
func (a *app) copyOut(ctx context.Context, c *control.Client, id, src, dst string) (copyView, error) {
	dst, err := filepath.Abs(dst)
	if err != nil {
		return copyView{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxCopyTimeout)
	defer cancel()
	resp, err := c.Stream(ctx, http.MethodGet, "/v1/sandboxes/"+id+"/files?path="+url.QueryEscape(src), nil)
	if err != nil {
		return copyView{}, sandboxErr(err)
	}
	defer resp.Body.Close()
	pr, pw := io.Pipe()
	type result struct {
		r   sandbox.ExtractResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := sandbox.Extract(pr, dst)
		// Drain so a failed extract can't stall the frame reader.
		_, _ = io.Copy(io.Discard, pr)
		done <- result{r, err}
	}()
	var stderr capWriter
	e, ferr := sandbox.ReadFrames(resp.Body, pw, &stderr)
	pw.CloseWithError(ferr)
	res := <-done
	switch {
	case ferr != nil:
		return copyView{}, fail("check that the sandbox still runs: tentmaker sandbox ls", "copy %s:%s: %v", id, src, ferr)
	case e.Error != "":
		return copyView{}, fail(e.Fix, "copy %s:%s: %s", id, src, e.Error)
	case e.ExitCode != 0:
		return copyView{}, fail("check the path; a relative path starts in the sandbox's work folder",
			"copy %s:%s: tar exited %d: %s", id, src, e.ExitCode, strings.TrimSpace(stderr.b.String()))
	case res.err != nil:
		return copyView{}, fail("copy into an empty folder", "copy %s:%s into %s: %v", id, src, dst, res.err)
	}
	return copyView{ID: id, From: id + ":" + src, To: dst, Files: res.r.Files, Bytes: res.r.Bytes, Skipped: res.r.Skipped}, nil
}

type sandboxList struct {
	Sandboxes []sandbox.Info `json:"sandboxes"`
}

func (a *app) cmdSandboxList(args []string) (any, error) {
	f := a.flags("sandbox ls", usageSandbox)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker sandbox ls", "unexpected argument %q", pos[0])
	}
	v, err := a.sandboxList(a.ctx)
	if err != nil {
		return nil, err
	}
	if a.json {
		return v, nil
	}
	if len(v.Sandboxes) == 0 {
		a.out("no sandboxes\n")
		return nil, nil
	}
	a.out("%-17s %-12s %-12s %5s %9s %-8s %s\n", "ID", "DEVICE", "PLATFORM", "CPUS", "MEMORY", "EXPIRES", "STATE")
	for _, s := range v.Sandboxes {
		a.out("%-17s %-12s %-12s %5g %9s %-8s %s\n", s.ID, s.Device, s.Platform, s.CPUs, config.Size(s.MemoryBytes),
			s.Expires.Local().Format("15:04"), s.State)
	}
	return nil, nil
}

func (a *app) sandboxList(ctx context.Context) (sandboxList, error) {
	c, err := a.sandboxClient()
	if err != nil {
		return sandboxList{}, err
	}
	var v sandboxList
	if err := c.SandboxCall(ctx, http.MethodGet, "/v1/sandboxes", nil, &v, sandboxCallTimeout); err != nil {
		return sandboxList{}, sandboxErr(err)
	}
	if v.Sandboxes == nil {
		v.Sandboxes = []sandbox.Info{}
	}
	return v, nil
}

type removeView struct {
	Removed []string `json:"removed"`
}

func (a *app) cmdSandboxRemove(args []string) (any, error) {
	f := a.flags("sandbox rm", usageSandbox)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) == 0 {
		return nil, usage("usage: tentmaker sandbox rm ID...", "name the sandbox to remove")
	}
	v, err := a.sandboxRemove(a.ctx, pos)
	if err != nil {
		return nil, err
	}
	if a.json {
		return v, nil
	}
	for _, id := range v.Removed {
		a.out("removed %s\n", id)
	}
	return nil, nil
}

func (a *app) sandboxRemove(ctx context.Context, ids []string) (removeView, error) {
	for _, id := range ids {
		if !sandbox.ValidID(id) {
			return removeView{}, usage("list them with tentmaker sandbox ls", "%q is not a sandbox ID", id)
		}
	}
	c, err := a.sandboxClient()
	if err != nil {
		return removeView{}, err
	}
	v := removeView{Removed: []string{}}
	for _, id := range ids {
		if err := c.SandboxCall(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil, sandboxCallTimeout); err != nil {
			err = sandboxErr(err)
			if len(v.Removed) > 0 {
				var ce *cliError
				if errors.As(err, &ce) {
					ce.msg += " (removed before it: " + strings.Join(v.Removed, ", ") + ")"
				}
			}
			return v, err
		}
		v.Removed = append(v.Removed, id)
	}
	return v, nil
}

// sandboxClient is the control client, with the token when this account
// can read it (root and Administrators are admins anyway).
func (a *app) sandboxClient() (*control.Client, error) {
	cfg, _, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	return a.controlClient(cfg), nil
}

// sandboxErr turns a control API refusal into a CLI error with its fix.
func sandboxErr(err error) error {
	var ce *control.Error
	if errors.As(err, &ce) {
		fix := ce.Fix
		if fix == "" {
			fix = "tentmaker doctor"
		}
		return fail(fix, "%s", ce.Message)
	}
	return serviceErr(err)
}
