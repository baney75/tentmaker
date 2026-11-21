package service

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// abs makes a slash path absolute on this OS (a drive letter on Windows).
func abs(p string) string {
	if runtime.GOOS == "windows" {
		return "C:" + filepath.FromSlash(p)
	}
	return p
}

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestSystemdUnitGolden(t *testing.T) {
	golden(t, "tentmaker.service", SystemdUnit(Spec{Binary: "/usr/local/bin/tentmaker"}))
	golden(t, "tentmaker-env.service", SystemdUnit(Spec{
		Binary: "/opt/my tools/tentmaker",
		Env:    map[string]string{"TENTMAKER_CONFIG": "/srv/tm/config.toml"},
	}))
}

func TestLaunchdPlistGolden(t *testing.T) {
	golden(t, "dev.tentmaker.daemon.plist", LaunchdPlist(Spec{Binary: "/usr/local/bin/tentmaker"}))
	golden(t, "dev.tentmaker.daemon-env.plist", LaunchdPlist(Spec{
		Binary:  "/Users/a&b/tentmaker",
		Env:     map[string]string{"TENTMAKER_CONFIG": "/tmp/<x>.toml"},
		LogPath: "/tmp/tm.log",
	}))
}

func TestUnitHasRequiredSettings(t *testing.T) {
	u := SystemdUnit(Spec{Binary: "/usr/local/bin/tentmaker"})
	for _, want := range []string{
		"Restart=always", "RestartSec=5", "After=network-online.target tailscaled.service",
		"Wants=network-online.target", "KillMode=process", "TimeoutStopSec=30",
		"ExecStart=/usr/local/bin/tentmaker daemon",
	} {
		if !strings.Contains(u, want+"\n") {
			t.Errorf("unit lacks %q", want)
		}
	}
	p := LaunchdPlist(Spec{Binary: "/usr/local/bin/tentmaker"})
	for _, want := range []string{"<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<true/>", MacLogPath, Label} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	for _, s := range []Spec{
		{Binary: "tentmaker"},
		{Binary: "/bin/tm\nExecStartPre=/bin/evil"},
		{Binary: "/bin/tm", Env: map[string]string{"A=B": "c"}},
		{Binary: "/bin/tm", Env: map[string]string{"A": "c\nd"}},
	} {
		if err := s.validate(); err == nil {
			t.Errorf("%+v: no error", s)
		}
	}
}

// fakeRun records commands and answers from a table.
type fakeRun struct {
	mu    sync.Mutex
	calls []string
	out   map[string]string
	fail  map[string]error
}

func (f *fakeRun) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("no deadline")
	}
	c := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, c)
	return []byte(f.out[c]), f.fail[c]
}

func (f *fakeRun) joined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func TestSystemdFlow(t *testing.T) {
	root := t.TempDir()
	f := &fakeRun{out: map[string]string{"systemctl is-active tentmaker.service": "active\n"}}
	m := New(Options{GOOS: "linux", Run: f.run, Root: root})
	ctx := context.Background()
	if err := m.Install(ctx, Spec{Binary: abs("/usr/local/bin/tentmaker")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "etc/systemd/system/tentmaker.service"))
	if err != nil || !strings.Contains(string(b), "KillMode=process") {
		t.Fatalf("unit not written: %v", err)
	}
	if got, want := f.joined(), "systemctl daemon-reload\nsystemctl enable tentmaker.service\nsystemctl restart tentmaker.service"; got != want {
		t.Errorf("calls:\n%s\nwant\n%s", got, want)
	}
	st, err := m.Status(ctx)
	if err != nil || !st.Installed || !st.Running {
		t.Errorf("status %+v %v", st, err)
	}
	f.out["systemctl is-active tentmaker.service"] = "inactive\n"
	f.fail = map[string]error{"systemctl is-active tentmaker.service": errors.New("exit 3")}
	st, err = m.Status(ctx)
	if err != nil || st.Running || st.Detail != "inactive" {
		t.Errorf("inactive status %+v %v", st, err)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/systemd/system/tentmaker.service")); !os.IsNotExist(err) {
		t.Error("unit still there")
	}
	if !strings.Contains(f.joined(), "systemctl disable --now tentmaker.service") {
		t.Error("not disabled")
	}
}

func TestLaunchdFlow(t *testing.T) {
	root := t.TempDir()
	f := &fakeRun{
		out:  map[string]string{"launchctl print system/dev.tentmaker.daemon": "{\n\tstate = running\n}"},
		fail: map[string]error{"launchctl print system/dev.tentmaker.daemon": errors.New("exit 113")},
	}
	m := New(Options{GOOS: "darwin", Run: f.run, Root: root})
	ctx := context.Background()
	if err := m.Install(ctx, Spec{Binary: abs("/usr/local/bin/tentmaker")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Library/LaunchDaemons/dev.tentmaker.daemon.plist")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Library/Logs/Tentmaker")); err != nil {
		t.Fatal("log dir not made")
	}
	want := "launchctl print system/dev.tentmaker.daemon\nlaunchctl enable system/dev.tentmaker.daemon\n" +
		"launchctl bootstrap system /Library/LaunchDaemons/dev.tentmaker.daemon.plist\nlaunchctl kickstart system/dev.tentmaker.daemon"
	if got := f.joined(); got != want {
		t.Errorf("calls:\n%s\nwant\n%s", got, want)
	}
	st, _ := m.Status(ctx)
	if st.Running || !st.Installed {
		t.Errorf("not loaded status %+v", st)
	}
	delete(f.fail, "launchctl print system/dev.tentmaker.daemon")
	st, _ = m.Status(ctx)
	if !st.Running {
		t.Errorf("loaded status %+v", st)
	}
	f.calls = nil
	if err := m.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.joined(), "launchctl kickstart -k system/dev.tentmaker.daemon") {
		t.Error("restart did not kickstart -k")
	}
	f.calls = nil
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.joined(), "launchctl bootout system/dev.tentmaker.daemon") {
		t.Error("uninstall did not bootout")
	}
}

type fakeSCM struct {
	calls []string
	st    State
}

func (f *fakeSCM) Install(name, display, desc, exe string, args []string) error {
	f.calls = append(f.calls, "install "+name+" "+exe+" "+strings.Join(args, " "))
	f.st.Installed = true
	return nil
}
func (f *fakeSCM) Remove(name string) error {
	f.calls = append(f.calls, "remove")
	f.st = State{}
	return nil
}
func (f *fakeSCM) Start(name string) error {
	f.calls = append(f.calls, "start")
	f.st.Running = true
	return nil
}
func (f *fakeSCM) Stop(name string) error {
	f.calls = append(f.calls, "stop")
	f.st.Running = false
	return nil
}
func (f *fakeSCM) Query(name string) (State, error) {
	return f.st, nil
}

func TestWindowsFlow(t *testing.T) {
	f := &fakeSCM{}
	m := New(Options{GOOS: "windows", SCM: f})
	ctx := context.Background()
	if err := m.Install(ctx, Spec{Binary: abs("/tm/tentmaker.exe")}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, ","); !strings.HasPrefix(got, "install Tentmaker ") || !strings.HasSuffix(got, "daemon,stop,start") {
		t.Errorf("calls %s", got)
	}
	if st, _ := m.Status(ctx); !st.Running || !st.Installed {
		t.Errorf("status %+v", st)
	}
	if err := m.Install(ctx, Spec{Binary: abs("/x/tm.exe"), Env: map[string]string{"A": "b"}}); err == nil {
		t.Error("env accepted on Windows")
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupported(t *testing.T) {
	if err := New(Options{GOOS: "plan9"}).Install(context.Background(), Spec{Binary: abs("/x")}); err == nil {
		t.Error("no error on plan9")
	}
}

func TestExecRunnerNeedsSystemPath(t *testing.T) {
	if _, err := ExecRunner(context.Background(), "tentmaker-no-such-tool"); err == nil {
		t.Error("found a tool that doesn't exist")
	}
}
