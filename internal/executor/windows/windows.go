// Package windows runs each job as a dedicated local standard user inside a
// Job Object with a hard memory and CPU cap, in a fresh directory.
//
// Everything that touches Windows goes through API. The real API lives in
// api_windows.go; other platforms get a stub so the package builds and its
// tests run anywhere.
package windows

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/executor/guestbin"
)

// Name is the executor's name.
const Name = "windows-job"

// User is the local account jobs run as.
const User = "tentmaker-runner"

// MinFreeDisk is what Preflight wants free under StateDir.
const MinFreeDisk = 10 << 30

// Options configures the executor. Zero values pick the defaults.
type Options struct {
	// BaseDir is Tentmaker's own folder (%ProgramData%\Tentmaker), which
	// holds the config, state, logs and cache. Install makes it readable
	// but not writable by other users; empty leaves it alone.
	BaseDir  string
	StateDir string // default %ProgramData%\Tentmaker\state; jobs live in <StateDir>\jobs\<name>
	CacheDir string // downloads (<CacheDir>\dist); default <StateDir>\cache
	Log      *slog.Logger
	// Binary returns the tentmaker binary for a guest platform (KindCheck).
	// Install copies it into <StateDir>\lib.
	Binary func(p core.Platform) (string, error)
	Arch   string // "x64" or "arm64"; empty means runtime.GOARCH
	// SystemRoot defaults to %SystemRoot%, then C:\Windows.
	SystemRoot string
	// HostCPUs is the machine's logical CPUs for the CPU rate cap; 0 means
	// runtime.NumCPU().
	HostCPUs int
	// Getenv reads the daemon's environment for the few machine-wide
	// variables a job inherits (PATH, ProgramFiles, ...). Default os.Getenv.
	Getenv func(string) string
	API    API
	Pin    *Pin // overrides the pinned actions/runner (tests)
	// AllowLAN lets jobs reach RFC 1918 addresses ([guards] allow_lan).
	AllowLAN bool
	Sleep    func(time.Duration)
	// Phase, when set, is called as Install starts each of its phases
	// (setup times them). It must not block.
	Phase func(name string)
}

// Executor is the windows-job executor.
type Executor struct {
	o    Options
	api  API
	log  *slog.Logger
	arch string

	mu   sync.Mutex
	jobs map[string]*job

	guest guestbin.Syncer
}

// job is one tracked job. p is nil while Start is still setting it up;
// such a job counts as running, so nothing cleans it from under Start.
type job struct {
	p      Process
	timer  *time.Timer
	done   bool
	code   int
	memory int64 // the job's memory limit
	oom    bool  // the job object refused memory past it
}

// New returns an executor. It does not touch the host.
func New(o Options) *Executor {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.API == nil {
		o.API = NewOSAPI()
	}
	if o.StateDir == "" {
		pd := o.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		o.StateDir = filepath.Join(pd, "Tentmaker")
	}
	if o.CacheDir == "" {
		o.CacheDir = filepath.Join(o.StateDir, "cache")
	}
	if o.SystemRoot == "" {
		o.SystemRoot = o.Getenv("SystemRoot")
		if o.SystemRoot == "" {
			o.SystemRoot = `C:\Windows`
		}
	}
	if o.Arch == "" {
		o.Arch = goArch(runtime.GOARCH)
	}
	if o.HostCPUs <= 0 {
		o.HostCPUs = runtime.NumCPU()
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	return &Executor{o: o, api: o.API, log: o.Log, arch: o.Arch, jobs: map[string]*job{}}
}

func goArch(a string) string {
	switch a {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	}
	return a
}

func (e *Executor) pin() (Pin, bool) {
	if e.o.Pin != nil {
		return *e.o.Pin, true
	}
	return PinFor(e.arch)
}

func (e *Executor) jobsDir() string           { return filepath.Join(e.o.StateDir, "jobs") }
func (e *Executor) jobDir(name string) string { return filepath.Join(e.jobsDir(), name) }
func (e *Executor) libDir() string            { return filepath.Join(e.o.StateDir, "lib") }
func (e *Executor) runnerRoot() string        { return filepath.Join(e.o.StateDir, "runner") }
func (e *Executor) secretDir() string         { return filepath.Join(e.o.StateDir, "secret") }

// toolsDir is where check jobs' setup actions find toolchains
// (<state>\tools\<tool>-<version>\): Administrators write, the runner reads.
func (e *Executor) toolsDir() string     { return filepath.Join(e.o.StateDir, "tools") }
func (e *Executor) passwordPath() string { return filepath.Join(e.secretDir(), "runner.pw") }
func (e *Executor) distDir() string      { return filepath.Join(e.o.CacheDir, "dist") }
func (e *Executor) script() string       { return filepath.Join(e.libDir(), "job-runner.ps1") }
func (e *Executor) guestBinary() string {
	return filepath.Join(e.libDir(), "tentmaker-windows-"+e.arch+".exe")
}
func (e *Executor) template() string {
	p, _ := e.pin()
	return filepath.Join(e.runnerRoot(), p.Dir())
}
func (e *Executor) powershell() string {
	return filepath.Join(e.o.SystemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

// adminACL is SYSTEM and Administrators only.
func adminACL() []ACE {
	return []ACE{{SIDSystem, AccessFull, true}, {SIDAdmins, AccessFull, true}}
}

// baseACL is for Tentmaker's own folder and its state folder: everyone may
// read them, and only SYSTEM and Administrators may add or change a file.
// Without it they inherit ProgramData's ACL, under which any user, a job
// too, may add files to their subfolders.
func baseACL() []ACE {
	return append(adminACL(), ACE{SIDUsers, AccessReadAndExecute, true})
}

// jobACL lets the runner user change everything under its job directory.
// OWNER RIGHTS keeps files the user creates from granting themselves
// WRITE_DAC, so the job can't open its tree to other accounts.
func jobACL(userSID string) []ACE {
	return append(adminACL(), ACE{userSID, AccessModify, true}, ACE{SIDOwnerRights, AccessModify, true})
}

// libACL lets the runner user read and run the job script and guest binary.
func libACL(userSID string) []ACE {
	return append(adminACL(), ACE{userSID, AccessReadAndExecute, true})
}

// Name implements core.Executor.
func (e *Executor) Name() string { return Name }

// Platforms implements core.Executor.
func (e *Executor) Platforms() []core.Platform {
	if _, ok := PinFor(e.arch); !ok {
		return nil
	}
	return []core.Platform{{OS: "windows", Arch: e.arch}}
}

// MaxSlots implements core.Executor. The budget alone limits slots.
func (e *Executor) MaxSlots() int { return 0 }

func checkName(name string) error {
	if !core.NamePattern.MatchString(name) {
		return fmt.Errorf("invalid runner name %q", trunc(name, 70))
	}
	return nil
}

// cpuRate is the Job Object hard cap: the job's share of the whole machine
// in 1/100 of a percent.
func cpuRate(cpus float64, hostCPUs int) uint32 {
	if hostCPUs <= 0 {
		hostCPUs = 1
	}
	r := math.Ceil(cpus / float64(hostCPUs) * 10000)
	if r < 1 {
		r = 1
	}
	if r > 10000 {
		r = 10000
	}
	return uint32(r)
}

func maxDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 3 * time.Hour
	}
	return d
}

// Start implements core.Executor.
func (e *Executor) Start(ctx context.Context, spec core.ExecSpec) error {
	if err := checkName(spec.Name); err != nil {
		return err
	}
	kind := spec.Kind
	if kind == "" {
		kind = core.KindRunner
	}
	if kind != core.KindRunner && kind != core.KindCheck {
		return fmt.Errorf("unknown job kind %q", trunc(kind, 20))
	}
	if spec.Platform.OS != "windows" || spec.Platform.Arch != e.arch {
		return fmt.Errorf("%s runs windows/%s, not %s", Name, e.arch, spec.Platform)
	}
	if len(spec.Grant.Secret) == 0 {
		return errors.New("the grant has no secret")
	}
	if spec.Limits.MemoryBytes <= 0 || spec.Limits.CPUs <= 0 {
		return errors.New("a job needs a memory and CPU limit")
	}
	if kind == core.KindCheck {
		if err := e.syncGuestBinary(); err != nil {
			return err
		}
	}
	dir := e.jobDir(spec.Name)
	e.mu.Lock()
	_, tracked := e.jobs[spec.Name]
	_, statErr := os.Lstat(dir)
	if tracked || statErr == nil {
		e.mu.Unlock()
		return fmt.Errorf("job %s already exists", spec.Name)
	}
	j := &job{code: -1, memory: spec.Limits.MemoryBytes}
	e.jobs[spec.Name] = j // reserve the name; the copy below takes a while
	e.mu.Unlock()
	started := false
	defer func() {
		if !started {
			e.mu.Lock()
			delete(e.jobs, spec.Name)
			e.mu.Unlock()
		}
	}()
	sid, err := e.api.UserSID(User)
	if err != nil {
		return fmt.Errorf("look up %s (run install): %w", User, err)
	}
	// Fail closed: no firewall rules for this user, no job.
	if err := e.ensureNet(ctx, sid); err != nil {
		return fmt.Errorf("the network rules for CI jobs are not in force: %v; run tentmaker setup as an administrator", err)
	}
	pw, err := e.password()
	if err != nil {
		return err
	}
	defer clear(pw)
	if err := e.prepareJob(dir, kind, sid); err != nil {
		os.RemoveAll(dir)
		return err
	}
	ps := ProcSpec{
		User:        User,
		UserSID:     sid,
		Password:    pw,
		Env:         e.jobEnv(spec),
		Stdin:       spec.Grant.Secret,
		LogPath:     filepath.Join(dir, "job.log"),
		MemoryBytes: spec.Limits.MemoryBytes,
		CPURate:     cpuRate(spec.Limits.CPUs, e.o.HostCPUs),
	}
	if kind == core.KindCheck {
		ps.App = e.guestBinary()
		ps.Args = []string{"check-run", "--out", filepath.Join(dir, "out")}
		ps.Dir = filepath.Join(dir, "work")
	} else {
		ps.App = e.powershell()
		ps.Args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", e.script(), "-Runner", filepath.Join(dir, "runner")}
		ps.Dir = filepath.Join(dir, "runner")
	}
	p, err := e.api.StartProcess(ps)
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("start job %s: %s", spec.Name, redact(redact(err.Error(), spec.Grant.Secret), pw))
	}
	life := maxDuration(spec.MaxDuration)
	e.mu.Lock()
	j.p = p
	j.timer = time.AfterFunc(life, func() {
		e.log.Warn("job hit its time limit", "executor", Name, "name", spec.Name, "limit", life.String())
		p.Terminate()
	})
	started = true
	e.mu.Unlock()
	go e.wait(spec.Name, j)
	e.log.Info("job started", "executor", Name, "name", spec.Name, "kind", kind,
		"memory", spec.Limits.MemoryBytes, "cpus", spec.Limits.CPUs, "cpu_rate", ps.CPURate)
	return nil
}

func (e *Executor) wait(name string, j *job) {
	e.mu.Lock()
	p, timer := j.p, j.timer
	e.mu.Unlock()
	<-p.Done()
	timer.Stop()
	e.mu.Lock()
	j.done, j.code, j.oom = true, p.ExitCode(), p.MemoryLimitHit()
	e.mu.Unlock()
	e.log.Info("job exited", "executor", Name, "name", name, "code", j.code)
}

func (e *Executor) password() ([]byte, error) {
	blob, err := os.ReadFile(e.passwordPath())
	if err != nil {
		return nil, fmt.Errorf("read the runner password (run install): %w", err)
	}
	pw, err := e.api.Unprotect(blob)
	if err != nil || len(pw) == 0 {
		return nil, errors.New("can't unprotect the runner password; run install to reset it")
	}
	return pw, nil
}

// prepareJob builds <jobs>\<name>: a protected ACL for the runner user
// first, so everything copied in after inherits it.
func (e *Executor) prepareJob(dir, kind, sid string) error {
	if _, err := os.Stat(e.jobsDir()); err != nil {
		return fmt.Errorf("jobs directory missing (run install): %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := e.api.SetDACL(dir, jobACL(sid)); err != nil {
		return fmt.Errorf("set the job's ACL: %w", err)
	}
	subs := []string{"home", "home/AppData/Roaming", "home/AppData/Local", "tmp", "toolcache", "work"}
	if kind == core.KindCheck {
		subs = append(subs, "out")
	}
	for _, s := range subs {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(s)), 0o700); err != nil {
			return err
		}
	}
	if kind == core.KindRunner {
		if err := copyTree(e.template(), filepath.Join(dir, "runner")); err != nil {
			return fmt.Errorf("copy the runner (run install): %w", err)
		}
	}
	return nil
}

// copyTree copies the trusted runner template. Links are refused rather
// than followed.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o700)
		case d.Type().IsRegular():
			return copyFile(p, to)
		}
		return fmt.Errorf("%s is not a regular file", p)
	})
}

// syncGuestBinary refreshes the check jobs' tentmaker binary when it
// differs from the daemon's (install copied it; an update made it stale).
func (e *Executor) syncGuestBinary() error {
	if e.o.Binary == nil {
		return errors.New("no tentmaker binary for check jobs (Options.Binary is nil)")
	}
	src, err := e.o.Binary(core.Platform{OS: "windows", Arch: e.arch})
	if err != nil {
		return fmt.Errorf("find the tentmaker binary for windows/%s: %w", e.arch, err)
	}
	copied, err := e.guest.Sync(src, e.guestBinary(), replaceExe)
	if copied {
		e.log.Info("refreshed the tentmaker binary for check jobs", "executor", Name, "path", e.guestBinary())
	}
	return err
}

// replaceExe puts a copy of src at dst. Windows can't replace an exe that
// a running check still uses, but it can rename it, so the old one moves
// aside first and is deleted when nothing runs it any more. The copy
// inherits the lib folder's ACL (SYSTEM and Administrators write; the
// runner user reads and runs).
func replaceExe(src, dst string) error {
	if olds, _ := filepath.Glob(dst + ".old-*"); len(olds) > 0 {
		for _, o := range olds {
			os.Remove(o) // still running: try again next time
		}
	}
	tmp := dst + ".new"
	os.Remove(tmp)
	if err := copyFile(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	old := fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())
	moved := os.Rename(dst, old) == nil
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		if moved {
			os.Rename(old, dst)
		}
		return err
	}
	if moved {
		os.Remove(old)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// passEnv is the daemon environment a job inherits: machine-wide paths, no
// credentials.
var passEnv = []string{
	"PATH", "PATHEXT", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
	"CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432", "PSModulePath",
	"PROCESSOR_ARCHITECTURE", "PROCESSOR_IDENTIFIER", "COMPUTERNAME", "OS",
}

func (e *Executor) jobEnv(spec core.ExecSpec) []string {
	dir := e.jobDir(spec.Name)
	home := filepath.Join(dir, "home")
	sys := e.o.SystemRoot
	drive := filepath.VolumeName(sys)
	if drive == "" {
		drive = "C:"
	}
	env := []string{}
	for _, k := range passEnv {
		if v := e.o.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if e.o.Getenv("PATH") == "" {
		env = append(env, "PATH="+strings.Join([]string{
			filepath.Join(sys, "System32"), sys, filepath.Join(sys, "System32", "Wbem"),
			filepath.Join(sys, "System32", "WindowsPowerShell", "v1.0"),
		}, ";"))
	}
	env = append(env,
		"SystemRoot="+sys,
		"windir="+sys,
		"SystemDrive="+drive,
		"ComSpec="+filepath.Join(sys, "System32", "cmd.exe"),
		"USERNAME="+User,
		"USERPROFILE="+home,
		"HOME="+home,
		"APPDATA="+filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		"TEMP="+filepath.Join(dir, "tmp"),
		"TMP="+filepath.Join(dir, "tmp"),
		"RUNNER_TOOL_CACHE="+filepath.Join(dir, "toolcache"),
		"AGENT_TOOLSDIRECTORY="+filepath.Join(dir, "toolcache"),
		"NUMBER_OF_PROCESSORS="+strconv.Itoa(int(math.Ceil(spec.Limits.CPUs))),
	)
	if spec.TestWorkers > 0 {
		env = append(env, "TENTMAKER_JOBS="+strconv.Itoa(spec.TestWorkers))
	}
	if spec.Kind == core.KindCheck {
		env = append(env, "TENTMAKER_TOOLS="+e.toolsDir())
	}
	return env
}

// State implements core.Executor. A job this process didn't start can't be
// running: its Job Object kills everything when the daemon's handle closes.
func (e *Executor) State(ctx context.Context, name string) (core.ExecState, error) {
	if err := checkName(name); err != nil {
		return core.ExecUnknown, err
	}
	e.mu.Lock()
	j, ok := e.jobs[name]
	running := ok && !j.done
	e.mu.Unlock()
	if ok {
		if running {
			return core.ExecRunning, nil
		}
		return core.ExecExited, nil
	}
	if _, err := os.Lstat(e.jobDir(name)); err == nil {
		return core.ExecExited, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return core.ExecMissing, nil
	} else {
		return core.ExecUnknown, err
	}
}

// Stop implements core.Executor.
func (e *Executor) Stop(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	e.mu.Lock()
	j, ok := e.jobs[name]
	var p Process
	if ok {
		p = j.p
	}
	e.mu.Unlock()
	if !ok {
		return nil
	}
	if p == nil {
		return fmt.Errorf("job %s is still starting; stop it again in a moment", name)
	}
	if err := p.Terminate(); err != nil {
		e.log.Warn("terminate job", "executor", Name, "name", name, "err", err.Error())
	}
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	select {
	case <-p.Done():
		return nil
	case <-t.C:
		return fmt.Errorf("job %s did not stop within 30s", name)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Clean implements core.Executor.
func (e *Executor) Clean(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	e.mu.Lock()
	j, ok := e.jobs[name]
	if ok && !j.done {
		e.mu.Unlock()
		return fmt.Errorf("job %s is still running", name)
	}
	e.mu.Unlock()
	if err := e.removeAll(e.jobDir(name)); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.jobs, name)
	e.mu.Unlock()
	return nil
}

// removeAll retries: Windows keeps a file busy for a moment after the last
// process holding it exits.
func (e *Executor) removeAll(dir string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		e.o.Sleep(time.Second)
	}
	return fmt.Errorf("remove %s: %w", dir, err)
}

// List implements core.Executor.
func (e *Executor) List(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	ents, err := os.ReadDir(e.jobsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, d := range ents {
		if core.NamePattern.MatchString(d.Name()) {
			seen[d.Name()] = true
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

// Reset implements core.Executor. It ends anything the runner user left
// behind outside a Job Object (a process started through WMI or a
// scheduled task) and deletes every job directory.
func (e *Executor) Reset(ctx context.Context) error {
	e.mu.Lock()
	for n, j := range e.jobs {
		if !j.done {
			e.mu.Unlock()
			return fmt.Errorf("job %s is still running", n)
		}
	}
	e.mu.Unlock()
	var errs []error
	sid, err := e.api.UserSID(User)
	switch {
	case err == nil:
		if n, err := e.api.DeleteUserTasks(User, sid); err != nil {
			errs = append(errs, fmt.Errorf("delete the runner user's scheduled tasks: %w", err))
		} else if n > 0 {
			e.log.Warn("deleted scheduled tasks a job left", "executor", Name, "count", n)
		}
		if n, err := e.api.KillUserProcesses(sid); err != nil {
			errs = append(errs, fmt.Errorf("end the runner user's processes: %w", err))
		} else if n > 0 {
			e.log.Warn("ended processes a job left outside its job object", "executor", Name, "count", n)
		}
	case !errors.Is(err, ErrNoUser):
		errs = append(errs, err)
	}
	ents, err := os.ReadDir(e.jobsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, d := range ents {
		if err := e.removeAll(filepath.Join(e.jobsDir(), d.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	e.mu.Lock()
	for n, j := range e.jobs {
		if j.done {
			delete(e.jobs, n)
		}
	}
	e.mu.Unlock()
	return errors.Join(errs...)
}

// SetPoolLimit implements core.Executor. Each job has its own Job Object
// cap; there is no pool-wide one on Windows yet.
func (e *Executor) SetPoolLimit(ctx context.Context, limit, inUse core.Resources) error { return nil }

// CheckResult implements core.Executor.
func (e *Executor) CheckResult(ctx context.Context, name string) (core.CheckResult, error) {
	if err := checkName(name); err != nil {
		return core.CheckResult{}, err
	}
	e.mu.Lock()
	j, ok := e.jobs[name]
	running := ok && !j.done
	oom, memory := ok && j.oom, int64(0)
	if ok {
		memory = j.memory
	}
	e.mu.Unlock()
	if running {
		return core.CheckResult{}, fmt.Errorf("job %s is still running", name)
	}
	res := readCheckResult(e.jobDir(name), "out", name)
	// The job object's limit makes an allocation fail rather than killing
	// the job, so check-run reports an ordinary step failure; say why. A
	// job that got by without that memory and passed stays passed.
	if oom && res.State != "success" {
		why := fmt.Sprintf("ran out of memory (the job's limit is %.1f GiB)", float64(memory)/(1<<30))
		if res.Description != "" {
			why += "; " + res.Description
		}
		res.State, res.Description = "failure", trunc(why, 1000)
	}
	return res, nil
}

func redact(s string, secret []byte) string {
	if len(secret) == 0 {
		return s
	}
	return strings.ReplaceAll(s, string(secret), "[redacted]")
}
