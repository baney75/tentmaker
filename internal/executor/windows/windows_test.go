package windows

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

const testSID = "S-1-5-21-1-2-3-1001"

// jit stands in for a grant's single-use registration.
var jit = `{"jit":"S3CRET-jit-config-do-not-leak"}`

type fakeProc struct {
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	code       int
	terminated int
}

func newFakeProc() *fakeProc { return &fakeProc{done: make(chan struct{}), code: -1} }

func (p *fakeProc) Done() <-chan struct{} { return p.done }

func (p *fakeProc) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

func (p *fakeProc) exit(code int) {
	p.once.Do(func() {
		p.mu.Lock()
		p.code = code
		p.mu.Unlock()
		close(p.done)
	})
}

func (p *fakeProc) Terminate() error {
	p.mu.Lock()
	p.terminated++
	p.mu.Unlock()
	p.exit(1)
	return nil
}

func (p *fakeProc) terminations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminated
}

// fakeAPI records every call. Protect is a reversible scramble so a test
// can tell the stored blob is not the password.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	admin    bool
	users    map[string]string // name -> SID
	password []byte
	groups   []Group
	rights   map[string][]string
	hidden   map[string]bool
	dacls    map[string][]ACE
	procs    []ProcSpec
	running  []*fakeProc
	startErr error
	onStart  func() // runs inside StartProcess, before it records anything
	zip      []byte
	fetches  int
	killed   int
	tasks    int
	free     uint64
	// Windows Firewall: rule name -> "Action Description", profile -> on.
	fw       map[string]string
	profiles map[string]string
	dns      []string
	// addrs are the host's addresses (Get-NetIPAddress); self is what
	// Tentmaker-Block-Self names.
	addrs   []string
	self    []string
	scripts []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{admin: true, users: map[string]string{}, rights: map[string][]string{},
		hidden: map[string]bool{}, dacls: map[string][]ACE{}, free: 100 << 30,
		fw: map[string]string{}, profiles: map[string]string{"Domain": "True", "Private": "True", "Public": "True"},
		dns:   []string{"168.63.129.16", "fe80::1%4", "192.168.1.1"},
		addrs: []string{"127.0.0.1", "::1", "fe80::5%4", "192.168.1.20", "2001:db8:1::20"}}
}

// PowerShell plays the firewall scripts netrules.go runs.
func (f *fakeAPI) PowerShell(ctx context.Context, script string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	first, _, _ := strings.Cut(script, "\n")
	f.calls = append(f.calls, "PowerShell "+first)
	switch first {
	case "# tentmaker: DNS servers":
		return strings.Join(f.dns, "\r\n") + "\r\n", nil
	case "# tentmaker: host addresses":
		return strings.Join(f.addrs, "\r\n") + "\r\n", nil
	case "# tentmaker: host address rule":
		_, ok := f.fw["Tentmaker-Block-Self"]
		for _, l := range strings.Split(script, "\n") {
			switch {
			case strings.HasPrefix(l, "if ($r) { $r | Remove-NetFirewallRule }"):
				delete(f.fw, "Tentmaker-Block-Self")
				f.self = nil
			case strings.HasPrefix(l, "if ($r) { Set-NetFirewallRule") && ok,
				strings.HasPrefix(l, "New-NetFirewallRule -Name 'Tentmaker-Block-Self'") && !ok:
				f.self = fakeAddrs(l)
			}
			if v, found := strings.CutPrefix(l, "$d = '"); found && !ok {
				f.fw["Tentmaker-Block-Self"] = "Block " + strings.ReplaceAll(strings.TrimSuffix(v, "'"), "''", "'")
			}
		}
		return "", nil
	case "# tentmaker: install network rules":
		clear(f.fw)
		f.self = nil
		desc := ""
		for _, l := range strings.Split(script, "\n") {
			if v, ok := strings.CutPrefix(l, "$d = '"); ok {
				desc = strings.ReplaceAll(strings.TrimSuffix(v, "'"), "''", "'")
			}
			if v, ok := strings.CutPrefix(l, "New-NetFirewallRule -Name '"); ok {
				name, _, _ := strings.Cut(v, "'")
				_, act, _ := strings.Cut(l, "-Action ")
				act, _, _ = strings.Cut(act, " ")
				f.fw[name] = act + " " + desc
				if name == "Tentmaker-Block-Self" {
					f.self = fakeAddrs(l)
				}
			}
		}
		return "", nil
	case "# tentmaker: remove network rules":
		clear(f.fw)
		f.self = nil
		return "", nil
	case "# tentmaker: check network rules":
		var b strings.Builder
		for _, p := range []string{"Domain", "Private", "Public"} {
			if v, ok := f.profiles[p]; ok {
				fmt.Fprintf(&b, "profile %s %s NotConfigured\r\n", p, v)
			}
		}
		names := make([]string, 0, len(f.fw))
		for n := range f.fw {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			fmt.Fprintf(&b, "rule %s True Outbound %s\r\n", n, f.fw[n])
		}
		if _, ok := f.fw["Tentmaker-Block-Self"]; ok {
			fmt.Fprintf(&b, "selfrule %s\r\n", strings.Join(f.self, " "))
		}
		for _, a := range f.addrs {
			fmt.Fprintf(&b, "addr %s\r\n", a)
		}
		for _, a := range f.dns {
			fmt.Fprintf(&b, "dns %s\r\n", a)
		}
		return b.String(), nil
	}
	return "", errors.New("fake PowerShell: unknown script " + first)
}

// fakeAddrs reads the -RemoteAddress @('a', 'b') list of a script line.
func fakeAddrs(l string) []string {
	_, v, ok := strings.Cut(l, "-RemoteAddress @(")
	if !ok {
		return nil
	}
	v, _, _ = strings.Cut(v, ")")
	var out []string
	for _, a := range strings.Split(v, ", ") {
		out = append(out, strings.Trim(a, "'"))
	}
	return out
}

func (f *fakeAPI) rec(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeAPI) IsAdmin() (bool, error)             { return f.admin, nil }
func (f *fakeAPI) FreeDisk(string) (uint64, error)    { return f.free, nil }
func (f *fakeAPI) Protect(b []byte) ([]byte, error)   { return scramble(b), nil }
func (f *fakeAPI) Unprotect(b []byte) ([]byte, error) { return scramble(b), nil }

func scramble(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ 0x5A
	}
	return out
}

func (f *fakeAPI) UserSID(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sid, ok := f.users[name]; ok {
		return sid, nil
	}
	return "", ErrNoUser
}

func (f *fakeAPI) CreateUser(name string, pw []byte) error {
	f.rec("CreateUser " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[name] = testSID
	f.password = bytes.Clone(pw)
	f.groups = []Group{{"Users", SIDUsers}, {"Remote Desktop Users", "S-1-5-32-555"}}
	return nil
}

func (f *fakeAPI) SetPassword(name string, pw []byte) error {
	f.rec("SetPassword " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.password = bytes.Clone(pw)
	return nil
}

func (f *fakeAPI) DeleteUser(name string) error {
	f.rec("DeleteUser " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.users, name)
	return nil
}

func (f *fakeAPI) LocalGroups(string) ([]Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.groups), nil
}

func (f *fakeAPI) RemoveFromGroup(group, name string) error {
	f.rec("RemoveFromGroup " + group + " " + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = slices.DeleteFunc(f.groups, func(g Group) bool { return g.Name == group })
	return nil
}

func (f *fakeAPI) AccountRights(sid string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rights[sid]), nil
}

func (f *fakeAPI) AddRights(sid string, r []string) error {
	f.rec("AddRights " + sid + " " + strings.Join(r, ","))
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range r {
		if !slices.Contains(f.rights[sid], x) {
			f.rights[sid] = append(f.rights[sid], x)
		}
	}
	return nil
}

func (f *fakeAPI) RemoveAllRights(sid string) error {
	f.rec("RemoveAllRights " + sid)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rights, sid)
	return nil
}

func (f *fakeAPI) HideUser(name string, hide bool) error {
	f.rec(fmt.Sprintf("HideUser %s %v", name, hide))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hidden[name] = hide
	return nil
}

func (f *fakeAPI) SetDACL(path string, aces []ACE) error {
	f.rec("SetDACL " + path)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dacls[path] = slices.Clone(aces)
	return nil
}

func (f *fakeAPI) StartProcess(p ProcSpec) (Process, error) {
	f.rec("StartProcess " + p.App + " " + strings.Join(p.Args, " "))
	if f.onStart != nil {
		f.onStart()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	p.Password, p.Stdin = bytes.Clone(p.Password), bytes.Clone(p.Stdin)
	f.procs = append(f.procs, p)
	pr := newFakeProc()
	f.running = append(f.running, pr)
	return pr, nil
}

func (f *fakeAPI) KillUserProcesses(sid string) (int, error) {
	f.rec("KillUserProcesses " + sid)
	return f.killed, nil
}

func (f *fakeAPI) DeleteUserTasks(name, sid string) (int, error) {
	f.rec("DeleteUserTasks " + name + " " + sid)
	return f.tasks, nil
}

func (f *fakeAPI) Fetch(ctx context.Context, url, dst string) error {
	f.rec("Fetch " + url)
	f.mu.Lock()
	f.fetches++
	f.mu.Unlock()
	return os.WriteFile(dst, f.zip, 0o644)
}

func (f *fakeAPI) has(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeAPI) proc(i int) *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[i]
}

type tenv struct {
	t    *testing.T
	dir  string
	api  *fakeAPI
	logs *syncBuffer
	o    Options
	e    *Executor
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	names := make([]string, 0, len(entries))
	for k := range entries {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(entries[name]))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newEnv(t *testing.T) *tenv {
	t.Helper()
	dir := t.TempDir()
	api := newFakeAPI()
	api.zip = makeZip(t, map[string]string{
		"run.cmd":                   "@echo run",
		"config.cmd":                "@echo config",
		"bin/Runner.Listener.exe":   "listener",
		"bin/sub/deep/file.dll":     "dll",
		"externals/node20/node.exe": "node",
	})
	sum := sha256.Sum256(api.zip)
	bin := filepath.Join(dir, "tentmaker.exe")
	must(t, os.WriteFile(bin, []byte("guest-binary"), 0o755))
	logs := &syncBuffer{}
	env := map[string]string{
		"PATH":         `C:\Windows\System32;C:\Program Files\Git\cmd`,
		"ProgramFiles": `C:\Program Files`,
		"DAEMON_ONLY":  "not-for-jobs",
	}
	o := Options{
		StateDir:   filepath.Join(dir, "state"),
		CacheDir:   filepath.Join(dir, "cache"),
		Log:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Binary:     func(core.Platform) (string, error) { return bin, nil },
		Arch:       "x64",
		SystemRoot: `C:\Windows`,
		HostCPUs:   8,
		Getenv:     func(k string) string { return env[k] },
		API:        api,
		Pin:        &Pin{Version: RunnerVersion, Arch: "x64", SHA256: hex.EncodeToString(sum[:])},
		Sleep:      func(time.Duration) {},
	}
	return &tenv{t: t, dir: dir, api: api, logs: logs, o: o, e: New(o)}
}

func (v *tenv) install() {
	v.t.Helper()
	must(v.t, Install(context.Background(), v.o))
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
		Grant:       core.Grant{Secret: []byte(jit)},
		Platform:    core.Platform{OS: "windows", Arch: "x64"},
		Limits:      core.Resources{MemoryBytes: 4 << 30, CPUs: 2},
		MaxDuration: time.Hour,
		TestWorkers: 3,
	}
}

// assertNoSecret scans everything the executor handed the OS, except the
// two fields meant for secrets (Password, Stdin), plus the log and any
// extra strings such as returned errors.
func (v *tenv) assertNoSecret(extra ...string) {
	v.t.Helper()
	v.api.mu.Lock()
	pw := string(v.api.password)
	hay := slices.Clone(v.api.calls)
	for _, p := range v.api.procs {
		hay = append(hay, p.App, p.Dir, p.LogPath, p.User, p.UserSID)
		hay = append(hay, p.Args...)
		hay = append(hay, p.Env...)
	}
	v.api.mu.Unlock()
	hay = append(hay, v.logs.String())
	hay = append(hay, extra...)
	for _, h := range hay {
		if strings.Contains(h, jit) {
			v.t.Fatalf("the grant leaked: %q", h)
		}
		if pw != "" && strings.Contains(h, pw) {
			v.t.Fatalf("the runner password leaked: %q", h)
		}
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, val, _ := strings.Cut(kv, "=")
		m[k] = val
	}
	return m
}

func TestInstall(t *testing.T) {
	v := newEnv(t)
	v.install()
	a := v.api
	if !a.has("CreateUser "+User) || a.has("SetPassword") {
		t.Fatalf("first install should create the user: %v", a.calls)
	}
	if len(a.password) < 40 || !strings.HasPrefix(string(a.password), complexityPrefix) {
		t.Fatalf("weak password: %d bytes", len(a.password))
	}
	blob, err := os.ReadFile(v.e.passwordPath())
	must(t, err)
	if bytes.Contains(blob, a.password) || !bytes.Equal(scramble(blob), a.password) {
		t.Fatal("the stored password is not the protected blob")
	}
	if _, err := os.Stat(v.e.passwordPath() + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("left the .new password file")
	}
	if len(a.groups) != 1 || a.groups[0].SID != SIDUsers {
		t.Fatalf("groups = %v, want only Users", a.groups)
	}
	for _, r := range append(slices.Clone(GrantRights), DenyRights...) {
		if !slices.Contains(a.rights[testSID], r) {
			t.Fatalf("missing right %s", r)
		}
	}
	if !a.hidden[User] {
		t.Fatal("user not hidden from the sign-in screen")
	}
	for _, d := range []string{v.e.jobsDir(), v.e.runnerRoot(), v.e.secretDir(), v.e.distDir()} {
		if !slices.Equal(a.dacls[d], adminACL()) {
			t.Fatalf("%s ACL = %v, want SYSTEM and Administrators only", d, a.dacls[d])
		}
	}
	if !slices.Equal(a.dacls[v.e.libDir()], libACL(testSID)) {
		t.Fatalf("lib ACL = %v", a.dacls[v.e.libDir()])
	}
	// The runner user reads and runs toolchains from the tools folder.
	if !slices.Equal(a.dacls[v.e.toolsDir()], libACL(testSID)) {
		t.Fatalf("tools ACL = %v", a.dacls[v.e.toolsDir()])
	}
	for _, f := range []string{
		filepath.Join(v.e.template(), "run.cmd"),
		filepath.Join(v.e.template(), "externals", "node20", "node.exe"),
		v.e.script(), v.e.guestBinary(),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(v.e.guestBinary()); string(got) != "guest-binary" {
		t.Fatal("guest binary not copied")
	}
	if p := v.e.Preflight(context.Background()); len(p) != 0 {
		t.Fatalf("Preflight after install: %v", p)
	}

	// A second run resets the password, keeps the verified runner, does not
	// download again and drops other runner versions.
	old := slices.Clone(a.password)
	stale := filepath.Join(v.e.runnerRoot(), "actions-runner-win-x64-2.300.0")
	must(t, os.MkdirAll(stale, 0o700))
	v.install()
	if !a.has("SetPassword "+User) || bytes.Equal(old, a.password) {
		t.Fatal("rerun should reset the password")
	}
	blob, _ = os.ReadFile(v.e.passwordPath())
	if !bytes.Equal(scramble(blob), a.password) {
		t.Fatal("stored password does not match the account after rerun")
	}
	if a.fetches != 1 {
		t.Fatalf("fetched %d times, want 1", a.fetches)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old runner version kept")
	}
	v.assertNoSecret()
}

func TestInstallRefuses(t *testing.T) {
	v := newEnv(t)
	v.api.admin = false
	if err := Install(context.Background(), v.o); err == nil {
		t.Fatal("install ran without admin")
	}
	if v.api.has("CreateUser") {
		t.Fatal("created a user without admin")
	}

	v = newEnv(t)
	v.api.zip = []byte("not the pinned zip")
	err := Install(context.Background(), v.o)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("bad download: %v", err)
	}
	if ents, _ := os.ReadDir(v.e.distDir()); len(ents) != 0 {
		t.Fatal("kept a download that failed its hash")
	}

	v = newEnv(t)
	v.o.Arch = "x86"
	v.o.Pin = nil
	if err := Install(context.Background(), v.o); err == nil {
		t.Fatal("installed on an unsupported arch")
	}
}

func TestPreflight(t *testing.T) {
	v := newEnv(t)
	p := v.e.Preflight(context.Background())
	if len(p) != 1 || p[0].Check != "windows-user" {
		t.Fatalf("fresh host: %v", p)
	}
	v.install()
	v.api.groups = append(v.api.groups, Group{"Administrators", SIDAdmins})
	v.api.rights[testSID] = []string{"SeBatchLogonRight"}
	v.api.free = 1 << 30
	must(t, os.Remove(v.e.script()))
	checks := map[string]int{}
	for _, x := range v.e.Preflight(context.Background()) {
		checks[x.Check]++
	}
	for _, c := range []string{"windows-user-groups", "windows-user-rights", "windows-files", "windows-disk"} {
		if checks[c] == 0 {
			t.Fatalf("Preflight missed %s: %v", c, checks)
		}
	}
	v.api.admin = false
	if p := v.e.Preflight(context.Background()); len(p) != 1 || p[0].Check != "windows-admin" {
		t.Fatalf("non-admin: %v", p)
	}
}

func TestStartRunner(t *testing.T) {
	v := newEnv(t)
	v.install()
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("job-runner-1", "")))
	if len(v.api.procs) != 1 {
		t.Fatalf("procs = %d", len(v.api.procs))
	}
	p := v.api.procs[0]
	jd := v.e.jobDir("job-runner-1")
	if p.User != User || p.UserSID != testSID || !bytes.Equal(p.Password, v.api.password) {
		t.Fatal("wrong account or password")
	}
	if string(p.Stdin) != jit {
		t.Fatal("the grant must arrive on stdin")
	}
	if p.App != v.e.powershell() || !slices.Contains(p.Args, v.e.script()) ||
		p.Args[len(p.Args)-1] != filepath.Join(jd, "runner") || p.Dir != filepath.Join(jd, "runner") {
		t.Fatalf("command: %s %v in %s", p.App, p.Args, p.Dir)
	}
	if p.MemoryBytes != 4<<30 || p.CPURate != 2500 {
		t.Fatalf("limits: mem %d rate %d", p.MemoryBytes, p.CPURate)
	}
	if p.LogPath != filepath.Join(jd, "job.log") {
		t.Fatalf("log path %s", p.LogPath)
	}
	if !slices.Equal(v.api.dacls[jd], jobACL(testSID)) {
		t.Fatalf("job ACL = %v", v.api.dacls[jd])
	}
	env := envMap(p.Env)
	want := map[string]string{
		"USERPROFILE":       filepath.Join(jd, "home"),
		"TEMP":              filepath.Join(jd, "tmp"),
		"RUNNER_TOOL_CACHE": filepath.Join(jd, "toolcache"),
		"TENTMAKER_JOBS":    "3",
		"PATH":              `C:\Windows\System32;C:\Program Files\Git\cmd`,
		"ProgramFiles":      `C:\Program Files`,
		"SystemRoot":        `C:\Windows`,
	}
	for k, w := range want {
		if env[k] != w {
			t.Fatalf("env %s = %q, want %q", k, env[k], w)
		}
	}
	if _, ok := env["DAEMON_ONLY"]; ok {
		t.Fatal("a daemon variable outside the allowlist reached the job")
	}
	for _, f := range []string{"runner/run.cmd", "runner/externals/node20/node.exe", "home/AppData/Local", "tmp", "work"} {
		if _, err := os.Stat(filepath.Join(jd, filepath.FromSlash(f))); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := v.e.State(ctx, "job-runner-1"); st != core.ExecRunning {
		t.Fatalf("state %s", st)
	}
	if err := v.e.Start(ctx, spec("job-runner-1", "")); err == nil {
		t.Fatal("started the same name twice")
	}
	v.assertNoSecret()
}

func TestStartCheck(t *testing.T) {
	v := newEnv(t)
	v.install()
	must(t, v.e.Start(context.Background(), spec("check-1", core.KindCheck)))
	p := v.api.procs[0]
	jd := v.e.jobDir("check-1")
	if p.App != v.e.guestBinary() || !slices.Equal(p.Args, []string{"check-run", "--out", filepath.Join(jd, "out")}) ||
		p.Dir != filepath.Join(jd, "work") {
		t.Fatalf("command: %s %v in %s", p.App, p.Args, p.Dir)
	}
	if string(p.Stdin) != jit {
		t.Fatal("the bundle must arrive on stdin")
	}
	if _, err := os.Stat(filepath.Join(jd, "runner")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("check jobs need no runner copy")
	}
	if _, err := os.Stat(filepath.Join(jd, "out")); err != nil {
		t.Fatal(err)
	}
	// Setup actions find toolchains in <state>\tools, the folder the docs name.
	if got := envMap(p.Env)["TENTMAKER_TOOLS"]; got != v.e.toolsDir() || got != filepath.Join(v.e.o.StateDir, "tools") {
		t.Fatalf("TENTMAKER_TOOLS = %q, want %q", got, filepath.Join(v.e.o.StateDir, "tools"))
	}
	v.assertNoSecret()
}

// Install copies the tentmaker binary for check jobs once. After an update
// the copy is stale, so Start replaces it when it differs, even while an
// older check still runs the old one.
func TestStartCheckRefreshesGuestBinary(t *testing.T) {
	v := newEnv(t)
	v.install()
	bin, _ := v.o.Binary(core.Platform{})
	guest := v.e.guestBinary()
	if b, _ := os.ReadFile(guest); string(b) != "guest-binary" {
		t.Fatalf("install copied %q", b)
	}
	must(t, os.WriteFile(bin, []byte("guest-binary v2, newer"), 0o755))
	must(t, v.e.Start(context.Background(), spec("check-1", core.KindCheck)))
	if b, _ := os.ReadFile(guest); string(b) != "guest-binary v2, newer" {
		t.Fatalf("guest binary is %q", b)
	}
	st1, err := os.Stat(guest)
	must(t, err)
	// Unchanged: left alone.
	must(t, v.e.Start(context.Background(), spec("check-2", core.KindCheck)))
	st2, err := os.Stat(guest)
	must(t, err)
	if !os.SameFile(st1, st2) {
		t.Fatal("an up-to-date guest binary was replaced")
	}
	// Nothing but the binary is left in the folder.
	entries, _ := os.ReadDir(filepath.Dir(guest))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(guest)+".") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestStartRefuses(t *testing.T) {
	v := newEnv(t)
	v.install()
	ctx := context.Background()
	var bad []core.ExecSpec
	for _, n := range []string{"..", "../evil", `a\b\c`, "AB-upper", "ab", "x/../../y", "-lead"} {
		bad = append(bad, spec(n, ""))
	}
	s := spec("job-ok-1", "")
	s.Platform = core.Platform{OS: "linux", Arch: "x64"}
	bad = append(bad, s)
	s = spec("job-ok-1", "")
	s.Platform.Arch = "arm64"
	bad = append(bad, s)
	s = spec("job-ok-1", "")
	s.Grant.Secret = nil
	bad = append(bad, s)
	s = spec("job-ok-1", "")
	s.Limits = core.Resources{}
	bad = append(bad, s)
	bad = append(bad, spec("job-ok-1", "vm"))
	for _, s := range bad {
		err := v.e.Start(ctx, s)
		if err == nil {
			t.Fatalf("started %q %s %s", s.Name, s.Platform, s.Kind)
		}
		v.assertNoSecret(err.Error())
	}
	if len(v.api.procs) != 0 {
		t.Fatal("a refused job reached StartProcess")
	}
	if ents, _ := os.ReadDir(v.e.jobsDir()); len(ents) != 0 {
		t.Fatal("a refused job left a directory")
	}

	// Without install there is no user, so nothing starts.
	w := newEnv(t)
	if err := w.e.Start(ctx, spec("job-ok-1", "")); err == nil {
		t.Fatal("started without install")
	}
}

func TestStartFailureCleansUp(t *testing.T) {
	v := newEnv(t)
	v.install()
	v.api.startErr = fmt.Errorf("CreateProcessAsUser failed near %s and %s", jit, v.api.password)
	err := v.e.Start(context.Background(), spec("job-fail-1", ""))
	if err == nil {
		t.Fatal("no error")
	}
	if _, serr := os.Stat(v.e.jobDir("job-fail-1")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatal("job dir left behind")
	}
	v.api.startErr = nil
	v.assertNoSecret(err.Error())
}

func waitState(t *testing.T, e *Executor, name string, want core.ExecState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := e.State(context.Background(), name)
		if st == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: state %s, want %s", name, st, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLifecycle(t *testing.T) {
	v := newEnv(t)
	v.install()
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("job-life-1", "")))
	if err := v.e.Clean(ctx, "job-life-1"); err == nil {
		t.Fatal("cleaned a running job")
	}
	if _, err := v.e.CheckResult(ctx, "job-life-1"); err == nil {
		t.Fatal("read a running job's result")
	}
	must(t, v.e.Stop(ctx, "job-life-1"))
	if n := v.api.proc(0).terminations(); n != 1 {
		t.Fatalf("terminated %d times", n)
	}
	waitState(t, v.e, "job-life-1", core.ExecExited)
	must(t, v.e.Clean(ctx, "job-life-1"))
	if st, _ := v.e.State(ctx, "job-life-1"); st != core.ExecMissing {
		t.Fatalf("after clean: %s", st)
	}
	must(t, v.e.Stop(ctx, "job-life-1"))

	// MaxDuration ends the job.
	s := spec("job-slow-1", "")
	s.MaxDuration = 20 * time.Millisecond
	must(t, v.e.Start(ctx, s))
	waitState(t, v.e, "job-slow-1", core.ExecExited)
	if v.api.proc(1).terminations() == 0 {
		t.Fatal("the time limit did not terminate the job")
	}

	// A job that ends on its own.
	must(t, v.e.Start(ctx, spec("job-quick-1", "")))
	v.api.proc(2).exit(0)
	waitState(t, v.e, "job-quick-1", core.ExecExited)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(v.logs.String(), "job exited") {
		if time.Now().After(deadline) {
			t.Fatal("exit not logged")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// After a daemon restart a leftover directory reads as exited.
	e2 := New(v.o)
	if st, _ := e2.State(ctx, "job-quick-1"); st != core.ExecExited {
		t.Fatalf("leftover state %s", st)
	}
	if st, err := e2.State(ctx, "../job"); err == nil || st != core.ExecUnknown {
		t.Fatal("State took a bad name")
	}
	v.assertNoSecret()
}

func TestListAndReset(t *testing.T) {
	v := newEnv(t)
	v.install()
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("job-a-1", "")))
	must(t, v.e.Start(ctx, spec("job-b-1", core.KindCheck)))
	must(t, os.Mkdir(filepath.Join(v.e.jobsDir(), "Not_A_Job"), 0o700))
	names, err := v.e.List(ctx)
	must(t, err)
	if !slices.Equal(names, []string{"job-a-1", "job-b-1"}) {
		t.Fatalf("List = %v", names)
	}
	if err := v.e.Reset(ctx); err == nil {
		t.Fatal("reset with jobs running")
	}
	if v.api.has("KillUserProcesses") {
		t.Fatal("reset touched processes while jobs run")
	}
	v.api.proc(0).exit(0)
	v.api.proc(1).exit(0)
	waitState(t, v.e, "job-a-1", core.ExecExited)
	waitState(t, v.e, "job-b-1", core.ExecExited)
	v.api.mu.Lock()
	v.api.killed, v.api.tasks = 2, 1
	v.api.mu.Unlock()
	must(t, v.e.Reset(ctx))
	if !v.api.has("KillUserProcesses "+testSID) || !v.api.has("DeleteUserTasks "+User+" "+testSID) {
		t.Fatalf("reset skipped the sweep: %v", v.api.calls)
	}
	if ents, _ := os.ReadDir(v.e.jobsDir()); len(ents) != 0 {
		t.Fatalf("reset left %d entries", len(ents))
	}
	if names, _ := v.e.List(ctx); len(names) != 0 {
		t.Fatalf("List after reset = %v", names)
	}
	if !strings.Contains(v.logs.String(), "outside its job object") {
		t.Fatal("stray processes not logged")
	}
	must(t, v.e.SetPoolLimit(ctx, core.Resources{}, core.Resources{}))
}

func TestCheckResult(t *testing.T) {
	v := newEnv(t)
	v.install()
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("check-r-1", core.KindCheck)))
	v.api.proc(0).exit(0)
	waitState(t, v.e, "check-r-1", core.ExecExited)
	out := filepath.Join(v.e.jobDir("check-r-1"), "out")
	r, err := v.e.CheckResult(ctx, "check-r-1")
	must(t, err)
	if r.State != "error" || !strings.Contains(r.Description, "no result.json") {
		t.Fatalf("missing result: %+v", r)
	}
	must(t, os.WriteFile(filepath.Join(out, "log.txt"), []byte(strings.Repeat("x", LogTailBytes+10)+"END"), 0o600))
	must(t, os.WriteFile(filepath.Join(out, "result.json"), []byte(`{"state":"success","description":"ok","runner":"spoof"}`), 0o600))
	r, err = v.e.CheckResult(ctx, "check-r-1")
	must(t, err)
	if r.State != "success" || r.Runner != "check-r-1" || len(r.LogTail) != LogTailBytes || !strings.HasSuffix(r.LogTail, "END") {
		t.Fatalf("result: state %s runner %s tail %d", r.State, r.Runner, len(r.LogTail))
	}
	must(t, os.WriteFile(filepath.Join(out, "result.json"), []byte(`{"state":"passed"}`), 0o600))
	if r, _ := v.e.CheckResult(ctx, "check-r-1"); r.State != "error" {
		t.Fatalf("bad state accepted: %+v", r)
	}
}

func TestUninstall(t *testing.T) {
	v := newEnv(t)
	v.install()
	ctx := context.Background()
	must(t, Uninstall(ctx, v.o, false))
	if _, ok := v.api.users[User]; !ok {
		t.Fatal("uninstall without purge deleted the user")
	}
	for _, d := range []string{v.e.jobsDir(), v.e.libDir(), v.e.runnerRoot()} {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s kept", d)
		}
	}
	if _, err := os.Stat(v.e.passwordPath()); err != nil {
		t.Fatal("uninstall without purge removed the password")
	}
	must(t, Uninstall(ctx, v.o, true))
	if _, ok := v.api.users[User]; ok {
		t.Fatal("purge kept the user")
	}
	if len(v.api.rights[testSID]) != 0 || v.api.hidden[User] {
		t.Fatal("purge kept rights or the sign-in screen entry")
	}
	for _, d := range []string{v.e.secretDir(), v.e.distDir()} {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s kept", d)
		}
	}
	must(t, Uninstall(ctx, v.o, true)) // idempotent
}

func TestUnzipRefuses(t *testing.T) {
	dir := t.TempDir()
	for name, entry := range map[string]string{
		"slip":      "../evil.txt",
		"abs":       "/etc/evil",
		"backslash": `..\evil.txt`,
		"deep":      "a/../../evil",
	} {
		src := filepath.Join(dir, name+".zip")
		must(t, os.WriteFile(src, makeZip(t, map[string]string{entry: "x"}), 0o644))
		if err := unzip(src, filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s: extracted %q", name, entry)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Fatal("zip slip wrote outside the target")
	}

	var b bytes.Buffer
	w := zip.NewWriter(&b)
	h := &zip.FileHeader{Name: "link"}
	h.SetMode(os.ModeSymlink | 0o777)
	f, err := w.CreateHeader(h)
	must(t, err)
	f.Write([]byte("/etc/passwd"))
	must(t, w.Close())
	src := filepath.Join(dir, "link.zip")
	must(t, os.WriteFile(src, b.Bytes(), 0o644))
	if err := unzip(src, filepath.Join(dir, "link")); err == nil {
		t.Fatal("extracted a symlink")
	}
}

func TestCPURate(t *testing.T) {
	for _, c := range []struct {
		cpus float64
		host int
		want uint32
	}{{2, 8, 2500}, {1, 3, 3334}, {16, 8, 10000}, {0.0001, 64, 1}, {1, 0, 10000}} {
		if got := cpuRate(c.cpus, c.host); got != c.want {
			t.Fatalf("cpuRate(%v, %d) = %d, want %d", c.cpus, c.host, got, c.want)
		}
	}
}

func TestTaskRunsAs(t *testing.T) {
	utf16le := func(s string) []byte {
		b := []byte{0xFF, 0xFE}
		for _, r := range s {
			b = append(b, byte(r), byte(r>>8))
		}
		return b
	}
	x := `<?xml version="1.0" encoding="UTF-16"?><Task><Principals><Principal id="Author">` +
		`<UserId>HOST\tentmaker-runner</UserId><LogonType>S4U</LogonType></Principal></Principals></Task>`
	if !taskRunsAs(decodeTaskXML(utf16le(x)), User, testSID) {
		t.Fatal(`missed COMPUTER\name in UTF-16`)
	}
	if !taskRunsAs("<UserId> "+testSID+" </UserId>", User, testSID) {
		t.Fatal("missed the SID")
	}
	for _, other := range []string{`<UserId>HOST\not-tentmaker-runner</UserId>`, "<UserId>S-1-5-18</UserId>", "<Author>tentmaker-runner</Author>"} {
		if taskRunsAs(other, User, testSID) {
			t.Fatalf("matched %s", other)
		}
	}
}

func TestEnvBlock(t *testing.T) {
	b, err := envBlock([]string{"b=2", "A=1", "Path=x"})
	must(t, err)
	var s []rune
	for _, u := range b {
		s = append(s, rune(u))
	}
	if got := string(s); got != "A=1\x00b=2\x00Path=x\x00\x00" {
		t.Fatalf("block %q", got)
	}
	if _, err := envBlock([]string{"A=\x00"}); err == nil {
		t.Fatal("NUL accepted")
	}
	if b, _ := envBlock(nil); len(b) != 2 {
		t.Fatal("an empty block must be two NULs")
	}
}

func TestPlatformsAndPins(t *testing.T) {
	v := newEnv(t)
	if p := v.e.Platforms(); len(p) != 1 || p[0] != (core.Platform{OS: "windows", Arch: "x64"}) {
		t.Fatalf("platforms %v", p)
	}
	if v.e.Name() != "windows-job" || v.e.MaxSlots() != 0 {
		t.Fatal("name or slots")
	}
	for _, a := range []string{"x64", "arm64"} {
		p, ok := PinFor(a)
		if !ok || len(p.SHA256) != 64 || !strings.HasSuffix(p.URL(), "actions-runner-win-"+a+"-"+RunnerVersion+".zip") {
			t.Fatalf("pin %s: %+v", a, p)
		}
	}
	if _, ok := PinFor("x86"); ok {
		t.Fatal("x86 pinned")
	}
	w := New(Options{Arch: "x86", API: newFakeAPI(), Getenv: func(string) string { return "" }})
	if len(w.Platforms()) != 0 {
		t.Fatal("an unsupported arch offered a platform")
	}
	if w.o.StateDir != filepath.Join(`C:\ProgramData`, "Tentmaker") {
		t.Fatalf("default StateDir %s", w.o.StateDir)
	}
}

func TestStartingJobIsProtected(t *testing.T) {
	v := newEnv(t)
	v.install()
	ctx := context.Background()
	name := "job-mid-1"
	v.api.onStart = func() {
		if st, _ := v.e.State(ctx, name); st != core.ExecRunning {
			t.Errorf("a starting job reads %s", st)
		}
		if err := v.e.Clean(ctx, name); err == nil {
			t.Error("cleaned a starting job")
		}
		if err := v.e.Stop(ctx, name); err == nil {
			t.Error("Stop on a starting job claimed success")
		}
		if err := v.e.Reset(ctx); err == nil {
			t.Error("reset while a job starts")
		}
		if err := v.e.Start(ctx, spec(name, "")); err == nil {
			t.Error("started the same name while it was starting")
		}
	}
	must(t, v.e.Start(ctx, spec(name, "")))
	v.api.onStart = nil
	if _, err := os.Stat(filepath.Join(v.e.jobDir(name), "runner", "run.cmd")); err != nil {
		t.Fatal("the starting job's files were removed")
	}

	// A failed start frees the name.
	v.api.startErr = errors.New("boom")
	if err := v.e.Start(ctx, spec("job-mid-2", "")); err == nil {
		t.Fatal("no error")
	}
	v.api.startErr = nil
	must(t, v.e.Start(ctx, spec("job-mid-2", "")))
}
