package linux

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// SandboxPrefix starts every sandbox's name. Only such names get a slice of
// their own, so no runner name can reach the slices sandboxes share.
const SandboxPrefix = "sbx-"

// ExecPrefix starts the unit of each command a sandbox runs.
const ExecPrefix = "tentmaker-exec-"

func isSandboxName(name string) bool {
	return strings.HasPrefix(name, SandboxPrefix) && core.NamePattern.MatchString(name)
}

// sandboxSlice holds a sandbox's idle unit and every command it runs, under
// tentmaker.slice (so the pool limit covers it); its MemoryMax and CPUQuota
// are the sandbox's.
func sandboxSlice(name string) string { return "tentmaker-" + name + ".slice" }

// limitsPath is where a sandbox's limits are kept, root only, for the units
// of its commands.
func (e *Executor) limitsPath(name string) string {
	return filepath.Join(e.priv, "sandboxes", name+".limits")
}

var _ core.Sandboxer = (*Executor)(nil)

// startSandbox starts a sandbox's idle unit and limits its slice. The job
// dir and network namespace are made; on error nothing is left running.
func (e *Executor) startSandbox(ctx context.Context, spec core.ExecSpec, uid, gid int, netns string) error {
	undo := func() {
		bg := context.WithoutCancel(ctx)
		e.h.Run(bg, Cmd{Path: "systemctl", Args: []string{"stop", sandboxSlice(spec.Name)}, Timeout: time.Minute})
		e.h.Run(bg, Cmd{Path: "systemctl", Args: []string{"stop", unitName(spec.Name)}, Timeout: time.Minute})
		e.h.Run(bg, Cmd{Path: "systemctl", Args: []string{"reset-failed", unitName(spec.Name)}, Timeout: 30 * time.Second})
		e.h.Run(bg, Cmd{Path: "systemctl", Args: []string{"revert", sandboxSlice(spec.Name)}, Timeout: 30 * time.Second})
		e.stopNetns(bg, spec.Name)
		os.RemoveAll(e.jobDir(spec.Name))
		os.Remove(e.limitsPath(spec.Name))
	}
	if err := os.MkdirAll(filepath.Dir(e.limitsPath(spec.Name)), 0o700); err != nil {
		undo()
		return err
	}
	lim := strconv.FormatInt(spec.Limits.MemoryBytes, 10) + " " + strconv.FormatFloat(spec.Limits.CPUs, 'f', -1, 64) + "\n"
	if err := config.WriteAtomic(e.limitsPath(spec.Name), []byte(lim), 0o600); err != nil {
		undo()
		return fmt.Errorf("write the sandbox's limits: %v", err)
	}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemd-run", Args: e.unitArgs(spec, core.KindSandbox, uid, gid, "", netns), Timeout: time.Minute}); err != nil {
		undo()
		return fmt.Errorf("start unit %s: %v", unitName(spec.Name), err)
	}
	// The slice exists now. Its limits bound every command together, as a
	// job's unit bounds the job; without them no command may run.
	props := []string{"set-property", "--runtime", sandboxSlice(spec.Name),
		"MemoryMax=" + strconv.FormatInt(spec.Limits.MemoryBytes, 10), "MemorySwapMax=0",
		"CPUQuota=" + cpuQuota(spec.Limits.CPUs), "TasksMax=4096"}
	if _, err := e.h.Run(ctx, Cmd{Path: "systemctl", Args: props, Timeout: time.Minute}); err != nil {
		undo()
		return fmt.Errorf("limit %s: %v", sandboxSlice(spec.Name), err)
	}
	e.log.Info("sandbox started", "executor", Name, "name", spec.Name,
		"memory", spec.Limits.MemoryBytes, "cpus", spec.Limits.CPUs, "ttl", spec.MaxDuration.String())
	return nil
}

// Exec implements core.Sandboxer: one command, as a unit of its own in the
// sandbox's slice, with the job unit's user, limits, network namespace and
// mounts. `systemd-run --pipe --wait` streams its output and returns its
// exit code.
func (e *Executor) Exec(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if !isSandboxName(name) {
		return -1, fmt.Errorf("%q is not a sandbox", name)
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return -1, errors.New("no command to run")
	}
	for _, a := range append(append([]string(nil), req.Argv...), req.Dir) {
		if strings.ContainsRune(a, 0) {
			return -1, errors.New("a command argument holds a NUL byte")
		}
	}
	st, err := e.State(ctx, name)
	if err != nil {
		return -1, err
	}
	if st != core.ExecRunning {
		return -1, fmt.Errorf("sandbox %s is not running", name)
	}
	sh, ok := e.h.(Streamer)
	if !ok {
		return -1, errors.New("this host can't stream a command's output")
	}
	uid, gid, err := e.ids()
	if err != nil {
		return -1, err
	}
	// Fail closed, as Start does: no command runs unless its traffic is
	// confined.
	if err := e.ensureNet(ctx, uid); err != nil {
		return -1, fmt.Errorf("%v; run sudo tentmaker setup", err)
	}
	pid, err := e.netnsPID(ctx, netnsUnit(name))
	if err != nil {
		return -1, fmt.Errorf("sandbox %s has no network namespace: %v", name, err)
	}
	netns := filepath.Join(e.p.Proc, strconv.Itoa(pid), "ns", "net")
	limits, err := e.sliceLimits(name)
	if err != nil {
		return -1, fmt.Errorf("sandbox %s: %v", name, err)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = maxDuration(0)
	}
	unit := ExecPrefix + name + "-" + randHex(6) + ".service"
	args := e.execArgs(name, unit, req, limits, uid, gid, netns, timeout)
	code, err := sh.Stream(ctx, Cmd{Path: "systemd-run", Args: args, Timeout: timeout + 30*time.Second}, stdin, stdout, stderr)
	if err != nil || ctx.Err() != nil {
		// systemd-run ended before the command did: stop the command.
		e.h.Run(context.WithoutCancel(ctx), Cmd{Path: "systemctl", Args: []string{"stop", unit}, Timeout: time.Minute})
		if err == nil {
			err = ctx.Err()
		}
		return -1, fmt.Errorf("run in sandbox %s: %w", name, err)
	}
	return code, nil
}

// execArgs is the systemd-run argv for one command in a sandbox.
func (e *Executor) execArgs(name, unit string, req core.SandboxExec, limits core.Resources, uid, gid int, netns string, timeout time.Duration) []string {
	dir := filepath.Join(e.jobDir(name), "work")
	if req.Dir != "" {
		if filepath.IsAbs(req.Dir) {
			dir = filepath.Clean(req.Dir)
		} else {
			dir = filepath.Join(dir, req.Dir)
		}
	}
	spec := core.ExecSpec{Kind: core.KindSandbox, Name: name, Limits: limits}
	args := []string{"--quiet", "--no-ask-password", "--pipe", "--wait", "--collect",
		"--unit=" + unit, "--slice=" + sandboxSlice(name),
		"--uid=" + strconv.Itoa(uid), "--gid=" + strconv.Itoa(gid)}
	args = append(args, e.jobProps(spec, uid, netns, timeout)...)
	for _, kv := range e.jobEnv(spec, uid) {
		args = append(args, "--setenv="+kv)
	}
	return append(append(args, "--working-directory="+dir, "--"), wrapArgv(req.Argv)...)
}

// sliceLimits are the sandbox's limits for its command units. The slice
// holds the real ones; each unit gets the same, so no unit takes more than
// the whole sandbox even if the slice's drop-in were lost. A sandbox whose
// limits can't be read runs nothing.
func (e *Executor) sliceLimits(name string) (core.Resources, error) {
	b, err := os.ReadFile(e.limitsPath(name))
	if err != nil {
		return core.Resources{}, fmt.Errorf("read its limits: %v", err)
	}
	var r core.Resources
	if f := strings.Fields(string(b)); len(f) == 2 {
		r.MemoryBytes, _ = strconv.ParseInt(f[0], 10, 64)
		r.CPUs, _ = strconv.ParseFloat(f[1], 64)
	}
	if r.MemoryBytes <= 0 || !(r.CPUs > 0) {
		return core.Resources{}, errors.New("its limits file is malformed")
	}
	return r, nil
}

// wrapArgv hands argv to the unit through sh, base64-encoded. systemd
// expands $VAR in a command's arguments on some versions (older than 254)
// and not on others, so no argument a caller wrote may reach ExecStart as
// is; the wrapper holds no '$' followed by a name or '{', which every
// version leaves alone.
func wrapArgv(argv []string) []string {
	var b strings.Builder
	b.WriteString("exec")
	for _, a := range argv {
		b.WriteString(" ")
		b.WriteString(shellQuote(a))
	}
	enc := base64.StdEncoding.EncodeToString([]byte(b.String()))
	return []string{"/bin/sh", "-c", `eval "$(echo ` + enc + ` | base64 -d)"`}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}
