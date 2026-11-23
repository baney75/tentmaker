//go:build !windows

package linux

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func (f *fakeHost) Stream(ctx context.Context, c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if fn := streamFn; fn != nil { // a sandbox test's
		f.mu.Lock()
		f.cmds = append(f.cmds, c)
		f.mu.Unlock()
		return fn(c, stdin, stdout, stderr)
	}
	in, _ := io.ReadAll(stdin)
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	f.stdins = append(f.stdins, string(in))
	fn := f.stream
	f.mu.Unlock()
	if fn != nil {
		return fn(c, in, stdout)
	}
	return 0, nil
}

func debugSpec() core.DebugSpec {
	return core.DebugSpec{Limits: core.Resources{MemoryBytes: 2 << 30, CPUs: 1.5}, CPUCeiling: 4, Timeout: time.Minute, MaxOutput: 32}
}

// lastSystemdRun is the newest systemd-run the fake host saw.
func lastSystemdRun(t *testing.T, f *fakeHost) Cmd {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.cmds) - 1; i >= 0; i-- {
		if f.cmds[i].Path == "systemd-run" {
			return f.cmds[i]
		}
	}
	t.Fatal("no systemd-run")
	return Cmd{}
}

// A failed job kept for debugging: its test reports are read, its runner
// credentials go, and a debug command runs in its own sandbox (uid,
// limits, namespace) with the command on stdin, never in argv.
func TestKeepAndDebugExec(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("job-one", "")))
	dir := v.e.jobDir("job-one")
	work := filepath.Join(dir, "runner", "_work", "app")
	must(t, os.MkdirAll(work, 0o755))
	must(t, os.WriteFile(filepath.Join(work, "report.xml"), []byte(`<testsuite name="app"><testcase classname="app" name="TestParse"><failure message="want 2"/></testcase></testsuite>`), 0o644))
	outside := filepath.Join(v.dir, "outside")
	must(t, os.WriteFile(outside, []byte("keep me"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "runner", ".credentials"), []byte("creds"), 0o600))
	os.Remove(filepath.Join(dir, "runner", ".runner"))
	must(t, os.Symlink(outside, filepath.Join(dir, "runner", ".runner")))

	// A running job is not kept or debugged.
	v.f.respond = showResponder("active", "loaded", false)
	if err := v.e.Keep(ctx, "job-one"); err == nil {
		t.Fatal("kept a running job")
	}
	if _, err := v.e.DebugExec(ctx, "job-one", "true", debugSpec()); err == nil {
		t.Fatal("debugged a running job")
	}

	v.f.respond = showResponder("failed", "loaded", false)
	tr, err := v.e.TestResults(ctx, "job-one")
	must(t, err)
	if len(tr.Cases) != 1 || tr.Cases[0].Name != "TestParse" || tr.Cases[0].Status != "failed" {
		t.Fatalf("tests = %+v", tr)
	}
	must(t, v.e.Keep(ctx, "job-one"))
	for _, f := range []string{".credentials", ".runner"} {
		if _, err := os.Lstat(filepath.Join(dir, "runner", f)); !os.IsNotExist(err) {
			t.Fatalf("runner %s survived Keep", f)
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep me" {
		t.Fatal("Keep followed a symlink out of the job folder")
	}

	v.f.stream = func(c Cmd, in []byte, out io.Writer) (int, error) {
		io.WriteString(out, strings.Repeat("x", 100)+"\nghp_"+strings.Repeat("C", 36)+"\nlast line\n")
		return 3, nil
	}
	res, err := v.e.DebugExec(ctx, "job-one", "echo hunter2-in-a-command", debugSpec())
	must(t, err)
	if res.ExitCode != 3 || !res.Truncated || string(res.Output) != "last line\n" {
		t.Fatalf("result = %+v %q", res, res.Output)
	}
	c := lastSystemdRun(t, v.f)
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--wait", "--pipe", "--slice=" + Slice,
		"--uid=" + strconv.Itoa(testUID), "--gid=" + strconv.Itoa(testGID),
		"MemoryMax=" + strconv.Itoa(2<<30), "CPUQuota=400%", "CPUWeight=100", "RuntimeMaxSec=60", "NoNewPrivileges=yes",
		"NetworkNamespacePath=" + filepath.Join(v.o.Paths.Proc, strconv.Itoa(testNetnsPID), "ns", "net"),
		"--working-directory=" + filepath.Join(dir, "runner", "_work"),
		"--unit=" + DebugPrefix + "job-one-",
		"-- /bin/sh -s",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("debug unit lacks %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "hunter2") || strings.Contains(strings.Join(c.Env, " "), "hunter2") {
		t.Fatal("the debug command is in argv or env")
	}
	if len(v.f.stdins) != 1 || !strings.Contains(v.f.stdins[0], "echo hunter2-in-a-command") {
		t.Fatalf("stdin = %q", v.f.stdins)
	}
	cmds := strings.Join(v.f.commands(), "\n")
	if !strings.Contains(cmds, "systemctl stop "+DebugPrefix+"job-one-") {
		t.Fatalf("the debug unit was not stopped:\n%s", cmds)
	}

	// The shell gets a pty and a shell, and the same sandbox.
	v.f.stream = func(c Cmd, in []byte, out io.Writer) (int, error) {
		out.Write(in)
		return 0, nil
	}
	var out strings.Builder
	must(t, v.e.DebugShell(ctx, "job-one", debugSpec(), strings.NewReader("ls\n"), &out))
	c = lastSystemdRun(t, v.f)
	args = strings.Join(c.Args, " ")
	if !strings.Contains(args, "--pty") || strings.Contains(args, "--pipe") || !strings.Contains(args, "NetworkNamespacePath=") || out.String() != "ls\n" {
		t.Fatalf("shell args %s out %q", args, out.String())
	}

	// Clean stops every debug unit before it takes the namespace away.
	must(t, v.e.Clean(ctx, "job-one"))
	cmds = strings.Join(v.f.commands(), "\n")
	dbg := strings.Index(cmds, "systemctl stop "+DebugPrefix+"job-one-*.service")
	ns := strings.LastIndex(cmds, "systemctl stop "+NetnsPrefix+"job-one.service")
	if dbg < 0 || ns < 0 || dbg > ns {
		t.Fatalf("clean order wrong (debug %d, netns %d):\n%s", dbg, ns, cmds)
	}
	assertNoSecret(t, v.f, v.logs)
}

// A debug command needs limits and a timeout, and a bad name is refused.
func TestDebugRefuses(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	if _, err := v.e.DebugExec(ctx, "../x", "true", debugSpec()); err == nil {
		t.Fatal("bad name accepted")
	}
	must(t, v.e.Start(ctx, spec("job-two", "")))
	v.f.respond = showResponder("failed", "loaded", false)
	if _, err := v.e.DebugExec(ctx, "job-two", "true", core.DebugSpec{Timeout: time.Minute}); err == nil {
		t.Fatal("a debug command without limits ran")
	}
	if err := v.e.DebugShell(ctx, "job-two", core.DebugSpec{Limits: debugSpec().Limits}, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("a shell without a timeout ran")
	}
}

// A shell that exits while its stdin (the debug connection) is still open
// ends at once and cleanly, not after WaitDelay with an error.
func TestStreamEndsWithOpenStdin(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	r, w := io.Pipe()
	defer w.Close()
	start := time.Now()
	code, err := OSHost{}.Stream(context.Background(), Cmd{Path: "/bin/sh", Args: []string{"-c", "exit 0"}, Timeout: time.Minute}, r, io.Discard, io.Discard)
	if err != nil || code != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("code %d err %v after %s", code, err, time.Since(start))
	}
}
