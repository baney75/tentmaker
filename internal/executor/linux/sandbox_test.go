package linux

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// streamFn, when set, answers fakeHost.Stream for the sandbox tests;
// they don't run in parallel.
var streamFn func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error)

const sbx = "sbx-0123456789ab"

func sandboxSpec() core.ExecSpec {
	return core.ExecSpec{Kind: core.KindSandbox, Name: sbx, Platform: core.Platform{OS: "linux", Arch: "x64"},
		Limits: core.Resources{MemoryBytes: 2 << 30, CPUs: 1.5}, MaxDuration: 30 * time.Minute, TestWorkers: 1}
}

// running answers systemctl show for the sandbox's unit as active.
func running(c Cmd) (Result, error) {
	if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "show" && slices.Contains(c.Args, unitName(sbx)) {
		return Result{Stdout: "ActiveState=active\nLoadState=loaded\n"}, nil
	}
	return Result{}, nil
}

func TestStartSandbox(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	c := findUnit(t, v.f, unitName(sbx))
	args := strings.Join(c.Args, "\n")
	for _, want := range []string{"--slice=tentmaker-" + sbx + ".slice", "RuntimeMaxSec=1800", "StandardInput=null",
		"MemoryMax=2147483648", "CPUQuota=150%", "NetworkNamespacePath=" + filepath.Join(v.dir, "proc", "4242", "ns", "net"),
		"--working-directory=" + filepath.Join(v.e.jobDir(sbx), "work")} {
		if !strings.Contains(args, want) {
			t.Errorf("holder unit lacks %s:\n%s", want, args)
		}
	}
	if tail := c.Args[len(c.Args)-3:]; strings.Join(tail, " ") != "-- sleep infinity" {
		t.Fatalf("holder runs %q", tail)
	}
	set := findCmd(t, v.f, "systemctl")
	for _, cmd := range v.f.cmds {
		if cmd.Path == "systemctl" && len(cmd.Args) > 0 && cmd.Args[0] == "set-property" {
			set = cmd
		}
	}
	if got := strings.Join(set.Args, " "); got != "set-property --runtime tentmaker-"+sbx+".slice MemoryMax=2147483648 MemorySwapMax=0 CPUQuota=150% TasksMax=4096" {
		t.Fatalf("slice limits: %s", got)
	}
	lim := v.e.limitsPath(sbx)
	b, err := os.ReadFile(lim)
	if err != nil || string(b) != "2147483648 1.5\n" {
		t.Fatalf("limits file %q %v", b, err)
	}
	if st, _ := os.Stat(lim); st.Mode().Perm() != 0o600 {
		t.Fatalf("limits file mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(v.e.jobDir(sbx), "runner")); err == nil {
		t.Fatal("a sandbox got a runner copy")
	}

	// Clean undoes the slice's drop-in and the limits file.
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "show" {
			return Result{Stdout: "ActiveState=inactive\nLoadState=not-found\n"}, nil
		}
		return Result{}, nil
	}
	if err := v.e.Clean(context.Background(), sbx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(v.f.commands(), "systemctl revert tentmaker-"+sbx+".slice") {
		t.Fatalf("clean did not revert the slice: %v", v.f.commands())
	}
	if _, err := os.Stat(lim); err == nil {
		t.Fatal("clean left the limits file")
	}
}

func TestStartSandboxRefusesAndUndoes(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	s := sandboxSpec()
	s.Name = "job-abc"
	if err := v.e.Start(context.Background(), s); err == nil {
		t.Fatal("a sandbox without the sbx- prefix started")
	}
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "set-property" {
			return Result{Code: 1}, errors.New("Unit tentmaker-sbx.slice not found")
		}
		return Result{}, nil
	}
	if err := v.e.Start(context.Background(), sandboxSpec()); err == nil {
		t.Fatal("a sandbox with no slice limits started")
	}
	cmds := v.f.commands()
	for _, want := range []string{"systemctl stop tentmaker-" + sbx + ".slice", "systemctl stop " + unitName(sbx)} {
		if !slices.Contains(cmds, want) {
			t.Errorf("undo did not run %q: %v", want, cmds)
		}
	}
	if _, err := os.Stat(v.e.jobDir(sbx)); err == nil {
		t.Fatal("undo left the job dir")
	}
	if _, err := os.Stat(v.e.limitsPath(sbx)); err == nil {
		t.Fatal("undo left the limits file")
	}
}

func TestSandboxExecArgs(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	v.f.respond = running
	var stdinGot string
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		b, _ := io.ReadAll(stdin)
		stdinGot = string(b)
		io.WriteString(stdout, "out")
		return 42, nil
	}
	defer func() { streamFn = nil }()
	argv := []string{"echo", "$HOME", "it's", "${PATH}", "a b"}
	var out bytes.Buffer
	code, err := v.e.Exec(context.Background(), sbx, core.SandboxExec{Argv: argv, Dir: "src", Timeout: time.Minute},
		strings.NewReader("in"), &out, io.Discard)
	if err != nil || code != 42 || out.String() != "out" || stdinGot != "in" {
		t.Fatalf("code %d err %v out %q stdin %q", code, err, out.String(), stdinGot)
	}
	var c Cmd
	for _, x := range v.f.cmds {
		if x.Path == "systemd-run" && slices.Contains(x.Args, "--pipe") {
			c = x
		}
	}
	if c.Timeout != 90*time.Second {
		t.Fatalf("stream timeout %v", c.Timeout)
	}
	args := strings.Join(c.Args, "\n")
	for _, want := range []string{"--pipe", "--wait", "--collect", "--slice=tentmaker-" + sbx + ".slice",
		"--uid=1234", "--gid=1235", "MemoryMax=2147483648", "CPUQuota=150%", "RuntimeMaxSec=60",
		"NetworkNamespacePath=" + filepath.Join(v.dir, "proc", "4242", "ns", "net"),
		"--working-directory=" + filepath.Join(v.e.jobDir(sbx), "work", "src")} {
		if !strings.Contains(args, want) {
			t.Errorf("exec unit lacks %s", want)
		}
	}
	unit := ""
	for _, a := range c.Args {
		if u, ok := strings.CutPrefix(a, "--unit="); ok {
			unit = u
		}
	}
	if !strings.HasPrefix(unit, ExecPrefix+sbx+"-") || !strings.HasSuffix(unit, ".service") {
		t.Fatalf("exec unit %q", unit)
	}
	// No argument of the caller's reaches systemd as written: systemd
	// expands $VAR in ExecStart on some versions.
	for _, a := range c.Args {
		if strings.Contains(a, "$HOME") || strings.Contains(a, "${PATH}") {
			t.Fatalf("a caller argument reached systemd-run: %q", a)
		}
	}
	tail := c.Args[len(c.Args)-3:]
	if tail[0] != "/bin/sh" || tail[1] != "-c" {
		t.Fatalf("wrapper %q", tail)
	}
	enc := strings.TrimSuffix(strings.TrimPrefix(tail[2], `eval "$(echo `), ` | base64 -d)"`)
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("wrapper %q: %v", tail[2], err)
	}
	if string(dec) != `exec 'echo' '$HOME' 'it'\''s' '${PATH}' 'a b'` {
		t.Fatalf("decoded command %q", dec)
	}
}

func TestSandboxExecFailsClosed(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	streams := 0
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		streams++
		return 0, nil
	}
	defer func() { streamFn = nil }()
	req := core.SandboxExec{Argv: []string{"true"}}

	// Not running.
	if _, err := v.e.Exec(context.Background(), sbx, req, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("exec in a sandbox that is not running")
	}
	v.f.respond = running
	// Limits gone or malformed.
	os.WriteFile(v.e.limitsPath(sbx), []byte("garbage"), 0o600)
	if _, err := v.e.Exec(context.Background(), sbx, req, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed limits: %v", err)
	}
	os.Remove(v.e.limitsPath(sbx))
	if _, err := v.e.Exec(context.Background(), sbx, req, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("exec without limits")
	}
	// The firewall rules are gone.
	os.WriteFile(v.e.limitsPath(sbx), []byte("2147483648 1.5\n"), 0o600)
	v.f.mu.Lock()
	v.f.nft = ""
	v.f.mu.Unlock()
	if _, err := v.e.Exec(context.Background(), sbx, req, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("exec with no network rules")
	}
	// Not a sandbox, or a NUL byte.
	if _, err := v.e.Exec(context.Background(), "job-abc", req, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("exec in a CI job")
	}
	if _, err := v.e.Exec(context.Background(), sbx, core.SandboxExec{Argv: []string{"a\x00b"}}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("a NUL byte in argv")
	}
	if streams != 0 {
		t.Fatalf("%d commands ran", streams)
	}
}

func TestSandboxExecStopsItsUnitWhenCut(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	v.f.respond = running
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		return -1, errors.New("systemd-run: signal: killed")
	}
	defer func() { streamFn = nil }()
	if _, err := v.e.Exec(context.Background(), sbx, core.SandboxExec{Argv: []string{"sleep", "1000"}}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("no error")
	}
	stopped := false
	for _, c := range v.f.commands() {
		if strings.HasPrefix(c, "systemctl stop "+ExecPrefix+sbx+"-") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("the command's unit was not stopped: %v", v.f.commands())
	}
}
