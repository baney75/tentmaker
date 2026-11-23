package cli

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
	"github.com/baney75/tentmaker/internal/sandbox"
)

// sandboxDaemon is fakeDaemon with sandboxes served by a real Manager over
// a real Worker and the fixture's fake executor.
type sandboxDaemon struct {
	*fakeDaemon
	m *sandbox.Manager
}

func (d sandboxDaemon) SandboxService() control.SandboxService { return d.m }

type admitAll struct{}

func (admitAll) Decide(ctx context.Context, now time.Time, running int) core.Decision {
	return core.Decision{Profile: "everyday", Slots: 4, Admit: running < 4, Limit: core.Resources{MemoryBytes: 8 << 30, CPUs: 8}}
}

type sandboxRig struct {
	f      *fixture
	w      *dispatch.Worker
	caller control.Caller
	users  []string
}

// serveSandboxes starts the control API with sandboxes, as caller.
func serveSandboxes(t *testing.T, f *fixture, caller control.Caller) *sandboxRig {
	t.Helper()
	f.run("", "setup", "--yes", "--memory", "8GiB")
	cfg := f.config()
	w := dispatch.NewWorker(dispatch.WorkerOptions{Config: cfg, Device: "box", Executors: []core.Executor{f.ex},
		Budget: admitAll{}, Sources: func() []core.WorkSource { return nil }, StateDir: t.TempDir()})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &sandboxRig{f: f, w: w, caller: caller}
	m := sandbox.New(sandbox.Options{
		Worker:   func() sandbox.Worker { return w },
		Device:   func() string { return "box" },
		Users:    func() []string { return r.users },
		LookupID: func(name string) (string, error) { return map[string]string{"alice": "1000"}[name], nil },
	})
	if err := os.MkdirAll(f.env.Paths.State, 0o755); err != nil {
		t.Fatal(err)
	}
	tok, err := control.NewToken(control.TokenPath(f.env.Paths.State))
	if err != nil {
		t.Fatal(err)
	}
	ctl := control.NewServer(sandboxDaemon{f.daemon, m}, tok, func(net.Conn) (control.Caller, error) { return r.caller, nil }, nil)
	srv := httptest.NewUnstartedServer(ctl)
	srv.Config.ConnContext = ctl.ConnContext
	// The caller is named once per connection; the tests change it
	// between calls, so every call gets a new one.
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	t.Cleanup(srv.Close)
	f.env.Control.Addr = strings.TrimPrefix(srv.URL, "http://")
	return r
}

func TestSandboxCLI(t *testing.T) {
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0", Who: "uid 0"})

	args := []string{"sandbox", "create", "--ttl", "10m", "--json"}
	if code := f.run("", args...); code != ExitOK {
		t.Fatalf("create exit %d: %s %s", code, f.stdout, f.stderr)
	}
	m := f.jsonObject(args)
	id, _ := m["id"].(string)
	if !sandbox.ValidID(id) || m["platform"] != "linux/x64" || m["state"] != "running" || m["device"] != "box" {
		t.Fatalf("create answered %v", m)
	}
	if f.ex.Runs()[id].Spec.Kind != core.KindSandbox {
		t.Fatal("the executor did not start a sandbox")
	}

	// exec streams output and exits with the command's code.
	if code := f.run("", "sandbox", "exec", id, "--", "echo", "hello", "world"); code != 0 || f.stdout.String() != "hello world\n" {
		t.Fatalf("exec: exit %d stdout %q stderr %q", code, f.stdout, f.stderr)
	}
	if code := f.run("", "sandbox", "exec", id, "--", "sh", "-c", "exit 3"); code != 3 {
		t.Fatalf("exec exit %d, want 3: %s", code, f.stderr)
	}
	if code := f.run("", "sandbox", "exec", id, "--", "nope"); code != 127 || !strings.Contains(f.stderr.String(), "command not found") {
		t.Fatalf("exec of a missing command: exit %d stderr %q", code, f.stderr)
	}
	args = []string{"sandbox", "exec", "--json", id, "--", "sh", "-c", "exit 5"}
	if code := f.run("", args...); code != 5 {
		t.Fatalf("exec --json exit %d", code)
	}
	if m := f.jsonObject(args); m["exit_code"] != 5.0 || m["id"] != id {
		t.Fatalf("exec --json answered %v", m)
	}
	// --json after -- belongs to the command.
	if code := f.run("", "sandbox", "exec", id, "--", "echo", "--json"); code != 0 || f.stdout.String() != "--json\n" {
		t.Fatalf("exec echo --json: %d %q", code, f.stdout)
	}
	local := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(local, []byte("from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// cp in, then read it back with exec and cp out.
	if code := f.run("", "sandbox", "cp", local, id+":inbox"); code != 0 {
		t.Fatalf("cp in: %d %s", code, f.stderr)
	}
	if got := f.ex.Files(id)["inbox/notes.txt"]; got != "from the host\n" {
		t.Fatalf("sandbox files %v", f.ex.Files(id))
	}
	if code := f.run("", "sandbox", "exec", id, "--", "cat", "inbox/notes.txt"); code != 0 || f.stdout.String() != "from the host\n" {
		t.Fatalf("cat: %d %q", code, f.stdout)
	}
	out := t.TempDir()
	args = []string{"sandbox", "cp", "--json", id + ":inbox", out}
	if code := f.run("", args...); code != 0 {
		t.Fatalf("cp out: %d %s", code, f.stdout)
	}
	if b, err := os.ReadFile(filepath.Join(out, "inbox", "notes.txt")); err != nil || string(b) != "from the host\n" {
		t.Fatalf("copied out %q %v", b, err)
	}
	if m := f.jsonObject(args); m["files"] != 1.0 {
		t.Fatalf("cp --json answered %v", m)
	}
	if code := f.run("", "sandbox", "cp", id+":missing", out); code != ExitFailed || !strings.Contains(f.stderr.String(), "Cannot stat") {
		t.Fatalf("cp of a missing path: %d %s", code, f.stderr)
	}

	args = []string{"sandbox", "ls", "--json"}
	f.run("", args...)
	if l, _ := f.jsonObject(args)["sandboxes"].([]any); len(l) != 1 {
		t.Fatalf("ls %s", f.stdout)
	}
	if code := f.run("", "sandbox", "ls"); code != 0 || !strings.Contains(f.stdout.String(), id) {
		t.Fatalf("ls: %s", f.stdout)
	}

	args = []string{"sandbox", "rm", "--json", id}
	if code := f.run("", args...); code != 0 {
		t.Fatalf("rm: %s", f.stdout)
	}
	if !called(f.ex, "Clean", id) {
		t.Fatal("rm did not clean the sandbox")
	}
	args = []string{"sandbox", "exec", "--json", id, "--", "true"}
	if code := f.run("", args...); code != ExitFailed {
		t.Fatalf("exec after rm exit %d", code)
	}
	if m := f.jsonObject(args); !strings.Contains(m["error"].(string), "no sandbox") || m["fix"] == "" {
		t.Fatalf("exec after rm answered %v", m)
	}
	if v := f.ex.Violations(); len(v) > 0 {
		t.Fatal(v)
	}
}

func called(ex *fakeexec.Executor, method, name string) bool {
	for _, c := range ex.Calls(method) {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestSandboxAccess(t *testing.T) {
	f := newFixture(t)
	r := serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	f.isRoot = false
	// The token is root's; an ordinary account can't read it. Point the
	// client at a token path that doesn't exist.
	f.env.Control.TokenPath = filepath.Join(t.TempDir(), "none")
	r.caller = control.Caller{ID: "1000", Who: "uid 1000"}

	args := []string{"sandbox", "create", "--json"}
	if code := f.run("", args...); code != ExitFailed {
		t.Fatalf("an account not in [sandbox] users created a sandbox: %d %s", code, f.stdout)
	}
	if m := f.jsonObject(args); !strings.Contains(m["fix"].(string), "[sandbox]") {
		t.Fatalf("refusal %v", m)
	}
	if len(f.ex.Calls("Start")) != 0 {
		t.Fatal("a refused caller started something")
	}

	// Listed by name (looked up to uid 1000), alice may.
	r.users = []string{"alice"}
	if code := f.run("", args...); code != 0 {
		t.Fatalf("alice: %d %s", code, f.stdout)
	}
	id := f.jsonObject(args)["id"].(string)
	if code := f.run("", "sandbox", "exec", id, "--", "echo", "mine"); code != 0 {
		t.Fatalf("alice's exec: %d %s", code, f.stderr)
	}

	// Another listed account can't see, use or remove alice's sandbox.
	r.users = []string{"alice", "1001"}
	r.caller = control.Caller{ID: "1001", Who: "uid 1001"}
	args = []string{"sandbox", "ls", "--json"}
	f.run("", args...)
	if l := f.jsonObject(args)["sandboxes"].([]any); len(l) != 0 {
		t.Fatalf("uid 1001 sees %v", l)
	}
	for _, a := range [][]string{
		{"sandbox", "exec", id, "--", "echo", "theirs"},
		{"sandbox", "rm", id},
		{"sandbox", "cp", id + ":.", t.TempDir()},
	} {
		if code := f.run("", a...); code != ExitFailed || !strings.Contains(f.stderr.String(), "no sandbox "+id) {
			t.Fatalf("%v by another account: %d %s", a, code, f.stderr)
		}
	}
	if len(f.ex.Execs(id)) != 1 || called(f.ex, "Stop", id) {
		t.Fatalf("another account reached the sandbox: execs %v", f.ex.Execs(id))
	}

	// An admin sees every sandbox.
	r.caller = control.Caller{Admin: true, ID: "0"}
	args = []string{"sandbox", "ls", "--json"}
	f.run("", args...)
	if l := f.jsonObject(args)["sandboxes"].([]any); len(l) != 1 {
		t.Fatalf("admin sees %v", l)
	}
}

func TestSandboxCreateCopiesTheWorkTree(t *testing.T) {
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main // edited, not committed\n")
	write("pkg/new.go", "package pkg // untracked\n")
	write("build/out.bin", "ignored")
	write(".git/config", "[core]\n")
	write("gone.go", "")
	os.Remove(filepath.Join(dir, "gone.go"))
	var listed string
	f.env.GitLsFiles = func(ctx context.Context, d string) ([]byte, error) {
		listed = d
		// What git ls-files -z --cached --others --exclude-standard says:
		// a tracked file deleted on disk is still listed; ignored files
		// and .git are not.
		return []byte("main.go\x00gone.go\x00pkg/new.go\x00"), nil
	}
	args := []string{"sandbox", "create", "--copy", dir, "--json"}
	if code := f.run("", args...); code != 0 {
		t.Fatalf("create --copy: %d %s %s", code, f.stdout, f.stderr)
	}
	m := f.jsonObject(args)
	if listed != dir {
		t.Fatalf("git listed %q, want %q", listed, dir)
	}
	files := f.ex.Files(m["id"].(string))
	if len(files) != 2 || files["main.go"] != "package main // edited, not committed\n" || files["pkg/new.go"] == "" {
		t.Fatalf("sandbox got %v", files)
	}
	if c, _ := m["copied"].(map[string]any); c["files"] != 2.0 {
		t.Fatalf("create answered %v", m)
	}

	// Outside a Git work tree the whole folder goes.
	plain := t.TempDir()
	os.WriteFile(filepath.Join(plain, "a.txt"), []byte("a"), 0o644)
	if code := f.run("", "sandbox", "create", "--copy", plain, "--json"); code != 0 {
		t.Fatalf("create --copy plain: %s", f.stdout)
	}
	if files := f.ex.Files(f.jsonObject(nil)["id"].(string)); files["a.txt"] != "a" {
		t.Fatalf("plain copy got %v", files)
	}

	// A copy that fails removes the sandbox it made.
	f.env.GitLsFiles = func(ctx context.Context, d string) ([]byte, error) { return nil, os.ErrPermission }
	before := len(f.ex.Calls("Start"))
	if code := f.run("", "sandbox", "create", "--copy", dir); code != ExitFailed {
		t.Fatalf("create with a failing copy: %d", code)
	}
	starts := f.ex.Calls("Start")
	if len(starts) != before+1 || !called(f.ex, "Clean", starts[len(starts)-1].Name) {
		t.Fatal("the sandbox of a failed copy was left behind")
	}
}

func TestSandboxExecStdin(t *testing.T) {
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	args := []string{"sandbox", "create", "--json"}
	f.run("", args...)
	id := f.jsonObject(args)["id"].(string)
	var got []string
	f.ex.SetExec(func(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		b, _ := io.ReadAll(stdin)
		got = append(got, string(b))
		io.WriteString(stdout, strings.ToUpper(string(b)))
		io.WriteString(stderr, "dir="+req.Dir)
		return 0, nil
	})
	if code := f.run("piped in", "sandbox", "exec", "-i", "--dir", "src", id, "--", "tr", "a-z", "A-Z"); code != 0 {
		t.Fatalf("exec -i: %d %s", code, f.stderr)
	}
	if f.stdout.String() != "PIPED IN" || f.stderr.String() != "dir=src" {
		t.Fatalf("stdout %q stderr %q", f.stdout, f.stderr)
	}
	// Without -i the command's stdin is empty.
	f.run("piped in", "sandbox", "exec", id, "--", "cat")
	if got[1] != "" {
		t.Fatalf("stdin without -i: %q", got[1])
	}
}
