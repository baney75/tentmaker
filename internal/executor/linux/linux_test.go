//go:build !windows

package linux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

const testUID, testGID = 1234, 1235

var secret = []byte(`{"jit":"S3CRET-jit-config-do-not-leak"}`)

type fakeHost struct {
	mu      sync.Mutex
	cmds    []Cmd
	respond func(c Cmd) (Result, error)
	noUser  bool
	chowns  map[string][2]int
	fetches []string
	fetch   func(url, dst string) error
	// The firewall the fake keeps: the loaded nft script, the loaded
	// iptables script, and the units systemctl enabled.
	nft     string
	ipt     string
	enabled map[string]bool
	nftFail bool // nft -f fails (a broken nft, a bad rules file)
}

func (f *fakeHost) Run(ctx context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	respond := f.respond
	res, handled, err := f.firewallLocked(c)
	f.mu.Unlock()
	if handled {
		return res, err
	}
	if respond != nil {
		return respond(c)
	}
	return Result{Code: 0}, nil
}

// firewallLocked plays nft, iptables, ip6tables and systemctl enable for
// the network rules.
func (f *fakeHost) firewallLocked(c Cmd) (Result, bool, error) {
	a := strings.Join(c.Args, " ")
	fail := func(msg string) (Result, bool, error) { return Result{Code: 1, Stderr: msg}, true, errors.New(msg) }
	switch base := filepath.Base(c.Path); {
	case base == "nft" && len(c.Args) == 2 && c.Args[0] == "-f":
		if f.nftFail {
			return fail("Error: Could not process rule")
		}
		b, err := os.ReadFile(c.Args[1])
		if err != nil {
			return fail(err.Error())
		}
		f.nft = string(b)
		return Result{}, true, nil
	case base == "nft" && a == "list table inet tentmaker":
		if f.nft == "" {
			return fail("Error: No such file or directory")
		}
		return Result{Stdout: f.nft}, true, nil
	case base == "nft" && a == "list tables":
		if f.nft == "" {
			return Result{Stdout: "table inet filter\n"}, true, nil
		}
		return Result{Stdout: "table inet filter\ntable inet tentmaker\n"}, true, nil
	case base == "nft" && a == "delete table inet tentmaker":
		f.nft = ""
		return Result{}, true, nil
	case c.Path == "/bin/sh" && len(c.Args) == 1 && strings.HasSuffix(c.Args[0], "net-iptables.sh"):
		b, err := os.ReadFile(c.Args[0])
		if err != nil {
			return fail(err.Error())
		}
		f.ipt = string(b)
		return Result{}, true, nil
	case base == "iptables" || base == "ip6tables":
		if f.ipt == "" {
			if strings.Contains(a, "-S OUTPUT") {
				return Result{Stdout: "-P OUTPUT ACCEPT\n"}, true, nil
			}
			return fail("iptables: No chain/target/match by that name.")
		}
		switch {
		case strings.Contains(a, "-S OUTPUT"):
			// One jump per owner the script's loop names.
			out := "-P OUTPUT ACCEPT\n"
			for _, l := range strings.Split(f.ipt, "\n") {
				if o, ok := strings.CutPrefix(strings.TrimSpace(l), "for o in "); ok {
					for _, u := range strings.Fields(strings.TrimSuffix(o, "; do")) {
						out += "-A OUTPUT -m owner --uid-owner " + u + " -j TENTMAKER\n"
					}
				}
			}
			return Result{Stdout: out}, true, nil
		case strings.Contains(a, "-S TENTMAKER"):
			var out []string
			for _, l := range strings.Split(f.ipt, "\n") {
				if i := strings.Index(l, "-A TENTMAKER "); i >= 0 && strings.Contains(l, "/"+base+"' ") {
					out = append(out, l[i:])
				}
			}
			return Result{Stdout: strings.Join(out, "\n") + "\n"}, true, nil
		case strings.Contains(a, "-X TENTMAKER"):
			if base == "ip6tables" {
				f.ipt = ""
			}
		}
		return Result{}, true, nil
	case c.Path == "systemctl" && len(c.Args) == 2 && c.Args[0] == "enable":
		if f.enabled == nil {
			f.enabled = map[string]bool{}
		}
		f.enabled[c.Args[1]] = true
		return Result{}, true, nil
	case c.Path == "systemctl" && len(c.Args) == 2 && c.Args[0] == "disable":
		delete(f.enabled, c.Args[1])
		return Result{}, true, nil
	case c.Path == "systemctl" && len(c.Args) == 2 && c.Args[0] == "is-enabled":
		if f.enabled[c.Args[1]] {
			return Result{Stdout: "enabled\n"}, true, nil
		}
		return fail("disabled")
	}
	return Result{}, false, nil
}

func (f *fakeHost) LookupUser(name string) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noUser || name != User {
		return 0, 0, errors.New("unknown user")
	}
	return testUID, testGID, nil
}

func (f *fakeHost) Lchown(path string, uid, gid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chowns == nil {
		f.chowns = map[string][2]int{}
	}
	f.chowns[path] = [2]int{uid, gid}
	return nil
}

func (f *fakeHost) Fetch(ctx context.Context, url, dst string) error {
	f.mu.Lock()
	f.fetches = append(f.fetches, url)
	fn := f.fetch
	f.mu.Unlock()
	if fn == nil {
		return errors.New("no network in tests")
	}
	return fn(url, dst)
}

func (f *fakeHost) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.cmds {
		out = append(out, c.String())
	}
	return out
}

// assertNoSecret fails if the secret reached argv, env, a log or an error.
func assertNoSecret(t *testing.T, f *fakeHost, logs *bytes.Buffer, errs ...error) {
	t.Helper()
	needle := string(secret)
	probe := "S3CRET"
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		for _, s := range append(append([]string{c.Path}, c.Args...), c.Env...) {
			if strings.Contains(s, needle) || strings.Contains(s, probe) {
				t.Fatalf("secret in argv or env: %s", c)
			}
		}
	}
	if logs != nil && strings.Contains(logs.String(), probe) {
		t.Fatalf("secret in logs: %s", logs)
	}
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), probe) {
			t.Fatalf("secret in error: %v", err)
		}
	}
}

type env struct {
	e    *Executor
	f    *fakeHost
	logs *bytes.Buffer
	dir  string
	o    Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	f := &fakeHost{}
	logs := &bytes.Buffer{}
	p := Paths{
		Lib:               filepath.Join(dir, "lib"),
		Opt:               filepath.Join(dir, "opt"),
		Systemd:           filepath.Join(dir, "systemd"),
		Homes:             filepath.Join(dir, "home"),
		Subuid:            filepath.Join(dir, "subuid"),
		Subgid:            filepath.Join(dir, "subgid"),
		WSLInterop:        filepath.Join(dir, "proc", "WSLInterop"),
		OSRelease:         filepath.Join(dir, "proc", "osrelease"),
		CgroupControllers: filepath.Join(dir, "cgroup.controllers"),
		SystemdRunning:    filepath.Join(dir, "run-systemd"),
		RunUser:           filepath.Join(dir, "run-user"),
		PodmanSocketUnit:  "/usr/lib/systemd/user/podman.socket",
		DockerCLI:         filepath.Join(dir, "docker"),
		ResolvConf:        filepath.Join(dir, "resolv.conf"),
		IfInet6:           filepath.Join(dir, "proc", "if_inet6"),
		IPv6Route:         filepath.Join(dir, "proc", "ipv6_route"),
		Nft:               filepath.Join(dir, "sbin", "nft"),
		Iptables:          filepath.Join(dir, "sbin", "iptables"),
		Ip6tables:         filepath.Join(dir, "sbin", "ip6tables"),
	}
	must(t, os.MkdirAll(filepath.Join(dir, "sbin"), 0o755))
	for _, tool := range []string{p.Nft, p.Iptables, p.Ip6tables} {
		must(t, os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755))
	}
	must(t, os.WriteFile(p.ResolvConf, []byte("# managed\nnameserver 127.0.0.53\noptions edns0\n"), 0o644))
	o := Options{
		StateDir: filepath.Join(dir, "state"),
		CacheDir: filepath.Join(dir, "cache"),
		Log:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Arch:     "x64",
		Host:     f,
		Paths:    p,
		Sleep:    func(time.Duration) {},
		Binary: func(pl core.Platform) (string, error) {
			b := filepath.Join(dir, "tentmaker-"+pl.OS+"-"+pl.Arch)
			return b, os.WriteFile(b, []byte("#!guest binary"), 0o755)
		},
	}
	e := New(o)
	return &env{e: e, f: f, logs: logs, dir: dir, o: o}
}

// installed lays out what Install leaves behind, without running it.
func (v *env) installed(t *testing.T) {
	t.Helper()
	tpl := filepath.Join(v.dir, "opt", "actions-runner-x64-"+RunnerVersion)
	must(t, os.MkdirAll(filepath.Join(tpl, "bin"), 0o755))
	must(t, os.MkdirAll(filepath.Join(tpl, "externals", "node20", "bin"), 0o755))
	must(t, os.WriteFile(filepath.Join(tpl, "run.sh"), []byte("#!/bin/bash\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(tpl, ".credentials-template"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(tpl, "bin", "Runner.Listener"), []byte("elf"), 0o755))
	must(t, os.Symlink("Runner.Listener", filepath.Join(tpl, "bin", "listener-link")))
	must(t, os.WriteFile(filepath.Join(tpl, "externals", "node20", "bin", "node"), []byte("elf"), 0o755))
	must(t, os.Symlink(tpl, filepath.Join(v.dir, "opt", "runner")))
	must(t, os.MkdirAll(v.e.jobsDir(), 0o750))
	must(t, os.MkdirAll(v.e.secretDir(), 0o700))
	// The network rules, written and loaded.
	rules := NftRules(v.e.policy(testUID))
	must(t, os.MkdirAll(v.o.Paths.Lib, 0o755))
	must(t, os.WriteFile(v.e.nftRules(), []byte(rules), 0o644))
	v.f.mu.Lock()
	v.f.nft = rules
	v.f.mu.Unlock()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func spec(name, kind string) core.ExecSpec {
	return core.ExecSpec{
		Kind:        kind,
		Name:        name,
		Grant:       core.Grant{RunnerName: name, Secret: secret},
		Platform:    core.Platform{OS: "linux", Arch: "x64"},
		Limits:      core.Resources{MemoryBytes: 2 << 30, CPUs: 1.5},
		MaxDuration: 90 * time.Minute,
		TestWorkers: 4,
	}
}

func findCmd(t *testing.T, f *fakeHost, path string) Cmd {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if c.Path == path {
			return c
		}
	}
	t.Fatalf("no %s command; ran %v", path, f.cmds)
	return Cmd{}
}

func TestStartRunner(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	var stdinSeen []byte
	var stdinMode os.FileMode
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemd-run" {
			for i, a := range c.Args {
				if a == "-p" && strings.HasPrefix(c.Args[i+1], "StandardInput=file:") {
					p := strings.TrimPrefix(c.Args[i+1], "StandardInput=file:")
					stdinSeen, _ = os.ReadFile(p)
					if st, err := os.Stat(p); err == nil {
						stdinMode = st.Mode().Perm()
					}
				}
			}
		}
		return Result{}, nil
	}
	err := v.e.Start(context.Background(), spec("job-abc", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdinSeen, secret) || stdinMode != 0o600 {
		t.Fatalf("unit stdin file: %q mode %o", stdinSeen, stdinMode)
	}
	if ents, _ := os.ReadDir(v.e.secretDir()); len(ents) != 0 {
		t.Fatalf("stdin file left behind: %v", ents)
	}
	c := findCmd(t, v.f, "systemd-run")
	jd := v.e.jobDir("job-abc")
	rt := filepath.Join(v.dir, "run-user", "1234")
	want := []string{
		"--quiet", "--no-ask-password",
		"--unit=tentmaker-job-job-abc.service", "--slice=tentmaker.slice", "--uid=1234", "--gid=1235",
		"-p", "Type=exec", "-p", "MemoryMax=2147483648", "-p", "MemorySwapMax=0", "-p", "OOMPolicy=stop", "-p", "CPUQuota=150%",
		"-p", "TasksMax=4096", "-p", "Nice=10", "-p", "IOSchedulingClass=idle", "-p", "RuntimeMaxSec=5400",
		"-p", "TimeoutStopSec=30", "-p", "NoNewPrivileges=yes", "-p", "PrivateTmp=yes",
		"-p", "InaccessiblePaths=-" + rt + "/bus", "-p", "InaccessiblePaths=-" + rt + "/systemd",
		"-p", "InaccessiblePaths=-/run/tailscale", "-p", "InaccessiblePaths=-/var/run/tailscale", "-p", "InaccessiblePaths=-/var/lib/tailscale",
		"-p", "InaccessiblePaths=-" + filepath.Join(v.dir, "home"), "-p", "InaccessiblePaths=-/root",
		"-p", "StandardInput=file:" + v.e.secretPath("job-abc"),
		"-p", "StandardOutput=journal", "-p", "StandardError=journal",
	}
	if got := c.Args[:len(want)]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("systemd-run args:\n got %q\nwant %q", got, want)
	}
	tailArgs := c.Args[len(c.Args)-5:]
	wantTail := []string{"--working-directory=" + jd + "/runner", "--", v.e.jobScript(), v.e.jobsDir(), "job-abc"}
	if strings.Join(tailArgs, " ") != strings.Join(wantTail, " ") {
		t.Fatalf("exec: %q want %q", tailArgs, wantTail)
	}
	args := strings.Join(c.Args, " ")
	for _, s := range []string{"--setenv=HOME=" + jd + "/home", "--setenv=DOCKER_HOST=unix://" + rt + "/podman/podman.sock", "--setenv=TENTMAKER_JOBS=4", "--setenv=RUNNER_TOOL_CACHE=" + jd + "/toolcache"} {
		if !strings.Contains(args, s) {
			t.Errorf("missing %s", s)
		}
	}
	// A fresh runner copy, externals linked to the read-only template.
	tpl, _ := filepath.EvalSymlinks(filepath.Join(v.dir, "opt", "runner"))
	if l, err := os.Readlink(filepath.Join(jd, "runner", "externals")); err != nil || l != filepath.Join(tpl, "externals") {
		t.Fatalf("externals link %q %v", l, err)
	}
	if l, _ := os.Readlink(filepath.Join(jd, "runner", "bin", "listener-link")); l != "Runner.Listener" {
		t.Fatalf("symlink in template not copied as a link: %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(jd, "runner", ".credentials-template")); string(b) != "x" {
		t.Fatal("dotfile not copied")
	}
	envFile, _ := os.ReadFile(filepath.Join(jd, "runner", ".env"))
	if !strings.Contains(string(envFile), "HOME="+jd+"/home\n") {
		t.Fatalf(".env: %s", envFile)
	}
	// Everything in the job dir belongs to the runner; the template stays root's.
	for _, p := range []string{jd, jd + "/runner", jd + "/runner/run.sh", jd + "/runner/externals", jd + "/home", jd + "/runner/.env"} {
		if got := v.f.chowns[p]; got != [2]int{testUID, testGID} {
			t.Errorf("chown %s = %v", p, got)
		}
	}
	for p := range v.f.chowns {
		if strings.HasPrefix(p, tpl) {
			t.Errorf("template path chowned: %s", p)
		}
	}
	assertNoSecret(t, v.f, v.logs, err)
}

func TestStartCheck(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	if err := v.e.Start(context.Background(), spec("chk-001", core.KindCheck)); err != nil {
		t.Fatal(err)
	}
	c := findCmd(t, v.f, "systemd-run")
	jd := v.e.jobDir("chk-001")
	tail := c.Args[len(c.Args)-6:]
	want := []string{"--working-directory=" + jd + "/work", "--", v.e.guestBinary(), "check-run", "--out", jd + "/out"}
	if strings.Join(tail, " ") != strings.Join(want, " ") {
		t.Fatalf("got %q want %q", tail, want)
	}
	if _, err := os.Stat(filepath.Join(jd, "runner")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a check job got a runner copy")
	}
	if st, err := os.Stat(filepath.Join(jd, "out")); err != nil || !st.IsDir() {
		t.Fatal("no out dir")
	}
	// Setup actions find toolchains in <state>/tools, the folder doctor
	// and the docs name; the job's own env says where it is.
	if args := strings.Join(c.Args, " "); !strings.Contains(args, "--setenv=TENTMAKER_TOOLS="+filepath.Join(v.dir, "state", "tools")+" ") {
		t.Fatalf("a check job is not told the tools folder: %s", args)
	}
	assertNoSecret(t, v.f, v.logs)
}

// Setup copies the tentmaker binary for check jobs once. After an update
// the copy is stale, so Start refreshes it (root-owned, 0755) when it
// differs from the daemon's binary.
func TestStartCheckRefreshesGuestBinary(t *testing.T) {
	v := newEnv(t)
	src := filepath.Join(v.dir, "daemon-bin")
	must(t, os.WriteFile(src, []byte("tentmaker v1"), 0o755))
	v.o.Binary = func(core.Platform) (string, error) { return src, nil }
	v.e = New(v.o)
	v.installed(t)
	guest := v.e.guestBinary()
	must(t, os.MkdirAll(filepath.Dir(guest), 0o755))
	must(t, os.WriteFile(guest, []byte("tentmaker v1"), 0o755))

	// An update replaced the daemon's binary.
	must(t, os.WriteFile(src, []byte("tentmaker v2, newer"), 0o755))
	must(t, v.e.Start(context.Background(), spec("chk-001", core.KindCheck)))
	if b, _ := os.ReadFile(guest); string(b) != "tentmaker v2, newer" {
		t.Fatalf("guest binary is %q", b)
	}
	if st, err := os.Stat(guest); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("guest binary mode: %v %v", st.Mode(), err)
	}
	v.f.mu.Lock()
	owner, ok := v.f.chowns[guest]
	delete(v.f.chowns, guest)
	v.f.mu.Unlock()
	if !ok || owner != [2]int{0, 0} {
		t.Fatalf("guest binary not root-owned: %v %v", owner, ok)
	}

	// Unchanged: not copied again.
	must(t, v.e.Start(context.Background(), spec("chk-002", core.KindCheck)))
	v.f.mu.Lock()
	_, again := v.f.chowns[guest]
	v.f.mu.Unlock()
	if again {
		t.Fatal("an up-to-date guest binary was rewritten")
	}

	// Same size, different bytes: replaced.
	must(t, os.WriteFile(src, []byte("tentmaker v3, newer"), 0o755))
	must(t, v.e.Start(context.Background(), spec("chk-003", core.KindCheck)))
	if b, _ := os.ReadFile(guest); string(b) != "tentmaker v3, newer" {
		t.Fatalf("guest binary is %q", b)
	}
}

func TestStartRefuses(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	bad := []core.ExecSpec{}
	s := spec("../etc", "")
	bad = append(bad, s)
	s = spec("Upper-case", "")
	bad = append(bad, s)
	s = spec("ok-name", "")
	s.Platform = core.Platform{OS: "linux", Arch: "arm64"}
	bad = append(bad, s)
	s = spec("ok-name", "")
	s.Grant.Secret = nil
	bad = append(bad, s)
	s = spec("ok-name", "")
	s.Limits = core.Resources{}
	bad = append(bad, s)
	s = spec("ok-name", "deploy")
	bad = append(bad, s)
	for _, b := range bad {
		if err := v.e.Start(context.Background(), b); err == nil {
			t.Errorf("Start(%+v) succeeded", b.Name)
		}
	}
	if len(v.f.cmds) != 0 {
		t.Fatalf("commands ran for refused specs: %v", v.f.commands())
	}
	// A failing systemd-run leaves no job dir and no stdin file.
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemd-run" {
			return Result{Code: 1}, errors.New("systemd-run: failed")
		}
		return Result{}, nil
	}
	err := v.e.Start(context.Background(), spec("job-fail", ""))
	if err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(v.e.jobDir("job-fail")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("job dir left after a failed start")
	}
	if _, err := os.Stat(v.e.secretPath("job-fail")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stdin file left after a failed start")
	}
	assertNoSecret(t, v.f, v.logs, err)
}

func showResponder(active, load string, fail bool) func(c Cmd) (Result, error) {
	return func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "show" {
			if fail {
				return Result{Code: -1}, errors.New("systemctl: timed out")
			}
			return Result{Stdout: "LoadState=" + load + "\nActiveState=" + active + "\n"}, nil
		}
		return Result{}, nil
	}
}

func TestState(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	must(t, os.Mkdir(v.e.jobDir("job-here"), 0o700))
	cases := []struct {
		name, active, load string
		fail               bool
		want               core.ExecState
	}{
		{"job-here", "active", "loaded", false, core.ExecRunning},
		{"job-here", "activating", "loaded", false, core.ExecRunning},
		{"job-here", "deactivating", "loaded", false, core.ExecRunning},
		{"job-here", "inactive", "not-found", false, core.ExecExited},
		{"job-here", "failed", "loaded", false, core.ExecExited},
		{"job-gone", "inactive", "not-found", false, core.ExecMissing},
		{"job-here", "maintenance", "loaded", false, core.ExecUnknown},
		{"job-here", "", "", true, core.ExecUnknown},
	}
	for _, c := range cases {
		v.f.respond = showResponder(c.active, c.load, c.fail)
		got, _ := v.e.State(context.Background(), c.name)
		if got != c.want {
			t.Errorf("%s %s/%s: got %s want %s", c.name, c.active, c.load, got, c.want)
		}
	}
	if st, err := v.e.State(context.Background(), "x"); err == nil || st != core.ExecUnknown {
		t.Fatal("bad name accepted")
	}
}

func TestStopAndClean(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	must(t, v.e.Start(context.Background(), spec("job-one", "")))
	v.f.respond = showResponder("active", "loaded", false)
	if err := v.e.Stop(context.Background(), "job-one"); err == nil {
		t.Fatal("Stop says done while the unit still runs")
	}
	if err := v.e.Clean(context.Background(), "job-one"); err == nil {
		t.Fatal("Clean ran on a running job")
	}
	v.f.respond = showResponder("", "", true)
	if err := v.e.Clean(context.Background(), "job-one"); err == nil {
		t.Fatal("Clean ran on a job whose state is unknown")
	}
	v.f.respond = showResponder("inactive", "not-found", false)
	must(t, v.e.Stop(context.Background(), "job-one"))
	must(t, v.e.Clean(context.Background(), "job-one"))
	if _, err := os.Stat(v.e.jobDir("job-one")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("job dir survived Clean")
	}
	cmds := strings.Join(v.f.commands(), "\n")
	for _, s := range []string{
		"systemctl stop tentmaker-job-job-one.service",
		"runuser -u tentmaker-runner -- env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LANG=C.UTF-8 HOME=" + v.e.root,
		"podman unshare rm -rf -- " + v.e.jobDir("job-one"),
	} {
		if !strings.Contains(cmds, s) {
			t.Errorf("missing %q in\n%s", s, cmds)
		}
	}
	// Cleaning again is fine.
	must(t, v.e.Clean(context.Background(), "job-one"))
	assertNoSecret(t, v.f, v.logs)
}

func TestList(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	must(t, os.Mkdir(v.e.jobDir("job-dir"), 0o700))
	must(t, os.Mkdir(v.e.jobDir("NOT_A_NAME"), 0o700))
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && c.Args[0] == "list-units" {
			return Result{Stdout: "tentmaker-job-job-unit.service loaded active running x\ntentmaker-job-job-dir.service loaded inactive dead x\nother.service loaded active running y\n"}, nil
		}
		return Result{}, nil
	}
	got, err := v.e.List(context.Background())
	must(t, err)
	if strings.Join(got, ",") != "job-dir,job-unit" {
		t.Fatalf("List = %v", got)
	}
}

func TestSetPoolLimit(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	must(t, v.e.SetPoolLimit(ctx, core.Resources{MemoryBytes: 8 << 30, CPUs: 4}, core.Resources{MemoryBytes: 2 << 30, CPUs: 2}))
	must(t, v.e.SetPoolLimit(ctx, core.Resources{MemoryBytes: 1 << 30, CPUs: 0.5}, core.Resources{MemoryBytes: 4 << 30, CPUs: 2.25}))
	must(t, v.e.SetPoolLimit(ctx, core.Resources{}, core.Resources{}))
	got := v.f.commands()
	want := []string{
		"systemctl set-property --runtime tentmaker.slice MemoryMax=8589934592 CPUQuota=400%",
		"systemctl set-property --runtime tentmaker.slice MemoryMax=4294967296 CPUQuota=225%",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestReset(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	must(t, os.WriteFile(v.e.baselinePath(), []byte("dbus.socket\npodman.service\npodman.socket\n"), 0o600))
	must(t, os.Mkdir(v.e.jobDir("job-old"), 0o700))
	must(t, os.WriteFile(filepath.Join(v.e.jobsDir(), ".hidden"), []byte("x"), 0o600))
	userUnits := "dbus.socket loaded active running x\npodman.socket loaded active listening y\n"
	jobUnits := ""
	v.f.respond = func(c Cmd) (Result, error) {
		a := strings.Join(c.Args, " ")
		switch {
		case c.Path == "systemctl" && strings.HasPrefix(a, "list-units"):
			return Result{Stdout: jobUnits}, nil
		case strings.Contains(a, "systemctl --user list-units"):
			return Result{Stdout: userUnits}, nil
		}
		return Result{}, nil
	}
	jobUnits = "tentmaker-job-job-live.service loaded active running x\n"
	if err := v.e.Reset(context.Background()); err == nil || !strings.Contains(err.Error(), "job-live") {
		t.Fatalf("Reset with a running job: %v", err)
	}
	if strings.Contains(strings.Join(v.f.commands(), "\n"), "user@") {
		t.Fatal("Reset touched the user manager while a job ran")
	}
	jobUnits = "tentmaker-job-job-old.service loaded failed failed x\n"
	must(t, v.e.Reset(context.Background()))
	cmds := strings.Join(v.f.commands(), "\n")
	for _, s := range []string{
		"systemctl stop user@1234.service user-runtime-dir@1234.service",
		"systemctl start user@1234.service",
		"podman pod rm -af", "podman rm -af", "podman volume prune -f", "podman network prune -f", "podman rmi -af",
	} {
		if !strings.Contains(cmds, s) {
			t.Errorf("Reset did not run %q", s)
		}
	}
	if ents, _ := os.ReadDir(v.e.jobsDir()); len(ents) != 0 {
		t.Fatalf("jobs left after reset: %v", ents)
	}
	userUnits += "evil.timer loaded active waiting z\n"
	if err := v.e.Reset(context.Background()); err == nil || !strings.Contains(err.Error(), "evil.timer") {
		t.Fatalf("planted unit not caught: %v", err)
	}
}

func TestCheckResult(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	out := filepath.Join(v.e.jobDir("chk-res"), "out")
	must(t, os.MkdirAll(out, 0o700))

	r, err := v.e.CheckResult(ctx, "chk-res")
	must(t, err)
	if r.State != "error" || !strings.Contains(r.Description, "no result.json") {
		t.Fatalf("missing result: %+v", r)
	}

	log := bytes.Repeat([]byte("a"), LogTailBytes) // older output
	log = append(log, []byte("THE END")...)
	must(t, os.WriteFile(filepath.Join(out, "log.txt"), log, 0o600))
	want := core.CheckResult{Repo: "o/r", SHA: "abc", Context: "tentmaker/test", State: "failure", Description: "step 2 failed", Duration: time.Minute}
	b, _ := json.Marshal(want)
	must(t, os.WriteFile(filepath.Join(out, "result.json"), b, 0o600))
	r, err = v.e.CheckResult(ctx, "chk-res")
	must(t, err)
	if r.State != "failure" || r.Runner != "chk-res" || r.Context != "tentmaker/test" || r.Duration != time.Minute {
		t.Fatalf("result: %+v", r)
	}
	if len(r.LogTail) != LogTailBytes || !strings.HasSuffix(r.LogTail, "THE END") {
		t.Fatalf("log tail is %d bytes", len(r.LogTail))
	}

	must(t, os.WriteFile(filepath.Join(out, "result.json"), []byte(`{"state":"pwned"}`), 0o600))
	if r, _ = v.e.CheckResult(ctx, "chk-res"); r.State != "error" {
		t.Fatalf("bad state accepted: %+v", r)
	}
	must(t, os.WriteFile(filepath.Join(out, "result.json"), []byte(`{not json`), 0o600))
	if r, _ = v.e.CheckResult(ctx, "chk-res"); r.State != "error" {
		t.Fatalf("bad json accepted: %+v", r)
	}

	// A job that links result.json or its out dir to a root-only file gets nothing.
	outside := filepath.Join(v.dir, "root-only.json")
	must(t, os.WriteFile(outside, []byte(`{"state":"success","description":"leaked"}`), 0o600))
	must(t, os.Remove(filepath.Join(out, "result.json")))
	must(t, os.Symlink(outside, filepath.Join(out, "result.json")))
	if r, _ = v.e.CheckResult(ctx, "chk-res"); r.State != "error" || strings.Contains(r.Description, "leaked") {
		t.Fatalf("symlinked result.json was read: %+v", r)
	}
	must(t, os.RemoveAll(out))
	must(t, os.Symlink(v.dir, out))
	must(t, os.WriteFile(filepath.Join(v.dir, "result.json"), []byte(`{"state":"success","description":"leaked"}`), 0o600))
	if r, _ = v.e.CheckResult(ctx, "chk-res"); r.State != "error" || strings.Contains(r.Description, "leaked") {
		t.Fatalf("symlinked out dir was followed: %+v", r)
	}
	if _, err := v.e.CheckResult(ctx, "../x"); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestRedact(t *testing.T) {
	if got := redact("boom "+string(secret)+" end", secret); strings.Contains(got, "S3CRET") {
		t.Fatal(got)
	}
	if got := redact("x", nil); got != "x" {
		t.Fatal(got)
	}
}

func TestPlatforms(t *testing.T) {
	v := newEnv(t)
	if p := v.e.Platforms(); len(p) != 1 || p[0] != (core.Platform{OS: "linux", Arch: "x64"}) {
		t.Fatalf("%v", p)
	}
	if New(Options{Arch: "riscv64"}).Platforms() != nil {
		t.Fatal("riscv64 has no pinned runner")
	}
	if v.e.Name() != "linux-podman" || v.e.MaxSlots() != 0 {
		t.Fatal("name or slots")
	}
	if goArch("amd64") != "x64" || goArch("arm64") != "arm64" {
		t.Fatal("arch mapping")
	}
	pin, _ := PinFor("arm64")
	if pin.URL() != "https://github.com/actions/runner/releases/download/v"+RunnerVersion+"/actions-runner-linux-arm64-"+RunnerVersion+".tar.gz" {
		t.Fatal(pin.URL())
	}
}

// A job the kernel killed for memory says so. Before this, systemd-run
// --collect dropped the unit's Result before Tentmaker read it, so the
// status said "cancelled at step ..." (check-run caught the SIGTERM from
// OOMPolicy=stop) or "check-run wrote no result.json" (the OOM killer
// picked check-run itself).
func TestCheckResultOutOfMemory(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	if strings.Contains(strings.Join(v.e.unitArgs(spec("job-x", "runner"), core.KindCheck, 1234, 1234, "/in"), " "), "--collect") {
		t.Fatal("--collect unloads a failed unit before CheckResult reads its Result")
	}
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && strings.Join(c.Args, " ") == "show --property=Result --property=MemoryMax "+unitName("chk-oom") {
			return Result{Stdout: "Result=oom-kill\nMemoryMax=2147483648\n"}, nil
		}
		return Result{}, nil
	}
	out := filepath.Join(v.e.jobDir("chk-oom"), "out")
	must(t, os.MkdirAll(out, 0o700))
	r, err := v.e.CheckResult(ctx, "chk-oom")
	must(t, err)
	if r.State != "failure" || r.Description != "ran out of memory (the job's limit is 2.0 GiB)" {
		t.Fatalf("no result.json after an OOM kill: %+v", r)
	}
	b, _ := json.Marshal(core.CheckResult{State: "error", Description: "cancelled at step Run x, 1s", Duration: time.Second})
	must(t, os.WriteFile(filepath.Join(out, "result.json"), b, 0o600))
	r, err = v.e.CheckResult(ctx, "chk-oom")
	must(t, err)
	if r.State != "failure" || r.Description != "ran out of memory (the job's limit is 2.0 GiB)" || r.Duration != time.Second {
		t.Fatalf("cancelled by the OOM stop: %+v", r)
	}

	// Clean unloads the failed unit the job left.
	v.f.respond = showResponder("failed", "loaded", false)
	n := len(v.f.cmds)
	must(t, v.e.Clean(ctx, "chk-oom"))
	if !strings.Contains(strings.Join(v.f.commands()[n:], "\n"), "systemctl reset-failed "+unitName("chk-oom")) {
		t.Error("Clean left the failed unit loaded")
	}
}
