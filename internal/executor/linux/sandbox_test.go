//go:build !windows

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
	"sync"
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

// holderExited answers systemctl for a sandbox whose holder ended while a
// command unit still runs in its slice. The command stops with the slice
// only when stops is true.
func holderExited(stops bool) (respond func(Cmd) (Result, error), live func() bool) {
	cmd := ExecPrefix + sbx + "-0a0b0c0d0e0f.service"
	running := true
	respond = func(c Cmd) (Result, error) {
		if c.Path != "systemctl" || len(c.Args) == 0 {
			return Result{}, nil
		}
		switch c.Args[0] {
		case "show":
			return Result{Stdout: "ActiveState=inactive\nLoadState=loaded\n"}, nil
		case "stop":
			if stops && slices.Contains(c.Args, sandboxSlice(sbx)) {
				running = false
			}
		case "list-units":
			if slices.Contains(c.Args, "--type=service,slice") && running {
				return Result{Stdout: cmd + " loaded active running Command\n" +
					sandboxSlice(sbx) + " loaded active active Slice\n"}, nil
			}
		}
		return Result{}, nil
	}
	return respond, func() bool { return running }
}

// The holder can end before its commands (its RuntimeMaxSec, a kill from
// inside the sandbox). Clean then stops the whole slice and keeps the
// limits, the namespace and the folder until nothing in it runs.
func TestSandboxCleanStopsCommandsThatOutliveTheHolder(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	respond, live := holderExited(false)
	v.f.respond = respond
	if err := v.e.Clean(context.Background(), sbx); err == nil || !strings.Contains(err.Error(), "still runs") {
		t.Fatalf("clean with a live command: %v", err)
	}
	if !live() {
		t.Fatal("the fake lost its command")
	}
	cmds := v.f.commands()
	if !slices.Contains(cmds, "systemctl stop "+sandboxSlice(sbx)) {
		t.Fatalf("clean did not stop the slice: %v", cmds)
	}
	if slices.Contains(cmds, "systemctl revert "+sandboxSlice(sbx)) {
		t.Fatal("clean reverted the slice's limits under a live command")
	}
	if _, err := os.Stat(v.e.limitsPath(sbx)); err != nil {
		t.Fatalf("clean removed the limits file under a live command: %v", err)
	}
	if _, err := os.Stat(v.e.jobDir(sbx)); err != nil {
		t.Fatalf("clean removed the job folder under a live command: %v", err)
	}
	// Stop fails the same way: the sandbox has not stopped.
	if err := v.e.Stop(context.Background(), sbx); err == nil {
		t.Fatal("stop with a live command")
	}

	// Once the slice's stop ends the command, Clean goes on.
	respond, live = holderExited(true)
	v.f.respond = respond
	v.f.mu.Lock()
	v.f.cmds = nil
	v.f.mu.Unlock()
	if err := v.e.Clean(context.Background(), sbx); err != nil {
		t.Fatal(err)
	}
	if live() {
		t.Fatal("the command still runs")
	}
	cmds = v.f.commands()
	stop, revert := slices.Index(cmds, "systemctl stop "+sandboxSlice(sbx)), slices.Index(cmds, "systemctl revert "+sandboxSlice(sbx))
	if stop < 0 || revert < 0 || stop > revert {
		t.Fatalf("clean must stop the slice, then revert it: %v", cmds)
	}
	if _, err := os.Stat(v.e.limitsPath(sbx)); err == nil {
		t.Fatal("clean left the limits file")
	}
}

// Reset runs only when no job runs, so a sandbox's command or slice left
// then is stopped, and a reset that can't stop it is refused.
func TestResetStopsSandboxLeftovers(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	respond, _ := holderExited(false)
	v.f.respond = respond
	err := v.e.Reset(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reset refused") {
		t.Fatalf("reset with a live sandbox command: %v", err)
	}
	if !slices.Contains(v.f.commands(), "systemctl stop "+ExecPrefix+sbx+"-0a0b0c0d0e0f.service") {
		t.Fatalf("reset did not stop the command: %v", v.f.commands())
	}
	respond, live := holderExited(true)
	v.f.respond = respond
	if err := v.e.Reset(context.Background()); err != nil && strings.Contains(err.Error(), "refused") {
		t.Fatalf("reset refused once the leftovers stopped: %v", err)
	}
	if live() {
		t.Fatal("reset left the sandbox's command running")
	}
	if !slices.Contains(v.f.commands(), "systemctl revert "+sandboxSlice(sbx)) {
		t.Fatalf("reset did not revert the slice: %v", v.f.commands())
	}
}

// A dash in a slice's name nests it, so a sandbox's name is exactly sbx-
// and 12 hex digits: no sandbox's slice can hold another's.
func TestSandboxNameIsExact(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	v.f.respond = running
	for _, name := range []string{"sbx-aaa-bbb", "sbx-0123456789ab-c", "sbx-0123456789AB", "sbx-0123456789a"} {
		s := sandboxSpec()
		s.Name = name
		if err := v.e.Start(context.Background(), s); err == nil {
			t.Errorf("Start took %q", name)
		}
		if _, err := v.e.Exec(context.Background(), name, core.SandboxExec{Argv: []string{"true"}}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "not a sandbox") {
			t.Errorf("Exec in %q: %v", name, err)
		}
	}
	for _, c := range v.f.cmds {
		if c.Path == "systemd-run" {
			t.Fatalf("a unit started: %v", c.Args)
		}
	}
}

// Dir is relative to the sandbox's work folder and stays inside it.
func TestSandboxExecDirStaysInTheWorkFolder(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	v.f.respond = running
	streams := 0
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		streams++
		return 0, nil
	}
	defer func() { streamFn = nil }()
	for _, dir := range []string{"/etc", "../../..", "src/../../.."} {
		if _, err := v.e.Exec(context.Background(), sbx, core.SandboxExec{Argv: []string{"true"}, Dir: dir}, nil, io.Discard, io.Discard); err == nil {
			t.Errorf("exec in %q", dir)
		}
	}
	if streams != 0 {
		t.Fatalf("%d commands ran", streams)
	}
}

// systemd ends a command when the sandbox stops under it or its time runs
// out, and systemd-run then returns an exit code the command never chose.
// Exec reports that as an error, not an exit.
func TestSandboxExecCutOffIsAnError(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	defer func() { streamFn = nil }()
	req := core.SandboxExec{Argv: []string{"sleep", "1000"}, Timeout: time.Minute}

	// The sandbox is stopped (removed, or its TTL ran out) while the
	// command runs.
	v.f.respond = running
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		v.f.respond = func(c Cmd) (Result, error) {
			if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "show" {
				return Result{Stdout: "ActiveState=inactive\nLoadState=loaded\n"}, nil
			}
			return Result{}, nil
		}
		return 143, nil
	}
	if code, err := v.e.Exec(context.Background(), sbx, req, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("a command cut off by a stop returned %d, %v", code, err)
	}

	// Its own time runs out.
	v.f.respond = running
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	v.e.o.Now = func() time.Time { return now }
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		now = now.Add(time.Minute)
		return 143, nil
	}
	if code, err := v.e.Exec(context.Background(), sbx, req, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "ran out of time") {
		t.Fatalf("a command cut off at its timeout returned %d, %v", code, err)
	}

	// A command that ends on its own, in time, in a running sandbox,
	// returns its exit code.
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		now = now.Add(59 * time.Second)
		return 3, nil
	}
	if code, err := v.e.Exec(context.Background(), sbx, req, nil, io.Discard, io.Discard); err != nil || code != 3 {
		t.Fatalf("a command's own exit returned %d, %v", code, err)
	}
}

// A command whose launch passed Exec's checks before a Stop, and whose unit
// starts only after Stop found the slice empty, must not outlive the Stop:
// Stop waits for it and stops the slice again (review sol-F1). A command
// asked for once Stop has begun is refused.
func TestSandboxStopWaitsForALaunchInFlight(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), sandboxSpec()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	stopped, live := false, false
	emptyChecked := make(chan struct{}) // Stop's first empty-slice check
	killed := make(chan struct{})       // a slice stop after the unit started
	var once, killOnce sync.Once
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && len(c.Args) > 0 {
			mu.Lock()
			defer mu.Unlock()
			switch c.Args[0] {
			case "show":
				if slices.Contains(c.Args, unitName(sbx)) {
					if stopped {
						return Result{Stdout: "ActiveState=inactive\nLoadState=loaded\n"}, nil
					}
					return Result{Stdout: "ActiveState=active\nLoadState=loaded\n"}, nil
				}
			case "stop":
				if slices.Contains(c.Args, sandboxSlice(sbx)) {
					stopped = true
					if live {
						live = false
						killOnce.Do(func() { close(killed) })
					}
				}
			case "list-units":
				if slices.Contains(c.Args, "--type=service,slice") {
					if live {
						return Result{Stdout: ExecPrefix + sbx + "-0a0b0c0d0e0f.service loaded active running Command\n"}, nil
					}
					if stopped {
						once.Do(func() { close(emptyChecked) })
					}
					return Result{}, nil
				}
			}
		}
		return running(c)
	}
	launch := make(chan struct{})
	streamFn = func(c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		// Paused between the checks and the unit's start until Stop has
		// found the slice empty; then the unit starts and runs until the
		// slice is stopped.
		close(launch)
		<-emptyChecked
		mu.Lock()
		live = true
		mu.Unlock()
		select {
		case <-killed:
			return -1, errors.New("systemd-run: unit stopped")
		case <-time.After(10 * time.Second):
			return 0, nil
		}
	}
	defer func() { streamFn = nil }()
	execErr := make(chan error, 1)
	go func() {
		_, err := v.e.Exec(context.Background(), sbx, core.SandboxExec{Argv: []string{"sleep", "1000"}}, nil, io.Discard, io.Discard)
		execErr <- err
	}()
	<-launch
	if err := v.e.Stop(context.Background(), sbx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	stillLive := live
	mu.Unlock()
	if stillLive {
		t.Fatal("Stop returned while a command it raced still runs in the slice")
	}
	select {
	case err := <-execErr:
		if err == nil {
			t.Fatal("a command cut off by Stop reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not return after Stop")
	}
	if _, err := v.e.Exec(context.Background(), sbx, core.SandboxExec{Argv: []string{"true"}}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("an Exec after Stop: %v", err)
	}
}
