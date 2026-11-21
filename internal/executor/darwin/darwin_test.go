//go:build !windows

package darwin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

var secret = []byte(`S3CRET-jit-config-do-not-leak`)

type fakeProc struct {
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	code   int
	killed bool
}

func newProc() *fakeProc                  { return &fakeProc{done: make(chan struct{}), code: -1} }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) ExitCode() int         { p.mu.Lock(); defer p.mu.Unlock(); return p.code }
func (p *fakeProc) Kill() {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.exit(-1)
}
func (p *fakeProc) exit(code int) {
	p.once.Do(func() {
		p.mu.Lock()
		p.code = code
		p.mu.Unlock()
		close(p.done)
	})
}

type spawned struct {
	c    Cmd
	log  string
	proc *fakeProc
}

// fakeTart simulates the tart CLI and the host tools around it.
type fakeTart struct {
	mu        sync.Mutex
	cmds      []Cmd
	spawns    []*spawned
	vms       map[string]bool
	images    []string
	agentOK   bool
	fail      map[string]error // by tart verb, or by tool path
	stuckVM   bool             // tart delete does nothing
	vmExits   bool             // tart run exits at once
	result    string
	logText   string
	chowns    map[string][2]int
	fetches   []string
	fetchBody map[string][]byte
	respond   func(c Cmd) (Result, bool, error)
}

func newFake() *fakeTart {
	return &fakeTart{vms: map[string]bool{}, agentOK: true, fail: map[string]error{}, chowns: map[string][2]int{}, fetchBody: map[string][]byte{}}
}

func (f *fakeTart) Run(ctx context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, c)
	if f.respond != nil {
		if r, ok, err := f.respond(c); ok {
			return r, err
		}
	}
	if err := f.fail[c.Path]; err != nil {
		return Result{Code: 1}, err
	}
	if !strings.HasSuffix(c.Path, "/tart") || len(c.Args) == 0 {
		return Result{}, nil
	}
	verb := c.Args[0]
	if err := f.fail[verb]; err != nil {
		return Result{Code: 1}, err
	}
	a := c.Args
	switch verb {
	case "clone":
		f.vms[a[2]] = true
	case "delete":
		if !f.stuckVM {
			delete(f.vms, a[1])
		}
	case "list":
		var out []string
		if a[2] == "local" {
			for v := range f.vms {
				out = append(out, v)
			}
		} else {
			out = append(out, f.images...)
		}
		sort.Strings(out)
		return Result{Stdout: strings.Join(out, "\n") + "\n"}, nil
	case "--version":
		return Result{Stdout: TartVersion + "\n"}, nil
	case "exec":
		if len(a) == 3 && a[2] == "true" {
			if !f.agentOK {
				return Result{Code: 1}, errors.New("no agent")
			}
			return Result{}, nil
		}
		if len(a) > 3 && a[2] == "cat" {
			return Result{Stdout: f.result}, nil
		}
		if len(a) > 3 && a[2] == "tail" {
			return Result{Stdout: f.logText}, nil
		}
	}
	return Result{}, nil
}

func (f *fakeTart) Spawn(c Cmd, log string) (Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, c)
	if c.Timeout <= 0 {
		return nil, errors.New("spawn without a timeout")
	}
	p := newProc()
	f.spawns = append(f.spawns, &spawned{c: c, log: log, proc: p})
	if c.Args[0] == "run" && f.vmExits {
		p.exit(1)
	}
	return p, nil
}

func (f *fakeTart) Fetch(ctx context.Context, url, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, url)
	b, ok := f.fetchBody[url]
	if !ok {
		return errors.New("no network")
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	return os.WriteFile(dst, b, 0o644)
}

func (f *fakeTart) Lchown(path string, uid, gid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chowns[path] = [2]int{uid, gid}
	return nil
}

func (f *fakeTart) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.cmds {
		out = append(out, strings.TrimPrefix(c.String(), filepath.Dir(c.Path)+"/"))
	}
	return out
}

func (f *fakeTart) spawn(i int) *spawned {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns[i]
}

type tenv struct {
	e    *Executor
	f    *fakeTart
	o    Options
	dir  string
	logs *bytes.Buffer
	now  time.Time
}

func newTenv(t *testing.T) *tenv {
	dir := t.TempDir()
	f := newFake()
	v := &tenv{f: f, dir: dir, logs: &bytes.Buffer{}, now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	v.o = Options{
		StateDir: filepath.Join(dir, "state"),
		CacheDir: filepath.Join(dir, "cache"),
		LibDir:   filepath.Join(dir, "lib"),
		HostArch: "arm64",
		Log:      slog.New(slog.NewTextHandler(&lockedBuf{b: v.logs}, nil)),
		Tools:    f,
		Now:      func() time.Time { mu.Lock(); defer mu.Unlock(); return v.now },
		Sleep:    func(d time.Duration) { mu.Lock(); v.now = v.now.Add(d); mu.Unlock() },
		Binary: func(p core.Platform) (string, error) {
			b := filepath.Join(dir, "tentmaker-"+p.OS+"-"+p.Arch)
			return b, os.WriteFile(b, []byte("guest-binary-"+p.OS), 0o755)
		},
	}
	v.e = New(v.o)
	return v
}

type lockedBuf struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func spec(name, kind, os string) core.ExecSpec {
	return core.ExecSpec{
		Kind: kind, Name: name,
		Grant:       core.Grant{Secret: secret},
		Platform:    core.Platform{OS: os, Arch: "arm64"},
		Limits:      core.Resources{MemoryBytes: 8 << 30, CPUs: 3.5},
		MaxDuration: time.Hour,
		TestWorkers: 4,
	}
}

func (v *tenv) assertNoSecret(t *testing.T, errs ...error) {
	t.Helper()
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	for _, c := range v.f.cmds {
		for _, s := range append(append([]string{c.Path}, c.Args...), c.Env...) {
			if strings.Contains(s, "S3CRET") {
				t.Fatalf("secret in argv or env: %s", c)
			}
		}
	}
	if strings.Contains(v.logs.String(), "S3CRET") {
		t.Fatal("secret in logs")
	}
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), "S3CRET") {
			t.Fatalf("secret in error %v", err)
		}
	}
}

func waitState(t *testing.T, e *Executor, name string, want core.ExecState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := e.State(context.Background(), name); st == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, err := e.State(context.Background(), name)
	t.Fatalf("state %s (%v), want %s", st, err, want)
}

func TestStartRunner(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	err := v.e.Start(ctx, spec("job-mac", "", "macos"))
	must(t, err)
	got := strings.Join(v.f.lines(), "\n")
	want := strings.Join([]string{
		"tart clone " + ImageMacOS + " tentmaker-job-mac",
		"tart set --memory 8192 --cpu 4 tentmaker-job-mac",
		"tart run --no-graphics --net-softnet --net-softnet-allow=out proto udp @host port 53,out proto tcp @host port 53 --net-softnet-block=100.64.0.0/10,169.254.0.0/16,out @host tentmaker-job-mac",
		"tart exec tentmaker-job-mac true",
	}, "\n")
	if !strings.HasPrefix(got, want) {
		t.Fatalf("commands:\n%s\nwant prefix:\n%s", got, want)
	}
	ex := v.f.spawn(1)
	if ex.c.Args[0] != "exec" || ex.c.Args[1] != "-i" || ex.c.Args[2] != "tentmaker-job-mac" || ex.c.Args[3] != "/bin/bash" {
		t.Fatalf("runner exec %q", ex.c.Args)
	}
	script := ex.c.Args[5]
	for _, s := range []string{"actions-runner-osx-arm64-" + RunnerVersion + ".tar.gz", RunnerSHA256OSX + "  runner.tgz", "shasum -a 256 -c", "IFS= read -r cfg", "export TENTMAKER_JOBS=4", "exec ./run.sh"} {
		if !strings.Contains(script, s) {
			t.Errorf("script lacks %q", s)
		}
	}
	if string(ex.c.Stdin) != string(secret)+"\n" {
		t.Fatalf("stdin %q", ex.c.Stdin)
	}
	if ex.c.Timeout != time.Hour {
		t.Fatalf("runner lifetime %s", ex.c.Timeout)
	}
	if !strings.Contains(strings.Join(ex.c.Env, " "), "TART_HOME="+filepath.Join(v.dir, "state", "macos-tart", "tart")) {
		t.Fatalf("env %q", ex.c.Env)
	}
	if st, _ := v.e.State(ctx, "job-mac"); st != core.ExecRunning {
		t.Fatalf("state %s", st)
	}
	// The runner exits: the VM is deleted at once and the job reads exited.
	ex.proc.exit(0)
	waitState(t, v.e, "job-mac", core.ExecExited)
	if v.f.vms["tentmaker-job-mac"] {
		t.Fatal("VM survived the job")
	}
	if !v.f.spawn(0).proc.killed {
		t.Fatal("tart run was not ended")
	}
	must(t, v.e.Clean(ctx, "job-mac"))
	if st, _ := v.e.State(ctx, "job-mac"); st != core.ExecMissing {
		t.Fatalf("after clean: %s", st)
	}
	v.assertNoSecret(t, err)
}

func TestStartCheckLinuxGuest(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	res := core.CheckResult{Repo: "o/r", SHA: "abc", Context: "tentmaker/build", State: "success", Description: "ok"}
	b, _ := json.Marshal(res)
	v.f.result = string(b)
	v.f.logText = "built\n"
	must(t, v.e.Start(ctx, spec("chk-lin", core.KindCheck, "linux")))
	var cp Cmd
	for _, c := range v.f.cmds {
		if len(c.Args) > 4 && c.Args[0] == "exec" && c.Args[1] == "-i" && c.Args[3] == "/bin/sh" {
			cp = c
		}
	}
	if strings.Join(cp.Args, "|") != "exec|-i|tentmaker-chk-lin|/bin/sh|-c|cat > /tmp/tentmaker && chmod 755 /tmp/tentmaker" || string(cp.Stdin) != "guest-binary-linux" {
		t.Fatalf("binary copy %q stdin %q", cp.Args, cp.Stdin)
	}
	if v.f.cmds[0].Args[1] != ImageLinux {
		t.Fatalf("cloned %s", v.f.cmds[0].Args[1])
	}
	run := v.f.spawn(1)
	if strings.Join(run.c.Args, " ") != "exec -i tentmaker-chk-lin /tmp/tentmaker check-run --out /tmp/tentmaker-out" || !bytes.Equal(run.c.Stdin, secret) {
		t.Fatalf("check-run %q", run.c.Args)
	}
	run.proc.exit(1)
	waitState(t, v.e, "chk-lin", core.ExecExited)
	lines := strings.Join(v.f.lines(), "\n")
	for _, s := range []string{"tart exec tentmaker-chk-lin cat /tmp/tentmaker-out/result.json", "tart exec tentmaker-chk-lin tail -c 65536 /tmp/tentmaker-out/log.txt"} {
		if !strings.Contains(lines, s) {
			t.Errorf("missing %q", s)
		}
	}
	got, err := v.e.CheckResult(ctx, "chk-lin")
	must(t, err)
	if got.State != "success" || got.Runner != "chk-lin" || got.LogTail != "built\n" || got.Context != "tentmaker/build" {
		t.Fatalf("result %+v", got)
	}
	must(t, v.e.Clean(ctx, "chk-lin"))
	if _, err := os.Stat(filepath.Join(v.dir, "state", "macos-tart", "out", "chk-lin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("outputs survived Clean")
	}
	v.assertNoSecret(t)
}

// A check job's source comes from the worker's loopback relay, which a VM
// can't reach: the host downloads it and pipes it into the guest, and the
// job reads it from a file.
func TestCheckSourceCopiedIntoGuest(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	relay := "http://127.0.0.1:7434/v1/relay/" + strings.Repeat("ab", 32)
	tarball := []byte("\x1f\x8b the source")
	v.f.fetchBody = map[string][]byte{relay: tarball}
	var copied []byte
	var copyCmd Cmd
	v.f.respond = func(c Cmd) (Result, bool, error) {
		if c.StdinFile != "" {
			copyCmd = c
			b, err := os.ReadFile(c.StdinFile)
			if err != nil {
				return Result{Code: 1}, true, err
			}
			copied = b
			return Result{}, true, nil
		}
		return Result{}, false, nil
	}
	bundle := []byte(`{"repo":"o/r","sha":"abc","archive_url":"` + relay + `","ref":"main","job":{"name":"build","steps":[{"run":"make"}]}}`)
	sp := spec("chk-src", core.KindCheck, "linux")
	sp.Grant.Secret = bundle
	must(t, v.e.Start(ctx, sp))
	if !bytes.Equal(copied, tarball) {
		t.Fatalf("copied %q into the guest", copied)
	}
	if strings.Join(copyCmd.Args, "|") != "exec|-i|tentmaker-chk-src|sh|-c|cat > /tmp/tentmaker-src.tar.gz" || copyCmd.Timeout <= 0 {
		t.Fatalf("source copy %q (timeout %s)", copyCmd.Args, copyCmd.Timeout)
	}
	if _, err := os.Stat(copyCmd.StdinFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the host copy of the source was left behind: %v", err)
	}
	run := v.f.spawn(1)
	var got core.CheckBundle
	if err := json.Unmarshal(run.c.Stdin, &got); err != nil {
		t.Fatalf("check-run stdin: %v", err)
	}
	if got.ArchiveURL != "file:///tmp/tentmaker-src.tar.gz" || strings.Contains(string(run.c.Stdin), "/v1/relay/") {
		t.Fatalf("the job's archive URL is %q", got.ArchiveURL)
	}
	if got.Repo != "o/r" || got.SHA != "abc" || got.Ref != "main" || got.Job.Name != "build" {
		t.Fatalf("the bundle lost fields: %+v", got)
	}

	// A relay that answers nothing fails the start at the source stage.
	v = newTenv(t)
	sp = spec("chk-nosrc", core.KindCheck, "linux")
	sp.Grant.Secret = bundle
	var be *ErrBoot
	if err := v.e.Start(ctx, sp); !errors.As(err, &be) || be.Stage != "source" || strings.Contains(err.Error(), "/v1/relay/") {
		t.Fatalf("no source: %v", err)
	}
}

func TestCheckVMDiesFirst(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("chk-die", core.KindCheck, "macos")))
	v.f.spawn(0).proc.exit(1) // the VM, not the job
	waitState(t, v.e, "chk-die", core.ExecExited)
	r, _ := v.e.CheckResult(ctx, "chk-die")
	if r.State != "error" || !strings.Contains(r.Description, "no result.json") {
		t.Fatalf("%+v", r)
	}
}

func TestBootFailures(t *testing.T) {
	ctx := context.Background()

	v := newTenv(t)
	v.f.fail["clone"] = errors.New("tart clone: no space")
	err := v.e.Start(ctx, spec("job-bad", "", "macos"))
	var be *ErrBoot
	if !errors.As(err, &be) || be.Stage != "clone" || !be.Deleted {
		t.Fatalf("clone failure: %#v", err)
	}
	if st, _ := v.e.State(ctx, "job-bad"); st != core.ExecMissing {
		t.Fatalf("state after failed boot: %s", st)
	}

	v = newTenv(t)
	v.f.agentOK = false
	err = v.e.Start(ctx, spec("job-slow", "", "macos"))
	if !errors.As(err, &be) || be.Stage != "agent" || !be.Deleted {
		t.Fatalf("agent timeout: %v", err)
	}
	if len(v.f.vms) != 0 {
		t.Fatal("VM left after agent timeout")
	}
	if len(v.f.spawns) != 1 {
		t.Fatal("the runner started without an agent")
	}

	v = newTenv(t)
	v.f.vmExits = true
	if err = v.e.Start(ctx, spec("job-exit", "", "macos")); !errors.As(err, &be) || be.Stage != "agent" {
		t.Fatalf("VM exit: %v", err)
	}

	v = newTenv(t)
	v.f.fail["clone"] = errors.New("boom")
	v.f.stuckVM = true
	v.f.vms["tentmaker-job-stk"] = true
	if err = v.e.Start(ctx, spec("job-stk", "", "macos")); !errors.As(err, &be) || be.Deleted {
		t.Fatalf("undeletable VM: %v", err)
	}
	if !strings.Contains(err.Error(), "could not be deleted") {
		t.Fatal(err)
	}
	v.assertNoSecret(t, err)
}

func TestStartRefuses(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	for _, s := range []core.ExecSpec{
		spec("../x", "", "macos"),
		spec("job-win", "", "windows"),
		func() core.ExecSpec { s := spec("job-ok", "", "macos"); s.Grant.Secret = nil; return s }(),
		func() core.ExecSpec { s := spec("job-ok", "", "macos"); s.Limits = core.Resources{}; return s }(),
		spec("job-ok", "weird", "macos"),
	} {
		if err := v.e.Start(ctx, s); err == nil {
			t.Errorf("Start(%s %s) succeeded", s.Name, s.Platform)
		}
	}
	if len(v.f.cmds) != 0 {
		t.Fatalf("ran %v", v.f.lines())
	}
	must(t, v.e.Start(ctx, spec("job-one", "", "macos")))
	must(t, v.e.Start(ctx, spec("job-two", "", "linux")))
	n := len(v.f.cmds)
	if err := v.e.Start(ctx, spec("job-three", "", "macos")); err == nil {
		t.Fatal("a third VM started")
	}
	if err := v.e.Start(ctx, spec("job-one", "", "macos")); err == nil {
		t.Fatal("a duplicate started")
	}
	if len(v.f.cmds) != n {
		t.Fatal("refused starts ran commands")
	}
	if New(Options{HostArch: "amd64"}).Platforms() != nil {
		t.Fatal("Intel Mac offers platforms")
	}
	if p := v.e.Platforms(); len(p) != 2 || v.e.MaxSlots() != 2 || v.e.Name() != "macos-tart" {
		t.Fatalf("%v", p)
	}
}

func TestStopAndState(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("job-stop", "", "macos")))
	if err := v.e.Clean(ctx, "job-stop"); err == nil {
		t.Fatal("Clean ran on a running job")
	}
	must(t, v.e.Stop(ctx, "job-stop"))
	if st, _ := v.e.State(ctx, "job-stop"); st != core.ExecExited {
		t.Fatalf("after stop %s", st)
	}
	if v.f.vms["tentmaker-job-stop"] {
		t.Fatal("VM survived Stop")
	}
	must(t, v.e.Clean(ctx, "job-stop"))
	must(t, v.e.Stop(ctx, "job-never"))

	// Untracked: a VM from before a restart is unknown, not exited.
	v.f.vms["tentmaker-job-old"] = true
	if st, _ := v.e.State(ctx, "job-old"); st != core.ExecUnknown {
		t.Fatalf("untracked VM: %s", st)
	}
	if err := v.e.Clean(ctx, "job-old"); err == nil {
		t.Fatal("Clean on an unknown job")
	}
	must(t, v.e.Stop(ctx, "job-old"))
	if st, _ := v.e.State(ctx, "job-old"); st != core.ExecMissing {
		t.Fatalf("after stop: %s", st)
	}
	v.f.fail["list"] = errors.New("tart broke")
	if st, err := v.e.State(ctx, "job-old"); st != core.ExecUnknown || err == nil {
		t.Fatal("list failure must read unknown")
	}
	delete(v.f.fail, "list")
	v.f.stuckVM = true
	v.f.vms["tentmaker-job-stuck"] = true
	if err := v.e.Stop(ctx, "job-stuck"); err == nil {
		t.Fatal("Stop says done but the VM is still there")
	}
}

func TestListAndReset(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	v.f.vms["tentmaker-job-left"] = true
	v.f.vms["someone-elses-vm"] = true
	v.f.vms["tentmaker-BAD_NAME"] = true
	must(t, v.e.Start(ctx, spec("job-live", "", "macos")))
	got, err := v.e.List(ctx)
	must(t, err)
	if strings.Join(got, ",") != "job-left,job-live" {
		t.Fatalf("List %v", got)
	}
	if err := v.e.Reset(ctx); err == nil {
		t.Fatal("Reset with a running job")
	}
	v.f.spawn(1).proc.exit(0)
	waitState(t, v.e, "job-live", core.ExecExited)
	old := filepath.Join(v.dir, "state", "macos-tart", "logs", "vm-ancient.log")
	must(t, os.MkdirAll(filepath.Dir(old), 0o700))
	must(t, os.WriteFile(old, nil, 0o600))
	fresh := filepath.Join(filepath.Dir(old), "vm-recent.log")
	must(t, os.WriteFile(fresh, nil, 0o600))
	recent := v.now.Add(-time.Hour)
	must(t, os.Chtimes(fresh, recent, recent))
	past := v.now.Add(-8 * 24 * time.Hour)
	must(t, os.Chtimes(old, past, past))
	must(t, v.e.Reset(ctx))
	if v.f.vms["tentmaker-job-left"] || !v.f.vms["someone-elses-vm"] {
		t.Fatalf("Reset deleted the wrong VMs: %v", v.f.vms)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("week-old log kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a recent log was pruned")
	}
	must(t, v.e.SetPoolLimit(ctx, core.Resources{}, core.Resources{}))
}

func (v *tenv) readyHost() {
	v.f.images = []string{ImageMacOS, ImageLinux}
	v.f.respond = func(c Cmd) (Result, bool, error) {
		switch c.Path {
		case "/usr/bin/sw_vers":
			return Result{Stdout: "26.2\n"}, true, nil
		case "/usr/bin/stat":
			return Result{Stdout: "0\n"}, true, nil
		case "/bin/df":
			return Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/disk3s5 971350180 400000000 500000000 45% /System/Volumes/Data\n"}, true, nil
		}
		return Result{}, false, nil
	}
}

func TestPreflight(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	v.readyHost()
	sn := filepath.Join(v.dir, "lib", "bin", "softnet")
	must(t, os.MkdirAll(filepath.Dir(sn), 0o755))
	must(t, os.WriteFile(sn, []byte("x"), 0o755))
	must(t, os.Chmod(sn, 0o755|os.ModeSetuid))
	if ps := v.e.Preflight(ctx); len(ps) != 0 {
		t.Fatalf("ready host: %+v", ps)
	}

	v.f.images = []string{ImageLinux}
	v.f.respond = func(c Cmd) (Result, bool, error) {
		switch c.Path {
		case "/usr/bin/sw_vers":
			return Result{Stdout: "15.6\n"}, true, nil
		case "/usr/bin/codesign":
			return Result{Code: 1}, true, errors.New("invalid signature")
		case "/usr/bin/stat":
			return Result{Stdout: "501\n"}, true, nil
		case "/bin/df":
			return Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/x 1 1 1048576 1% /\n"}, true, nil
		}
		if len(c.Args) > 0 && c.Args[0] == "--version" {
			return Result{Stdout: "2.41.1\n"}, true, nil
		}
		return Result{}, false, nil
	}
	checks := map[string]string{}
	for _, p := range v.e.Preflight(ctx) {
		checks[p.Check] += p.What + "; "
	}
	for _, c := range []string{"macOS", "tart", "tart codesign", "softnet", "images", "disk"} {
		if checks[c] == "" {
			t.Errorf("missed %s: %v", c, checks)
		}
	}
	if !strings.Contains(checks["disk"], "want 120 GiB") {
		t.Errorf("disk need: %s", checks["disk"])
	}
	if ps := New(Options{HostArch: "amd64", Tools: v.f}).Preflight(ctx); len(ps) != 1 || ps[0].Check != "arch" {
		t.Fatalf("intel: %+v", ps)
	}
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestInstallAndUninstall(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	v.readyHost()
	tartTar, snTar := []byte("tart tarball"), []byte("softnet tarball")
	v.f.fetchBody[TartURL] = tartTar
	v.f.fetchBody[SoftnetURL] = snTar
	v.o.Pins = &Pins{TartSHA256: sum(tartTar), SoftnetSHA256: sum(snTar)}
	stale := []string{
		"ghcr.io/cirruslabs/macos-tahoe-xcode:26.4",
		"ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:" + strings.Repeat("a", 64),
		"ghcr.io/cirruslabs/macos-tahoe-xcode-extra:1",
		"ghcr.io/other/macos-tahoe-xcode:26.4",
	}
	v.f.images = append([]string{ImageMacOS, ImageLinux}, stale...)
	v.f.vms["tentmaker-job-left"] = true
	v.f.vms["mine"] = true
	inner := v.f.respond
	v.f.respond = func(c Cmd) (Result, bool, error) {
		if c.Path == "/usr/bin/tar" {
			dir := c.Args[3]
			if strings.Contains(c.Args[1], "softnet") {
				os.WriteFile(filepath.Join(dir, "softnet"), []byte("softnet-bin"), 0o755)
			} else {
				os.MkdirAll(filepath.Join(dir, "tart.app", "Contents", "MacOS"), 0o755)
				os.WriteFile(filepath.Join(dir, "tart.app", "Contents", "MacOS", "tart"), []byte("tart-bin"), 0o775)
			}
			return Result{}, true, nil
		}
		if len(c.Args) > 1 && c.Args[0] == "delete" {
			var keep []string
			for _, i := range v.f.images {
				if i != c.Args[1] {
					keep = append(keep, i)
				}
			}
			v.f.images = keep
		}
		return inner(c)
	}
	must(t, Install(ctx, v.o))
	lines := strings.Join(v.f.lines(), "\n")
	for _, s := range []string{
		"codesign --verify --deep --strict " + filepath.Join(v.dir, "lib", "tart-"+TartVersion+".part", "tart.app"),
		"tart delete tentmaker-job-left",
		"tart pull " + ImageLinux,
		"tart pull " + ImageMacOS,
		"tart delete ghcr.io/cirruslabs/macos-tahoe-xcode:26.4",
		"tart delete ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:" + strings.Repeat("a", 64),
	} {
		if !strings.Contains(lines, s) {
			t.Errorf("Install did not run %q", s)
		}
	}
	for _, s := range []string{"delete ghcr.io/cirruslabs/macos-tahoe-xcode-extra:1", "delete ghcr.io/other/", "delete mine"} {
		if strings.Contains(lines, s) {
			t.Errorf("Install ran %q", s)
		}
	}
	sn := filepath.Join(v.dir, "lib", "bin", "softnet")
	st, err := os.Stat(sn)
	must(t, err)
	if st.Mode()&os.ModeSetuid == 0 || st.Mode().Perm() != 0o755 || v.f.chowns[sn+".tmp"] != [2]int{0, 0} {
		t.Fatalf("softnet mode %v owner %v", st.Mode(), v.f.chowns[sn+".tmp"])
	}
	if l, _ := os.Readlink(filepath.Join(v.dir, "lib", "tart")); l != filepath.Join(v.dir, "lib", "tart-"+TartVersion) {
		t.Fatalf("tart link %q", l)
	}
	if st, _ := os.Stat(filepath.Join(v.dir, "lib", "tart", "tart.app", "Contents", "MacOS", "tart")); st.Mode().Perm() != 0o755 {
		t.Fatal("tart binary is group writable")
	}
	n, nf := len(v.f.cmds), len(v.f.fetches)
	must(t, Install(ctx, v.o))
	if strings.Contains(strings.Join(v.f.lines()[n:], "\n"), "codesign") || len(v.f.fetches) != nf {
		t.Fatal("rerun reinstalled tart")
	}

	// Uninstall stops, with Tart kept, when Tart can't list VMs.
	v.f.fail["list"] = errors.New("tart broke")
	if err := Uninstall(ctx, v.o, false); err == nil {
		t.Fatal("uninstall went on without listing VMs")
	}
	if _, err := os.Stat(filepath.Join(v.dir, "lib", "tart")); err != nil {
		t.Fatal("tart removed after a failed uninstall")
	}
	delete(v.f.fail, "list")
	v.f.vms["tentmaker-job-x"] = true
	must(t, Uninstall(ctx, v.o, true))
	if v.f.vms["tentmaker-job-x"] || !v.f.vms["mine"] {
		t.Fatal("uninstall deleted the wrong VMs")
	}
	if len(v.f.images) != 2 || v.f.images[0] != "ghcr.io/cirruslabs/macos-tahoe-xcode-extra:1" {
		t.Fatalf("images after purge: %v", v.f.images)
	}
	for _, p := range []string{"lib/tart", "lib/tart-" + TartVersion, "lib/bin/softnet", "state/macos-tart"} {
		if _, err := os.Lstat(filepath.Join(v.dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived", p)
		}
	}
	must(t, Uninstall(ctx, v.o, true))

	// A checksum mismatch stops the install and drops the download.
	v2 := newTenv(t)
	v2.readyHost()
	v2.f.fetchBody[TartURL] = []byte("tampered")
	v2.o.Pins = &Pins{TartSHA256: sum(tartTar), SoftnetSHA256: sum(snTar)}
	if err := Install(ctx, v2.o); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("tampered tart: %v", err)
	}
	v3 := newTenv(t)
	v3.readyHost()
	v3.o.Images = map[string]string{"macos/arm64": "ghcr.io/cirruslabs/macos-tahoe-xcode:latest"}
	if err := Install(ctx, v3.o); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("unpinned image: %v", err)
	}
}

// dfFree makes df report gib GiB free.
func (v *tenv) dfFree(gib int64) {
	inner := v.f.respond
	v.f.respond = func(c Cmd) (Result, bool, error) {
		if c.Path == "/bin/df" {
			return Result{Stdout: fmt.Sprintf("Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/x 1 1 %d 1%% /\n", gib<<20)}, true, nil
		}
		return inner(c)
	}
}

func (v *tenv) installable(t *testing.T) {
	t.Helper()
	tartTar, snTar := []byte("tart tarball"), []byte("softnet tarball")
	v.f.fetchBody[TartURL] = tartTar
	v.f.fetchBody[SoftnetURL] = snTar
	v.o.Pins = &Pins{TartSHA256: sum(tartTar), SoftnetSHA256: sum(snTar)}
	inner := v.f.respond
	v.f.respond = func(c Cmd) (Result, bool, error) {
		if c.Path == "/usr/bin/tar" {
			dir := c.Args[3]
			if strings.Contains(c.Args[1], "softnet") {
				os.WriteFile(filepath.Join(dir, "softnet"), []byte("softnet-bin"), 0o755)
			} else {
				os.MkdirAll(filepath.Join(dir, "tart.app", "Contents", "MacOS"), 0o755)
				os.WriteFile(filepath.Join(dir, "tart.app", "Contents", "MacOS", "tart"), []byte("tart-bin"), 0o775)
			}
			return Result{}, true, nil
		}
		if len(c.Args) > 1 && c.Args[0] == "pull" {
			v.f.images = append(v.f.images, c.Args[1])
		}
		return inner(c)
	}
}

func (v *tenv) pulls() []string {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	var out []string
	for _, c := range v.f.cmds {
		if len(c.Args) > 1 && c.Args[0] == "pull" {
			out = append(out, c.Args[1])
		}
	}
	return out
}

// A normal Mac has room for the Linux image but not the macOS one: setup
// installs Linux guests only, and doctor says how to add macOS ones.
func TestInstallSkipsMacOSWithoutRoom(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	v.readyHost()
	v.f.images = nil
	v.dfFree(60)
	v.installable(t)
	must(t, Install(ctx, v.o))
	if got := v.pulls(); len(got) != 1 || got[0] != ImageLinux {
		t.Fatalf("pulled %v, want only the Linux image", got)
	}
	e := New(v.o)
	if got := e.Platforms(); len(got) != 1 || got[0] != (core.Platform{OS: "linux", Arch: "arm64"}) {
		t.Fatalf("platforms %v", got)
	}
	for _, p := range e.Preflight(ctx) {
		if p.Check == "images" || p.Check == "disk" {
			t.Errorf("preflight: %+v", p)
		}
	}
	notes := e.Notes(ctx)
	if len(notes) != 1 || notes[0].What != "macOS guests are off: the macOS image needs 120 GiB, 60 GiB free; run sudo tentmaker setup --macos-guests when you have room" || notes[0].Fix == "" {
		t.Fatalf("notes %+v", notes)
	}
	if err := e.Start(ctx, spec("mac-job", core.KindRunner, "macos")); err == nil {
		t.Fatal("started a macOS job without the image")
	}

	// --macos-guests pulls it once the image itself fits, margin or not.
	v.o.MacOSGuests = true
	if err := Install(ctx, v.o); err == nil || !strings.Contains(err.Error(), "60 GiB free") {
		t.Fatalf("forced without room for the image: %v", err)
	}
	v.dfFree(100)
	must(t, Install(ctx, v.o))
	if got := v.pulls(); got[len(got)-1] != ImageMacOS {
		t.Fatalf("pulls %v", got)
	}
	e = New(v.o)
	if len(e.Platforms()) != 2 || len(e.Notes(ctx)) != 0 {
		t.Fatalf("platforms %v notes %v", e.Platforms(), e.Notes(ctx))
	}
	// Plenty of room: both, with no flag.
	w := newTenv(t)
	w.readyHost()
	w.f.images = nil
	w.dfFree(500)
	w.installable(t)
	must(t, Install(ctx, w.o))
	if got := w.pulls(); len(got) != 2 {
		t.Fatalf("pulled %v with room for both", got)
	}
}

// Without room even for the Linux image, Install downloads nothing and
// succeeds, so setup still installs the service; doctor names the problem.
func TestInstallWithoutRoomForAnyImage(t *testing.T) {
	v := newTenv(t)
	ctx := context.Background()
	v.readyHost()
	v.f.images = nil
	v.dfFree(30)
	v.f.fetchBody[TartURL] = []byte("tart tarball")
	must(t, Install(ctx, v.o))
	v.f.mu.Lock()
	fetches := len(v.f.fetches)
	v.f.mu.Unlock()
	if fetches != 0 || len(v.pulls()) != 0 {
		t.Fatalf("downloaded without room: fetches %d pulls %v", fetches, v.pulls())
	}
	e := New(v.o)
	if got := e.Platforms(); len(got) != 0 {
		t.Fatalf("platforms %v", got)
	}
	var found bool
	for _, p := range e.Preflight(ctx) {
		if p.Check == "images" && strings.Contains(p.What, "the Linux image needs 50 GiB, 30 GiB free") && p.Fix != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("preflight %+v", e.Preflight(ctx))
	}
	if n := e.Notes(ctx); len(n) != 1 || !strings.HasPrefix(n[0].What, "macOS guests are off: the macOS image needs 130 GiB, 30 GiB free") {
		t.Fatalf("notes %+v", n)
	}
}

func TestVMSize(t *testing.T) {
	for _, c := range []struct {
		l         core.Resources
		mib, cpus int64
	}{
		{core.Resources{MemoryBytes: 8 << 30, CPUs: 4}, 8192, 4},
		{core.Resources{MemoryBytes: 3<<30 + 1, CPUs: 0.5}, 3073, 1},
		{core.Resources{MemoryBytes: 2 << 30, CPUs: 2.0000000001}, 2048, 2},
	} {
		if m, c2 := vmSize(c.l); m != c.mib || c2 != c.cpus {
			t.Errorf("%+v: %d %d", c.l, m, c2)
		}
	}
	if !strings.Contains(runnerScript("linux", 0), "sha256sum -c --quiet -") || strings.Contains(runnerScript("linux", 0), "TENTMAKER_JOBS") {
		t.Fatal("linux runner script")
	}
	if !strings.Contains(runnerScript("linux", 0), "actions-runner-linux-arm64-"+RunnerVersion) {
		t.Fatal("linux asset")
	}
}
