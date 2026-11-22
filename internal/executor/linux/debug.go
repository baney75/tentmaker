package linux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
	"github.com/baney75/tentmaker/internal/testresults"
)

// Debugging a failed job (docs/DESIGN.md, "Debugging a failed job").
//
// A kept job is one whose unit has exited but whose folder, slot, network
// namespace holder and uplink are left in place. A debug command runs as a
// transient unit of its own, tentmaker-debug-<job>-<random>, built like the
// job's: the runner's uid and gid, the job's memory and CPU limits, its
// mounts (not its repo cache), NoNewPrivileges, and the job's own network
// namespace, so the network fence (which matches the runner's uid) and the
// namespace both still apply. Clean and Stop stop every debug unit of the
// job before they take its namespace and folder away.

var _ core.Debugger = (*Executor)(nil)

// DebugPrefix starts the name of every debug command's unit.
const DebugPrefix = "tentmaker-debug-"

// workspaces are where a job's tests write their reports, under its folder.
var workspaces = []string{"runner/_work", "work"}

// runnerCredentials are the files the runner keeps its registration in. A
// kept job's runner is already deleted at the forge; these go too.
var runnerCredentials = []string{"runner/.runner", "runner/.credentials", "runner/.credentials_rsaparams"}

// Streamer is a Host that can run a command with its input and output
// joined to the caller's, for an interactive debug shell and for a debug
// command's output.
type Streamer interface {
	// Stream runs c with stdin and stdout (stderr too) until it exits, ctx
	// ends or c.Timeout passes, and returns its exit code. A non-zero exit
	// is not an error.
	Stream(ctx context.Context, c Cmd, stdin io.Reader, stdout io.Writer) (int, error)
}

// TestResults implements dispatch.TestCollector: the JUnit reports a job
// wrote in its workspace. It reads as root without following a symlink.
func (e *Executor) TestResults(ctx context.Context, name string) (runs.TestResults, error) {
	if err := checkName(name); err != nil {
		return runs.TestResults{}, err
	}
	dir := e.jobDir(name)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return runs.TestResults{}, core.ErrNotFound
	}
	return testresults.Collect(dir, workspaces, testresults.Limits{}), nil
}

// Keep implements core.Debugger: it leaves an exited job's folder and
// namespace for Clean, and removes the runner's credentials.
func (e *Executor) Keep(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	st, err := e.State(ctx, name)
	if err != nil {
		return err
	}
	if st != core.ExecExited {
		return fmt.Errorf("job %s is %s, not exited", name, st)
	}
	os.Remove(e.secretPath(name))
	if _, err := e.netnsPID(ctx, netnsUnit(name)); err != nil {
		return fmt.Errorf("the job's network namespace is gone: %w", err)
	}
	root, err := os.OpenRoot(e.jobDir(name))
	if err != nil {
		return fmt.Errorf("open the job's folder: %w", err)
	}
	defer root.Close()
	for _, f := range runnerCredentials {
		// Remove never follows a final symlink, and os.Root refuses a path
		// that leaves the folder.
		if err := root.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the runner's %s: %w", filepath.Base(f), err)
		}
	}
	e.log.Info("job kept for debugging", "executor", Name, "name", name)
	return nil
}

// debugUnit names a new debug unit of job name.
func debugUnit(name string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return DebugPrefix + name + "-" + hex.EncodeToString(b) + ".service"
}

// debugUnits is the pattern of every debug unit of job name.
func debugUnits(name string) string { return DebugPrefix + name + "-*.service" }

// debugArgs is the systemd-run argv of one debug unit: the job's sandbox,
// with tty or with its stdio piped. It holds no secret and no command text
// a caller gave; that goes on stdin.
func (e *Executor) debugArgs(name, unit string, spec core.DebugSpec, tty bool, uid, gid, nsPID int, argv ...string) []string {
	dir := e.jobDir(name)
	args := []string{"--quiet", "--no-ask-password", "--wait",
		"--unit=" + unit,
		"--slice=" + Slice,
		"--uid=" + strconv.Itoa(uid),
		"--gid=" + strconv.Itoa(gid),
		"-p", "Type=exec",
		"-p", "MemoryMax=" + strconv.FormatInt(spec.Limits.MemoryBytes, 10),
		"-p", "MemorySwapMax=0",
		"-p", "CPUQuota=" + cpuQuota(spec.Limits.CPUs),
		"-p", "TasksMax=4096",
		"-p", "Nice=10",
		"-p", "RuntimeMaxSec=" + strconv.FormatInt(int64(max(spec.Timeout, time.Second)/time.Second), 10),
		"-p", "TimeoutStopSec=10",
		"-p", "NoNewPrivileges=yes",
		"-p", "PrivateTmp=yes",
		"-p", "NetworkNamespacePath=" + filepath.Join(e.p.Proc, strconv.Itoa(nsPID), "ns", "net"),
	}
	if tty {
		args = append(args, "--pty")
	} else {
		args = append(args, "--pipe")
	}
	for _, p := range e.jobMounts(uid, dir) {
		args = append(args, "-p", p)
	}
	env := e.jobEnv(core.ExecSpec{Name: name, Kind: e.kindOf(name)}, uid)
	if tty {
		env = append(env, "TERM=xterm-256color")
	}
	for _, kv := range env {
		args = append(args, "--setenv="+kv)
	}
	return append(append(args, "--working-directory="+e.workspace(name), "--"), argv...)
}

// kindOf says whether job name is a check job (it has a work folder with
// out) or a runner job.
func (e *Executor) kindOf(name string) string {
	if _, err := os.Lstat(filepath.Join(e.jobDir(name), "runner")); err == nil {
		return core.KindRunner
	}
	return core.KindCheck
}

// workspace is the folder a debug command starts in: the runner's _work
// (where actions/checkout put the repo) or a check job's work folder.
func (e *Executor) workspace(name string) string {
	dir := e.jobDir(name)
	for _, w := range workspaces {
		if fi, err := os.Lstat(filepath.Join(dir, w)); err == nil && fi.IsDir() {
			return filepath.Join(dir, w)
		}
	}
	return dir
}

// debugReady checks that job name is kept and returns the runner's ids and
// the pid holding its network namespace.
func (e *Executor) debugReady(ctx context.Context, name string, spec core.DebugSpec) (Streamer, int, int, int, error) {
	if err := checkName(name); err != nil {
		return nil, 0, 0, 0, err
	}
	s, ok := e.h.(Streamer)
	if !ok {
		return nil, 0, 0, 0, errors.New("this host can't run a debug command")
	}
	if spec.Limits.MemoryBytes <= 0 || spec.Limits.CPUs <= 0 || spec.Timeout <= 0 {
		return nil, 0, 0, 0, errors.New("a debug command needs a memory and CPU limit and a timeout")
	}
	st, err := e.State(ctx, name)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if st != core.ExecExited {
		return nil, 0, 0, 0, fmt.Errorf("job %s is %s, not a kept sandbox", name, st)
	}
	uid, gid, err := e.ids()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	pid, err := e.netnsPID(ctx, netnsUnit(name))
	if err != nil {
		// Fail closed: no debug command runs outside the job's namespace.
		return nil, 0, 0, 0, fmt.Errorf("the job's network namespace is gone: %w", err)
	}
	return s, uid, gid, pid, nil
}

// DebugExec implements core.Debugger. The command goes to sh on stdin, so
// it is in no argv on the host; its output is the last spec.MaxOutput
// bytes of stdout and stderr together.
func (e *Executor) DebugExec(ctx context.Context, name, command string, spec core.DebugSpec) (core.DebugResult, error) {
	s, uid, gid, pid, err := e.debugReady(ctx, name, spec)
	if err != nil {
		return core.DebugResult{}, err
	}
	unit := debugUnit(name)
	args := e.debugArgs(name, unit, spec, false, uid, gid, pid, "/bin/sh", "-s")
	out := &tailWriter{max: max(spec.MaxOutput, 1)}
	script := "exec 2>&1\n" + command + "\n"
	start := time.Now()
	code, err := s.Stream(ctx, Cmd{Path: "systemd-run", Args: args, Timeout: spec.Timeout + 30*time.Second},
		strings.NewReader(script), out)
	timedOut := ctx.Err() != nil || time.Since(start) >= spec.Timeout || e.unitResult(context.WithoutCancel(ctx), unit) == "timeout"
	e.stopUnits(context.WithoutCancel(ctx), unit)
	if err != nil && !timedOut {
		return core.DebugResult{}, fmt.Errorf("run the debug command: %v", err)
	}
	return core.DebugResult{ExitCode: code, Output: out.Bytes(), Truncated: out.cut, TimedOut: timedOut}, nil
}

// DebugShell implements core.Debugger: an interactive shell, bash when the
// host has it, on a pty systemd gives the unit.
func (e *Executor) DebugShell(ctx context.Context, name string, spec core.DebugSpec, stdin io.Reader, stdout io.Writer) error {
	s, uid, gid, pid, err := e.debugReady(ctx, name, spec)
	if err != nil {
		return err
	}
	unit := debugUnit(name)
	args := e.debugArgs(name, unit, spec, true, uid, gid, pid,
		"/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash -i; exec sh -i")
	_, err = s.Stream(ctx, Cmd{Path: "systemd-run", Args: args, Timeout: spec.Timeout + 30*time.Second}, stdin, stdout)
	// Ending systemd-run does not end the unit; this does.
	e.stopUnits(context.WithoutCancel(ctx), unit)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("debug shell: %v", err)
	}
	return nil
}

// unitResult is a unit's Result ("success", "timeout", ...), or "".
func (e *Executor) unitResult(ctx context.Context, unit string) string {
	res, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: []string{"show", "--property=Result", "--value", unit}, Timeout: 30 * time.Second})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// stopDebug stops every debug unit of job name.
func (e *Executor) stopDebug(ctx context.Context, name string) {
	e.stopUnits(ctx, debugUnits(name))
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	b   bytes.Buffer
	max int64
	cut bool
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.b.Write(p)
	if over := int64(w.b.Len()) - w.max; over > 0 {
		w.b.Next(int(over))
		w.cut = true
	}
	return n, nil
}

func (w *tailWriter) Bytes() []byte { return append([]byte(nil), w.b.Bytes()...) }
