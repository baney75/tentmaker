// Package darwin is the macos-tart executor: every job runs in its own
// throwaway Tart VM (Apple's Virtualization framework), cloned from an image
// pinned by digest, on a Softnet network that reaches only the internet, and
// is stopped and deleted when the job ends, whatever happened.
//
// The VM is the boundary: no host user, file or credential enters it. The
// single-use secret reaches the guest on the stdin of one `tart exec -i`
// process (the Tart guest agent, a host-owned vsock channel; no ssh and no
// password), never argv, the environment or a host file.
package darwin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// Name is the executor's name.
const Name = "macos-tart"

// MaxVMs is Apple's limit on concurrent macOS guests.
const MaxVMs = 2

// Guest paths for checks mode.
const (
	guestBinary = "/tmp/tentmaker"
	guestOut    = "/tmp/tentmaker-out"
)

// ErrBoot is a VM that failed to come up (clone, set, run, the guest agent,
// or the copy of the guest binary). The VM is already deleted unless
// Deleted is false. Dispatch backs off on it.
type ErrBoot struct {
	Stage   string // "clone", "set", "run", "agent", "copy", "exec"
	Err     error
	Deleted bool
}

func (e *ErrBoot) Error() string {
	s := "VM boot failed at " + e.Stage + ": " + e.Err.Error()
	if !e.Deleted {
		s += " (and the VM could not be deleted)"
	}
	return s
}
func (e *ErrBoot) Unwrap() error { return e.Err }

// Options configure the executor.
type Options struct {
	StateDir string // <StateDir>/macos-tart holds outputs, logs and (by default) Tart's storage
	CacheDir string // downloads
	Log      *slog.Logger
	// Binary returns the tentmaker binary for a guest platform (KindCheck).
	Binary func(p core.Platform) (string, error)
	// LibDir holds Tart and Softnet (root-owned). Default /usr/local/lib/tentmaker.
	LibDir string
	// TartHome is Tart's storage (TART_HOME). Default <StateDir>/macos-tart/tart.
	TartHome string
	// Prefix starts every job VM's name. Default "tentmaker-".
	Prefix string
	// Images maps "macos/arm64" and "linux/arm64" to digest-pinned images.
	// Nil means both pinned defaults.
	Images map[string]string
	// AgentTimeout bounds the wait for the guest agent. Default 5 minutes.
	AgentTimeout time.Duration
	// HostArch is runtime.GOARCH unless set (tests).
	HostArch string
	// Pins overrides the Tart and Softnet checksums (tests only).
	Pins  *Pins
	Tools Tools
	Now   func() time.Time
	Sleep func(time.Duration)
}

// Pins are the checksums Install verifies downloads against.
type Pins struct{ TartSHA256, SoftnetSHA256 string }

// DefaultImages are the pinned guest images.
func DefaultImages() map[string]string {
	return map[string]string{"macos/arm64": ImageMacOS, "linux/arm64": ImageLinux}
}

// Executor is the macos-tart executor.
type Executor struct {
	o    Options
	t    Tools
	log  *slog.Logger
	base string
	mu   sync.Mutex
	jobs map[string]*job
}

type job struct {
	name, vm, kind string
	vmProc         Proc
	execProc       Proc
	destroyMu      sync.Mutex
	finished       chan struct{} // closed once the watcher has copied results and deleted the VM
	ended          bool          // guarded by Executor.mu
}

// New returns the executor. It touches nothing until a method is called.
func New(o Options) *Executor {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Tools == nil {
		o.Tools = OSTools{}
	}
	if o.StateDir == "" {
		o.StateDir = "/Library/Application Support/Tentmaker"
	}
	if o.CacheDir == "" {
		o.CacheDir = "/Library/Caches/Tentmaker"
	}
	if o.LibDir == "" {
		o.LibDir = "/usr/local/lib/tentmaker"
	}
	base := filepath.Join(o.StateDir, "macos-tart")
	if o.TartHome == "" {
		o.TartHome = filepath.Join(base, "tart")
	}
	if o.Prefix == "" {
		o.Prefix = "tentmaker-"
	}
	if o.Images == nil {
		o.Images = DefaultImages()
	}
	if o.AgentTimeout <= 0 {
		o.AgentTimeout = 5 * time.Minute
	}
	if o.HostArch == "" {
		o.HostArch = runtime.GOARCH
	}
	if o.Pins == nil {
		o.Pins = &Pins{TartSHA256: TartSHA256, SoftnetSHA256: SoftnetSHA256}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	return &Executor{o: o, t: o.Tools, log: o.Log, base: base, jobs: map[string]*job{}}
}

// Name implements core.Executor.
func (e *Executor) Name() string { return Name }

// Platforms implements core.Executor: what has a pinned image, on Apple silicon.
func (e *Executor) Platforms() []core.Platform {
	if e.o.HostArch != "arm64" {
		return nil
	}
	var out []core.Platform
	for _, p := range []core.Platform{{OS: "macos", Arch: "arm64"}, {OS: "linux", Arch: "arm64"}} {
		if e.o.Images[p.String()] != "" {
			out = append(out, p)
		}
	}
	return out
}

// MaxSlots implements core.Executor.
func (e *Executor) MaxSlots() int { return MaxVMs }

func (e *Executor) tartPath() string {
	return filepath.Join(e.o.LibDir, "tart", "tart.app", "Contents", "MacOS", "tart")
}
func (e *Executor) outRoot() string { return filepath.Join(e.base, "out") }
func (e *Executor) logDir() string  { return filepath.Join(e.base, "logs") }

func (e *Executor) env() []string {
	return []string{
		"PATH=" + filepath.Join(e.o.LibDir, "bin") + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + e.base,
		"TART_HOME=" + e.o.TartHome,
		"LANG=en_US.UTF-8",
	}
}

func (e *Executor) tart(timeout time.Duration, args ...string) Cmd {
	return Cmd{Path: e.tartPath(), Args: args, Env: e.env(), Timeout: timeout}
}

func (e *Executor) vmName(name string) (string, error) {
	if !core.NamePattern.MatchString(name) {
		return "", fmt.Errorf("bad runner name %q", trunc(name, 80))
	}
	return e.o.Prefix + name, nil
}

// Start implements core.Executor. It boots the VM and starts the job, and
// returns once the job process runs. A boot failure returns *ErrBoot.
func (e *Executor) Start(ctx context.Context, spec core.ExecSpec) error {
	vm, err := e.vmName(spec.Name)
	if err != nil {
		return err
	}
	kind := spec.Kind
	if kind == "" {
		kind = core.KindRunner
	}
	if kind != core.KindRunner && kind != core.KindCheck {
		return fmt.Errorf("unknown job kind %q", trunc(kind, 20))
	}
	image := e.o.Images[spec.Platform.String()]
	if image == "" || e.o.HostArch != "arm64" {
		return fmt.Errorf("%s does not run %s here", Name, spec.Platform)
	}
	if len(spec.Grant.Secret) == 0 {
		return errors.New("the grant has no secret")
	}
	if spec.Limits.MemoryBytes <= 0 || spec.Limits.CPUs <= 0 {
		return errors.New("a job needs a memory and CPU limit")
	}
	var bin []byte
	if kind == core.KindCheck {
		if e.o.Binary == nil {
			return errors.New("no tentmaker binary for checks (Options.Binary is nil)")
		}
		path, err := e.o.Binary(spec.Platform)
		if err != nil {
			return fmt.Errorf("tentmaker binary for %s: %w", spec.Platform, err)
		}
		if bin, err = os.ReadFile(path); err != nil {
			return err
		}
	}
	e.mu.Lock()
	if _, ok := e.jobs[spec.Name]; ok {
		e.mu.Unlock()
		return fmt.Errorf("job %s already exists", spec.Name)
	}
	if n := e.liveLocked(); n >= MaxVMs {
		e.mu.Unlock()
		return fmt.Errorf("%d VMs run already; Apple allows %d", n, MaxVMs)
	}
	j := &job{name: spec.Name, vm: vm, kind: kind, finished: make(chan struct{})}
	e.jobs[spec.Name] = j
	e.mu.Unlock()

	fail := func(stage string, err error) error {
		deleted := e.destroy(context.WithoutCancel(ctx), j) == nil
		e.mu.Lock()
		delete(e.jobs, spec.Name)
		e.mu.Unlock()
		close(j.finished)
		err = errors.New(redact(err.Error(), spec.Grant.Secret))
		e.log.Warn("job failed to start", "executor", Name, "name", spec.Name, "stage", stage, "error", err.Error(), "vm_deleted", deleted)
		return &ErrBoot{Stage: stage, Err: err, Deleted: deleted}
	}
	life := maxDuration(spec.MaxDuration)
	if stage, err := e.boot(ctx, j, image, spec.Limits, life); err != nil {
		return fail(stage, err)
	}
	var c Cmd
	if kind == core.KindCheck {
		out := filepath.Join(e.outRoot(), spec.Name)
		os.RemoveAll(out)
		if err := os.MkdirAll(out, 0o700); err != nil {
			return fail("copy", err)
		}
		cp := e.tart(5*time.Minute, "exec", "-i", vm, "/bin/sh", "-c", "cat > "+guestBinary+" && chmod 755 "+guestBinary)
		cp.Stdin = bin
		if _, err := e.t.Run(ctx, cp); err != nil {
			return fail("copy", fmt.Errorf("copy the tentmaker binary into the VM: %v", err))
		}
		c = e.tart(life, "exec", "-i", vm, guestBinary, "check-run", "--out", guestOut)
		c.Stdin = append([]byte(nil), spec.Grant.Secret...)
	} else {
		c = e.tart(life, "exec", "-i", vm, "/bin/bash", "-c", runnerScript(spec.Platform.OS, spec.TestWorkers))
		c.Stdin = append(append([]byte(nil), spec.Grant.Secret...), '\n')
	}
	p, err := e.t.Spawn(c, filepath.Join(e.logDir(), "job-"+vm+".log"))
	if err != nil {
		return fail("exec", err)
	}
	e.mu.Lock()
	j.execProc = p
	e.mu.Unlock()
	go e.watch(j)
	e.log.Info("job started", "executor", Name, "name", spec.Name, "kind", kind, "vm", vm, "platform", spec.Platform.String())
	return nil
}

func (e *Executor) liveLocked() int {
	n := 0
	for _, j := range e.jobs {
		if !j.ended {
			n++
		}
	}
	return n
}

func maxDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 3 * time.Hour
	}
	return d
}

// vmSize turns limits into `tart set` arguments: whole MiB and whole CPUs.
func vmSize(l core.Resources) (mib, cpus int64) {
	mib = int64(math.Ceil(float64(l.MemoryBytes) / (1 << 20)))
	cpus = int64(math.Ceil(l.CPUs - 1e-9))
	if cpus < 1 {
		cpus = 1
	}
	return mib, cpus
}

// boot clones the image, sizes the clone, runs it headless on Softnet and
// waits for the guest agent. It returns the failing stage.
func (e *Executor) boot(ctx context.Context, j *job, image string, l core.Resources, life time.Duration) (string, error) {
	if _, err := e.t.Run(ctx, e.tart(time.Hour, "clone", image, j.vm)); err != nil {
		return "clone", err
	}
	mib, cpus := vmSize(l)
	if _, err := e.t.Run(ctx, e.tart(time.Minute, "set", "--memory", strconv.FormatInt(mib, 10), "--cpu", strconv.FormatInt(cpus, 10), j.vm)); err != nil {
		return "set", err
	}
	run := e.tart(life+e.o.AgentTimeout+10*time.Minute, "run", "--no-graphics", "--net-softnet",
		"--net-softnet-allow="+SoftnetAllow, "--net-softnet-block="+SoftnetBlock, j.vm)
	p, err := e.t.Spawn(run, filepath.Join(e.logDir(), "vm-"+j.vm+".log"))
	if err != nil {
		return "run", err
	}
	e.mu.Lock()
	j.vmProc = p
	e.mu.Unlock()
	deadline := e.o.Now().Add(e.o.AgentTimeout)
	for {
		select {
		case <-p.Done():
			return "agent", errors.New("the VM exited before its guest agent answered")
		default:
		}
		if err := ctx.Err(); err != nil {
			return "agent", err
		}
		if _, err := e.t.Run(ctx, e.tart(30*time.Second, "exec", j.vm, "true")); err == nil {
			return "", nil
		}
		if e.o.Now().After(deadline) {
			return "agent", fmt.Errorf("the guest agent did not answer within %s", e.o.AgentTimeout)
		}
		e.o.Sleep(2 * time.Second)
	}
}

// watch waits for the job (or its VM) to end, copies a check job's result
// out, and deletes the VM so it frees its memory at once.
func (e *Executor) watch(j *job) {
	e.mu.Lock()
	execP, vmP := j.execProc, j.vmProc
	e.mu.Unlock()
	execDone := false
	select {
	case <-execP.Done():
		execDone = true
	case <-vmP.Done():
	}
	ctx := context.Background()
	if j.kind == core.KindCheck && execDone {
		e.copyOut(ctx, j)
	}
	if err := e.destroy(ctx, j); err != nil {
		e.log.Error("VM not deleted", "executor", Name, "name", j.name, "vm", j.vm, "error", err.Error())
	}
	e.mu.Lock()
	j.ended = true
	e.mu.Unlock()
	close(j.finished)
	e.log.Info("job ended", "executor", Name, "name", j.name, "exit", execP.ExitCode())
}

// copyOut copies result.json and the log's tail from the guest to the host.
func (e *Executor) copyOut(ctx context.Context, j *job) {
	dir := filepath.Join(e.outRoot(), j.name)
	files := []struct {
		name string
		args []string
		max  int
	}{
		{"result.json", []string{"exec", j.vm, "cat", guestOut + "/result.json"}, MaxOutput},
		{"log.txt", []string{"exec", j.vm, "tail", "-c", strconv.Itoa(LogTailBytes), guestOut + "/log.txt"}, LogTailBytes},
	}
	for _, f := range files {
		c := e.tart(2*time.Minute, f.args...)
		c.MaxOutput = f.max
		res, err := e.t.Run(ctx, c)
		if err != nil {
			e.log.Warn("copy from VM failed", "executor", Name, "name", j.name, "file", f.name, "error", err.Error())
			continue
		}
		if err := config.WriteAtomic(filepath.Join(dir, f.name), []byte(res.Stdout), 0o600); err != nil {
			e.log.Warn("write job output failed", "executor", Name, "name", j.name, "error", err.Error())
		}
	}
}

// destroy kills the job's processes, then stops and deletes its VM and
// checks it is gone. Safe to call more than once.
func (e *Executor) destroy(ctx context.Context, j *job) error {
	j.destroyMu.Lock()
	defer j.destroyMu.Unlock()
	e.mu.Lock()
	procs := []Proc{j.execProc, j.vmProc}
	e.mu.Unlock()
	for _, p := range procs {
		if p != nil {
			p.Kill()
		}
	}
	return e.deleteVM(ctx, j.vm)
}

func (e *Executor) deleteVM(ctx context.Context, vm string) error {
	var last error
	for i := 0; i < 3; i++ {
		e.t.Run(ctx, e.tart(2*time.Minute, "stop", "--timeout", "10", vm))
		e.t.Run(ctx, e.tart(2*time.Minute, "delete", vm))
		vms, err := e.localVMs(ctx)
		if err == nil && !contains(vms, vm) {
			return nil
		}
		last = err
		e.o.Sleep(time.Second)
	}
	if last != nil {
		return fmt.Errorf("VM %s: can't confirm it is deleted: %v", vm, last)
	}
	return fmt.Errorf("VM %s is still there after tart delete", vm)
}

func (e *Executor) localVMs(ctx context.Context) ([]string, error) {
	res, err := e.t.Run(ctx, e.tart(time.Minute, "list", "--source", "local", "--quiet"))
	if err != nil {
		return nil, fmt.Errorf("tart list: %v", err)
	}
	var out []string
	for _, l := range strings.Split(res.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// State implements core.Executor. A VM this process didn't start (after a
// restart) is unknown, so nothing cuts a job it can't see.
func (e *Executor) State(ctx context.Context, name string) (core.ExecState, error) {
	vm, err := e.vmName(name)
	if err != nil {
		return core.ExecUnknown, err
	}
	e.mu.Lock()
	j, ok := e.jobs[name]
	ended := ok && j.ended
	e.mu.Unlock()
	if ok {
		if ended {
			return core.ExecExited, nil
		}
		return core.ExecRunning, nil
	}
	vms, err := e.localVMs(ctx)
	if err != nil {
		return core.ExecUnknown, err
	}
	if contains(vms, vm) {
		return core.ExecUnknown, nil
	}
	if _, err := os.Lstat(filepath.Join(e.outRoot(), name)); err == nil {
		return core.ExecExited, nil
	}
	return core.ExecMissing, nil
}

// Stop implements core.Executor: the VM is stopped and deleted.
func (e *Executor) Stop(ctx context.Context, name string) error {
	vm, err := e.vmName(name)
	if err != nil {
		return err
	}
	e.mu.Lock()
	j, ok := e.jobs[name]
	e.mu.Unlock()
	if !ok {
		if err := e.deleteVM(ctx, vm); err != nil {
			return err
		}
		return nil
	}
	if err := e.destroy(ctx, j); err != nil {
		return err
	}
	select {
	case <-j.finished:
	case <-time.After(30 * time.Second):
		return fmt.Errorf("job %s did not finish after its VM was deleted", name)
	case <-ctx.Done():
		return ctx.Err()
	}
	e.log.Info("job stopped", "executor", Name, "name", name)
	return nil
}

// Clean implements core.Executor: the VM (if any) and the job's outputs go.
func (e *Executor) Clean(ctx context.Context, name string) error {
	st, err := e.State(ctx, name)
	if err != nil {
		return err
	}
	if st == core.ExecRunning || st == core.ExecUnknown {
		return fmt.Errorf("job %s may still run; stop it first", name)
	}
	if err := e.Stop(ctx, name); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.jobs, name)
	e.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(e.outRoot(), name)); err != nil {
		return err
	}
	return nil
}

// List implements core.Executor: tracked jobs and every prefixed VM.
func (e *Executor) List(ctx context.Context) ([]string, error) {
	vms, err := e.localVMs(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, v := range vms {
		if n, ok := strings.CutPrefix(v, e.o.Prefix); ok && core.NamePattern.MatchString(n) {
			seen[n] = true
		}
	}
	e.mu.Lock()
	for n := range e.jobs {
		seen[n] = true
	}
	e.mu.Unlock()
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Reset implements core.Executor: with nothing running, delete every
// prefixed VM (tracked or left over), every job output, and logs older than
// a week. The pinned images are never touched.
func (e *Executor) Reset(ctx context.Context) error {
	e.mu.Lock()
	for n, j := range e.jobs {
		if !j.ended {
			e.mu.Unlock()
			return fmt.Errorf("reset refused: job %s runs", n)
		}
	}
	e.jobs = map[string]*job{}
	e.mu.Unlock()
	vms, err := e.localVMs(ctx)
	if err != nil {
		return err
	}
	var problems []string
	for _, v := range vms {
		if strings.HasPrefix(v, e.o.Prefix) {
			if err := e.deleteVM(ctx, v); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	if err := os.RemoveAll(e.outRoot()); err != nil {
		problems = append(problems, err.Error())
	}
	e.pruneLogs(7 * 24 * time.Hour)
	if len(problems) > 0 {
		return errors.New("reset: " + strings.Join(problems, "; "))
	}
	return nil
}

func (e *Executor) pruneLogs(age time.Duration) {
	entries, err := os.ReadDir(e.logDir())
	if err != nil {
		return
	}
	cutoff := e.o.Now().Add(-age)
	for _, ent := range entries {
		if info, err := ent.Info(); err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(e.logDir(), ent.Name()))
		}
	}
}

// SetPoolLimit implements core.Executor. Each VM's memory and CPUs are fixed
// at clone time; there is no pool-wide knob, so this is a no-op.
func (e *Executor) SetPoolLimit(ctx context.Context, limit, inUse core.Resources) error { return nil }

// CheckResult implements core.Executor.
func (e *Executor) CheckResult(ctx context.Context, name string) (core.CheckResult, error) {
	if _, err := e.vmName(name); err != nil {
		return core.CheckResult{}, err
	}
	return readCheckResult(e.outRoot(), name, name), nil
}

// runnerScript runs inside the VM through the guest agent. The JIT config
// arrives on stdin, one line. The runner is downloaded and checked in the
// guest, so nothing from the host is copied in.
func runnerScript(guestOS string, workers int) string {
	pin := runnerPin(guestOS)
	check := "sha256sum -c --quiet -"
	if guestOS == "macos" {
		check = "shasum -a 256 -c --quiet -"
	}
	w := ""
	if workers > 0 {
		w = "export TENTMAKER_JOBS=" + strconv.Itoa(workers) + "\n"
	}
	return `set -euo pipefail
IFS= read -r cfg
work=$(mktemp -d /tmp/tentmaker-runner.XXXXXX)
cd "$work"
curl -fsSL --retry 3 -o runner.tgz "` + pin.URL() + `"
echo "` + pin.SHA256 + `  runner.tgz" | ` + check + `
tar xzf runner.tgz && rm runner.tgz
` + w + `[ "$(id -u)" != 0 ] || export RUNNER_ALLOW_RUNASROOT=1
export ACTIONS_RUNNER_INPUT_JITCONFIG="$cfg"
unset cfg
exec ./run.sh
`
}

func redact(s string, secret []byte) string {
	if len(secret) == 0 {
		return s
	}
	return strings.ReplaceAll(s, string(secret), "[redacted]")
}
