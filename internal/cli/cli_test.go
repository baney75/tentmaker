package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/daemon"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

// TestSetupYesJSON is setup end to end against fakes: saved config, the
// stored secret's name, the service spec, and the piped credential in no
// output.
func TestSetupYesJSON(t *testing.T) {
	f := newFixture(t)
	f.serveControl()
	args := []string{"setup", "--yes", "--json", "--token-stdin", "--repo", "baney75/app", "--memory", "4GiB", "--boost-memory", "12GiB", "--pool", "home"}
	code := f.run(canary+"\n", args...)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
	out := f.jsonObject(args)
	if out["secret"] != "github-token" || out["login"] != "octo" || out["device"] != "test-box" || out["ok"] != true {
		t.Errorf("output %v", out)
	}
	var steps []string
	for _, s := range out["steps"].([]any) {
		m := s.(map[string]any)
		steps = append(steps, m["step"].(string))
		if m["status"] == "fail" {
			t.Errorf("step failed: %v", m)
		}
	}
	want := []string{"check", "wizard", "forge", "save", "sandbox", "service", "doctor"}
	if !reflect.DeepEqual(steps, want) {
		t.Errorf("steps %v, want %v", steps, want)
	}
	if f.stderr.Len() != 0 {
		t.Errorf("stderr in --json mode: %s", f.stderr)
	}

	cfg := f.config()
	if cfg.Device != "test-box" || cfg.Budget.Everyday.Memory != 4*config.GiB || cfg.Budget.Boost.Memory != 12*config.GiB {
		t.Errorf("config %+v", cfg)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "baney75/app" || cfg.Repos[0].Mode != "runner" {
		t.Errorf("repos %+v", cfg.Repos)
	}
	if cfg.Mesh.Pool != "home" || !cfg.Mesh.Enabled {
		t.Errorf("mesh %+v", cfg.Mesh)
	}
	if cfg.Update.Channel != "stable" {
		t.Errorf("channel %q", cfg.Update.Channel)
	}
	if b, _ := f.store.Get("github-token"); string(b) != canary {
		t.Error("the credential was not stored as github-token")
	}
	if len(f.verified) != 1 || f.verified[0] != canary {
		t.Errorf("verified %d credentials", len(f.verified))
	}
	raw, _ := os.ReadFile(f.env.ConfigPath)
	if strings.Contains(string(raw), canary) {
		t.Error("the credential is in the config file")
	}
	if f.svc.spec == nil {
		t.Fatal("service not installed")
	}
	exe, _ := f.env.Executable()
	if f.svc.spec.Binary != exe || !reflect.DeepEqual(f.svc.spec.Args, []string{"daemon"}) || f.svc.spec.LogPath != f.env.Paths.Log {
		t.Errorf("service spec %+v", *f.svc.spec)
	}
	if f.execInst != 1 {
		t.Errorf("executors installed %d times", f.execInst)
	}
	if out["service"] != "running" {
		t.Errorf("service %v", out["service"])
	}
}

// TestSetupRerunKeepsValues: a second setup changes only what it names.
func TestSetupRerunKeepsValues(t *testing.T) {
	f := newFixture(t)
	if code := f.run(canary, "setup", "--yes", "--no-service", "--token-stdin", "--device", "box1", "--memory", "3GiB", "--repo", "a/b", "--no-mesh", "--channel", "dev"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	code := f.run("", "setup", "--yes", "--no-service", "--boost-memory", "10GiB", "--json")
	if code != ExitOK {
		t.Fatalf("rerun exit %d: %s", code, f.stdout)
	}
	out := f.jsonObject(nil)
	if out["secret"] != "github-token" {
		t.Errorf("the stored credential was not kept: %v", out["secret"])
	}
	cfg := f.config()
	if cfg.Device != "box1" || cfg.Budget.Everyday.Memory != 3*config.GiB || cfg.Budget.Boost.Memory != 10*config.GiB {
		t.Errorf("budget/device not kept: %+v", cfg)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "a/b" || cfg.Mesh.Enabled || cfg.Update.Channel != "dev" {
		t.Errorf("repos/mesh/channel not kept: %+v %+v %+v", cfg.Repos, cfg.Mesh, cfg.Update)
	}
	if len(f.verified) != 1 {
		t.Errorf("rerun verified the credential again (%d)", len(f.verified))
	}
	if b, _ := f.store.Get("github-token"); string(b) != canary {
		t.Error("stored credential changed")
	}
}

func TestSetupFromGH(t *testing.T) {
	f := newFixture(t)
	f.ghValue = canary
	f.setUp("--repo", "a/b")
	if b, _ := f.store.Get("github-token"); string(b) != canary {
		t.Error("gh credential not stored")
	}
	f2 := newFixture(t)
	f2.ghValue = canary
	f2.setUp("--repo", "a/b", "--no-gh")
	if _, err := f2.store.Get("github-token"); err == nil {
		t.Error("--no-gh still used gh")
	}
}

func TestSetupForgejoNeedsURL(t *testing.T) {
	f := newFixture(t)
	code := f.run("", "setup", "--yes", "--forge", "forgejo", "--json")
	if code != ExitUsage || !strings.Contains(f.stdout.String(), "--forge-url") {
		t.Fatalf("exit %d %s", code, f.stdout)
	}
	code = f.run(canary, "setup", "--yes", "--no-service", "--forge", "forgejo", "--forge-url", "https://codeberg.org", "--repo", "o/r", "--token-stdin", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d %s", code, f.stdout)
	}
	if out := f.jsonObject(nil); out["secret"] != "forgejo-codeberg.org-token" {
		t.Errorf("secret %v", out["secret"])
	}
	if cfg := f.config(); cfg.Forges["forgejo"].URL != "https://codeberg.org" || cfg.Repos[0].Forge != "forgejo" {
		t.Errorf("config %+v %+v", cfg.Forges, cfg.Repos)
	}
}

func TestSetupNeverPromptsWithoutTTY(t *testing.T) {
	f := newFixture(t)
	code := f.run("", "setup")
	if code != ExitUsage || !strings.Contains(f.stderr.String(), "--yes") {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	if _, err := os.Stat(f.env.ConfigPath); err == nil {
		t.Error("config written")
	}
}

func TestSetupWizard(t *testing.T) {
	f := newFixture(t)
	f.env.StdinTTY = true
	f.env.ReadSecret = func() ([]byte, error) { return []byte(""), nil } // Enter: no credential
	// An origin in the current folder is offered as the default repo.
	os.MkdirAll(filepath.Join(f.root, ".git"), 0o755)
	os.WriteFile(filepath.Join(f.root, ".git", "config"), []byte("[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:baney75/site.git\n"), 0o644)
	answers := strings.Join([]string{"", "5GiB", "nonsense", "11GiB", "off", ""}, "\n") + "\n"
	code := f.run(answers, "setup", "--no-service", "--no-mesh")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
	cfg := f.config()
	if cfg.Device != "test-box" || cfg.Budget.Everyday.Memory != 5*config.GiB || cfg.Budget.Boost.Memory != 11*config.GiB || cfg.Budget.OnBattery != "off" {
		t.Errorf("config %+v", cfg.Budget)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "baney75/site" {
		t.Errorf("repos %+v", cfg.Repos)
	}
	if !strings.Contains(f.stderr.String(), "[8GiB]") && !strings.Contains(f.stderr.String(), "[") {
		t.Errorf("prompts show no defaults: %s", f.stderr)
	}
}

// mutating lists every command that changes the device, with args that
// pass flag checks.
var mutating = [][]string{
	{"setup", "--yes"},
	{"budget", "set", "--memory", "4GiB"},
	{"schedule", "add", "--name", "work", "--days", "mon-fri", "--from", "09:00", "--to", "17:00", "--profile", "everyday"},
	{"schedule", "remove", "work"},
	{"repo", "add", "a/b"},
	{"repo", "remove", "a/b"},
	{"connect", "github", "--token-stdin"},
	{"pause", "--for", "1h", "call"},
	{"resume"},
	{"update"},
	{"rollback"},
	{"uninstall", "--yes"},
}

func TestNonRootExits3WithRerunHint(t *testing.T) {
	for _, args := range mutating {
		f := newFixture(t)
		f.isRoot = false
		code := f.run(canary, args...)
		if code != ExitRoot {
			t.Errorf("%v: exit %d, want 3 (%s)", args, code, f.stderr)
			continue
		}
		hint := "sudo tentmaker " + strings.Join(args, " ")
		if !strings.Contains(f.stderr.String(), "fix: "+hint) {
			t.Errorf("%v: no rerun hint %q in %q", args, hint, f.stderr)
		}
		// JSON mode: the same hint inside the one error object.
		jargs := append(append([]string{}, args...), "--json")
		if code := f.run(canary, jargs...); code != ExitRoot {
			t.Errorf("%v: exit %d", jargs, code)
		}
		out := f.jsonObject(jargs)
		if fix, _ := out["fix"].(string); !strings.HasPrefix(fix, "sudo tentmaker ") || out["error"] == nil {
			t.Errorf("%v: %v", jargs, out)
		}
		if _, err := os.Stat(f.env.ConfigPath); err == nil {
			t.Errorf("%v wrote the config without root", args)
		}
	}
	f := newFixture(t)
	f.isRoot = false
	f.env.GOOS = "windows"
	f.run("", "resume")
	if !strings.Contains(f.stderr.String(), "PowerShell as Administrator") {
		t.Errorf("windows hint: %s", f.stderr)
	}
}

func TestReadOnlyCommandsNeedNoRoot(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	f.isRoot = false
	for _, args := range [][]string{{"status"}, {"pool"}, {"budget"}, {"repo", "list"}, {"schedule", "list"}, {"doctor"}, {"logs"}, {"version"}, {"update", "--check"}, {"migrate", f.root}} {
		code := f.run("", args...)
		if code == ExitRoot {
			t.Errorf("%v asked for root", args)
		}
	}
}

// usageCases are bad invocations of every command.
var usageCases = [][]string{
	{"nope"},
	{"setup", "--bogus"},
	{"setup", "--yes", "--memory", "lots"},
	{"setup", "--yes", "--on-battery", "sometimes"},
	{"setup", "--yes", "--forge", "svn"},
	{"setup", "--yes", "--repo", "notarepo"},
	{"setup", "--yes", "--channel", "nightly"},
	{"setup", "--yes", "--device", "Bad Name"},
	{"setup", "--yes", "--forge", "gitlab", "--mode", "runner"},
	{"setup", "extra"},
	{"status", "--bogus"},
	{"status", "--watch", "--json"},
	{"pool", "--bogus"},
	{"budget", "--bogus"},
	{"budget", "what"},
	{"budget", "set"},
	{"budget", "set", "--memory", "lots"},
	{"budget", "set", "--on-battery", "boost"},
	{"budget", "set", "--boost-needs-idle", "maybe"},
	{"budget", "set", "--pause-below", "120"},
	{"schedule"},
	{"schedule", "nope"},
	{"schedule", "add", "--name", "x"},
	{"schedule", "add", "--name", "x", "--days", "someday", "--from", "09:00", "--to", "10:00", "--profile", "boost"},
	{"schedule", "add", "--name", "x", "--from", "9am", "--to", "10:00", "--profile", "boost"},
	{"schedule", "add", "--name", "x", "--from", "09:00", "--to", "10:00", "--profile", "turbo"},
	{"schedule", "remove"},
	{"repo"},
	{"repo", "nope"},
	{"repo", "list", "extra"},
	{"repo", "add"},
	{"repo", "add", "bad"},
	{"repo", "add", "a/b", "--forge", "svn"},
	{"repo", "add", "a/b", "--bogus"},
	{"repo", "add", "a/b", "--branch", "e2e"},
	{"repo", "add", "a/b", "--mode", "checks", "--branch", "a..b"},
	{"repo", "remove"},
	{"connect"},
	{"connect", "svn"},
	{"connect", "github", "--url", "ftp://x"},
	{"devices", "bogus"},
	{"devices", "remove"},
	{"join", "tm1-secret"},
	{"invite", "--trust", "owner"},
	{"invite", "--ttl", "90d"},
	{"invite", "--trust", "public", "--repos", "a/b"},
	{"devices", "add"},
	{"devices", "add", "h", "--channel", "nightly"},
	{"devices", "add", "h", "--memory", "lots"},
	{"devices", "add", "h;rm", "--repo", "a/b"},
	{"migrate", "a", "b"},
	{"migrate", "--platform", "bsd", "."},
	{"pause", "--for", "soon"},
	{"resume", "extra"},
	{"doctor", "--bogus"},
	{"logs", "-n", "0"},
	{"logs", "-f", "--json"},
	{"logs", "a", "b"},
	{"logs", "../etc"},
	{"logs", "tm-a-1", "-n", "0"},
	{"runs", "extra"},
	{"runs", "-n", "0"},
	{"rerun"},
	{"rerun", "a", "b"},
	{"rerun", "https://github.com/o/r/pull/1"},
	{"stats", "--since", "forever"},
	{"stats", "extra"},
	{"alerts"},
	{"alerts", "send"},
	{"update", "--channel", "nightly"},
	{"update", "extra"},
	{"rollback", "extra"},
	{"uninstall", "extra"},
	{"uninstall"},
	{"mcp", "extra"},
	{"version", "extra"},
	{"daemon", "extra"},
	{"check-run"},
	{"check-run", "--out", "x", "extra"},
}

func TestUsageErrors(t *testing.T) {
	for _, args := range usageCases {
		f := newFixture(t)
		code := f.run("", args...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d, want 2 (stderr %q)", args, code, f.stderr)
			continue
		}
		if !wantsJSON(args) && !strings.HasPrefix(f.stderr.String(), "tentmaker: ") {
			t.Errorf("%v: stderr %q", args, f.stderr)
		}
		jargs := append(append([]string{}, args...), "--json")
		if args[len(args)-1] == "--json" {
			jargs = args
		}
		if code := f.run("", jargs...); code != ExitUsage {
			t.Errorf("%v: exit %d", jargs, code)
			continue
		}
		if out := f.jsonObject(jargs); out["error"] == nil {
			t.Errorf("%v: %v", jargs, out)
		}
	}
}

func TestHelpForEveryCommand(t *testing.T) {
	f := newFixture(t)
	for name, c := range commands {
		for _, args := range [][]string{{name, "--help"}, {"help", name}} {
			if code := f.run("", args...); code != ExitOK {
				t.Errorf("%v: exit %d", args, code)
			}
			if !strings.Contains(f.stdout.String(), firstLine(c.usage)) {
				t.Errorf("%v: %q", args, f.stdout)
			}
		}
	}
	for _, args := range [][]string{{"--help"}, {"help"}, {"-h"}} {
		if code := f.run("", args...); code != ExitOK || !strings.Contains(f.stdout.String(), "Exit codes") {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// TestJSONForEveryCommand: --json prints one JSON object on stdout, for
// successes and errors alike.
func TestJSONForEveryCommand(t *testing.T) {
	f := newFixture(t)
	f.serveControl()
	cases := [][]string{
		{"--json"},
		{"version", "--json"},
		{"status", "--json"},
		{"pool", "--json"},
		{"budget", "--json"},
		{"doctor", "--json"},
		{"setup", "--yes", "--json", "--no-mesh", "--token-stdin", "--repo", "a/b"},
		{"--json"},
		{"status", "--json"},
		{"budget", "set", "--memory", "3GiB", "--boost-cpus", "6", "--json"},
		{"schedule", "add", "--name", "work", "--days", "mon-fri", "--from", "09:00", "--to", "17:00", "--profile", "everyday", "--json"},
		{"schedule", "list", "--json"},
		{"schedule", "remove", "work", "--json"},
		{"schedule", "remove", "work", "--json"},
		{"repo", "add", "c/d", "--json"},
		{"repo", "list", "--json"},
		{"repo", "remove", "c/d", "--json"},
		{"connect", "github", "--token-stdin", "--json"},
		{"pause", "--for", "30m", "--json", "a", "call"},
		{"resume", "--json"},
		{"devices", "add", "desktop", "--repo", "a/b", "--json"},
		{"migrate", f.root, "--json"},
		{"logs", "--json"},
		{"update", "--check", "--json"},
		{"update", "--json"},
		{"rollback", "--json"},
		{"check-run", "--out", filepath.Join(f.root, "out"), "--json"},
		{"uninstall", "--yes", "--json"},
		{"nope", "--json"},
	}
	for _, args := range cases {
		stdin := ""
		switch args[0] {
		case "setup", "connect":
			stdin = canary
		case "check-run":
			stdin = `{"repo":"a/b","sha":"abc"}`
		}
		code := f.run(stdin, args...)
		out := f.jsonObject(args)
		if code != ExitOK && out["error"] == nil && args[0] != "doctor" {
			t.Errorf("%v: exit %d without an error object: %v", args, code, out)
		}
		if f.stderr.Len() != 0 {
			t.Errorf("%v: stderr in --json mode: %s", args, f.stderr)
		}
	}
	if len(f.daemon.paused) != 1 || f.daemon.paused[0] != "a call" || f.daemon.resumes != 1 {
		t.Errorf("pause/resume did not reach the service: %+v", f.daemon)
	}
	if f.daemon.reloads == 0 {
		t.Error("config changes did not reload the service")
	}
}

func TestBudgetSetAndSchedule(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	if code := f.run("", "budget", "set", "--memory", "3GiB", "--boost-memory", "9GiB", "--cpus", "1.5", "--on-battery", "off", "--boost-needs-idle", "true", "--pause-below", "50", "--stop-below", "0"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	b := f.config().Budget
	if b.Everyday.Memory != 3*config.GiB || b.Boost.Memory != 9*config.GiB || b.Everyday.CPUs != 1.5 || b.OnBattery != "off" || !b.BoostNeedsIdle || b.PauseBelow != 50 || b.StopBelow != 0 {
		t.Errorf("budget %+v", b)
	}
	if !strings.Contains(f.stdout.String(), "service is not running") {
		t.Errorf("no note about the service: %s", f.stdout)
	}
	// An invalid combination is refused and nothing is saved.
	if code := f.run("", "budget", "set", "--memory", "12GiB"); code != ExitFailed {
		t.Errorf("everyday above boost: exit %d", code)
	}
	if f.config().Budget.Everyday.Memory != 3*config.GiB {
		t.Error("an invalid budget was saved")
	}
	f.run("", "schedule", "add", "--name", "night", "--days", "fri-mon", "--from", "22:00", "--to", "06:00", "--profile", "boost")
	ws := f.config().Budget.Schedule
	if len(ws) != 1 || !reflect.DeepEqual(ws[0].Days, []string{"fri", "sat", "sun", "mon"}) {
		t.Fatalf("schedule %+v", ws)
	}
	if code := f.run("", "schedule", "remove", "night"); code != ExitOK || len(f.config().Budget.Schedule) != 0 {
		t.Errorf("remove: exit %d", code)
	}
	if code := f.run("", "schedule", "remove", "night"); code != ExitFailed {
		t.Errorf("removing a missing window: exit %d", code)
	}
}

func TestParseDays(t *testing.T) {
	for in, want := range map[string][]string{
		"daily": nil, "": nil, "mon-fri": {"mon", "tue", "wed", "thu", "fri"},
		"sat,sun": {"sat", "sun"}, "sun-tue": {"sun", "mon", "tue"}, "MON,mon": {"mon"},
	} {
		got, err := parseDays(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"someday", "mon-xyz", "mon,,tue"} {
		if _, err := parseDays(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestOrigin(t *testing.T) {
	cfg := "[remote \"upstream\"]\n\turl = https://github.com/x/y\n[remote \"origin\"]\n\turl = https://github.com/baney75/tentmaker.git\n\tfetch = +refs/heads/*\n"
	if got := repoFromOrigin(gitOrigin(cfg), "github.com"); got != "baney75/tentmaker" {
		t.Errorf("https: %q", got)
	}
	if got := repoFromOrigin("git@github.com:o/n.git", "github.com"); got != "o/n" {
		t.Errorf("ssh: %q", got)
	}
	if got := repoFromOrigin("git@gitlab.com:o/n.git", "github.com"); got != "" {
		t.Errorf("other host: %q", got)
	}
}

// forkForge is the fake forge plus GitHub's fork approval setter.
type forkForge struct {
	*fakeforge.Forge
	set []string
}

func (f *forkForge) SetForkApproval(ctx context.Context, repo string) error {
	f.set = append(f.set, repo)
	return nil
}

func TestRepoAdd(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	f.store.Set("github-token", []byte(canary))
	f.forge.SetRepo(core.RepoInfo{Repo: "o/pub", Public: true, DefaultBranch: "main", ForkPolicy: "approve_first_time"})
	if code := f.run("", "repo", "add", "o/pub"); code != ExitFailed || !strings.Contains(f.stderr.String(), "--allow-public") {
		t.Errorf("public repo: exit %d %s", code, f.stderr)
	}
	ff := &forkForge{Forge: f.forge}
	f.env.NewForge = func(name string, fc config.Forge, tok func(context.Context) (string, error), log *slog.Logger) (core.Forge, core.StatusForge, error) {
		return ff, f.forge, nil
	}
	f.forge.SetFile("o/pub", "main", ".github/workflows/ci.yml", []byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make\n"))
	code := f.run("", "repo", "add", "o/pub", "--allow-public", "--enable", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stdout)
	}
	out := f.jsonObject(nil)
	if out["fork_approval_set"] != true || !reflect.DeepEqual(ff.set, []string{"o/pub"}) {
		t.Errorf("fork approval: %v %v", out["fork_approval_set"], ff.set)
	}
	mig, _ := out["migrate"].([]any)
	if len(mig) != 1 || mig[0].(map[string]any)["changed"] != true {
		t.Errorf("migrate preview %v", out["migrate"])
	}
	if r := f.config().Repos; len(r) != 1 || !r[0].AllowPublic {
		t.Errorf("repos %+v", r)
	}
	// Adding it again updates instead of duplicating; remove takes it out.
	f.run("", "repo", "add", "o/pub", "--allow-public")
	if len(f.config().Repos) != 1 {
		t.Error("duplicate repo")
	}
	f.run("", "repo", "remove", "o/pub")
	if len(f.config().Repos) != 0 {
		t.Error("not removed")
	}
	// Checks mode can follow branches besides the default one.
	if code := f.run("", "repo", "add", "o/pub", "--allow-public", "--mode", "checks", "--branch", "e2e", "--branch", "release/1"); code != ExitOK {
		t.Fatalf("--branch: exit %d: %s", code, f.stderr)
	}
	if r := f.config().Repos; len(r) != 1 || !reflect.DeepEqual(r[0].Branches, []string{"e2e", "release/1"}) {
		t.Errorf("branches %+v", r)
	}
}

func TestConnect(t *testing.T) {
	f := newFixture(t)
	if code := f.run(canary, "connect", "github", "--token-stdin"); code != ExitNotSetUp {
		t.Errorf("before setup: exit %d", code)
	}
	f.setUp()
	if code := f.run(canary, "connect", "forgejo", "--url", "https://git.example.com/", "--token-stdin", "--json"); code != ExitOK {
		t.Fatalf("exit %d %s", code, f.stdout)
	}
	out := f.jsonObject(nil)
	if out["secret"] != "forgejo-git.example.com-token" || out["source"] != "stdin" {
		t.Errorf("out %v", out)
	}
	if f.config().Forges["forgejo"].URL != "https://git.example.com" {
		t.Errorf("forges %+v", f.config().Forges)
	}
	if code := f.run("", "connect", "github", "--token-stdin"); code != ExitUsage {
		t.Errorf("empty stdin: exit %d", code)
	}
	if code := f.run("", "connect", "gitlab"); code != ExitUsage {
		t.Errorf("no credential anywhere: exit %d", code)
	}
}

func TestVerifyToken(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization") + r.Header.Get("PRIVATE-TOKEN")
		if !strings.Contains(gotAuth, canary) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"login":"octo","username":"octo"}`)
	}))
	defer srv.Close()
	for kind, path := range map[string]string{"github": "/user", "forgejo": "/api/v1/user", "gitlab": "/api/v4/user"} {
		login, err := verifyToken(context.Background(), srv.Client(), kind, srv.URL, canary)
		if err != nil || login != "octo" || gotPath != path {
			t.Errorf("%s: %q %v %s", kind, login, err, gotPath)
		}
		_, err = verifyToken(context.Background(), srv.Client(), kind, srv.URL, "wrong")
		if err == nil || strings.Contains(err.Error(), "wrong") {
			t.Errorf("%s: bad credential error %v", kind, err)
		}
	}
}

// An app installation token (Actions' GITHUB_TOKEN) gets 403 from /user
// but is valid; a token GitHub refuses everywhere is not.
func TestVerifyTokenGitHubInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case !strings.Contains(auth, canary):
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/user":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
		case r.URL.Path == "/installation/repositories" && strings.Contains(auth, "inst"):
			io.WriteString(w, `{"total_count":1,"repositories":[]}`)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	login, err := verifyToken(context.Background(), srv.Client(), "github", srv.URL, canary+"inst")
	if err != nil || login != "" {
		t.Errorf("installation token: %q %v", login, err)
	}
	if _, err := verifyToken(context.Background(), srv.Client(), "github", srv.URL, canary); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("refused token: %v", err)
	}
}

func TestPauseResumeNeedsService(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	if code := f.run("", "pause"); code != ExitFailed || !strings.Contains(f.stderr.String(), "service not running") {
		t.Errorf("exit %d %s", code, f.stderr)
	}
}

func TestStatusAndBare(t *testing.T) {
	f := newFixture(t)
	if code := f.run(""); code != ExitNotSetUp || !strings.Contains(f.stdout.String(), "sudo tentmaker setup") || f.stderr.Len() != 0 {
		t.Errorf("bare before setup: exit %d %q %q", code, f.stdout, f.stderr)
	}
	f.setUp("--repo", "a/b")
	if code := f.run("", "status"); code != ExitOK {
		t.Fatalf("exit %d %s", code, f.stderr)
	}
	for _, want := range []string{"test-box", "service", "not running", "next"} {
		if !strings.Contains(f.stdout.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, f.stdout)
		}
	}
	f.serveControl()
	if code := f.run("", "--json"); code != ExitOK || f.jsonObject(nil)["version"] != "1.0.0" {
		t.Errorf("bare with service: %d %s", code, f.stdout)
	}
}

func TestMigrate(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "repo", ".github", "workflows")
	os.MkdirAll(dir, 0o755)
	wf := filepath.Join(dir, "ci.yml")
	src := "on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make\n"
	os.WriteFile(wf, []byte(src), 0o644)
	repo := filepath.Join(f.root, "repo")
	if code := f.run("", "migrate", repo); code != ExitOK || !strings.Contains(f.stdout.String(), "dry run") {
		t.Fatalf("exit %d %s", code, f.stdout)
	}
	if b, _ := os.ReadFile(wf); string(b) != src {
		t.Fatal("dry run wrote")
	}
	if code := f.run("", "migrate", repo, "--write"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(wf); string(b) == src || !strings.Contains(string(b), "tentmaker") {
		t.Errorf("not written: %s", b)
	}
	if code := f.run("", "migrate", filepath.Join(f.root, "nothing")); code != ExitFailed {
		t.Errorf("missing path: exit %d", code)
	}
}

func TestLogs(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(filepath.Dir(f.env.Paths.Log), 0o755)
	os.WriteFile(f.env.Paths.Log, []byte("a\nb\nc\nd\n"), 0o644)
	if code := f.run("", "logs", "-n", "2", "--json"); code != ExitOK {
		t.Fatal(code)
	}
	if lines := f.jsonObject(nil)["lines"].([]any); len(lines) != 2 || lines[1] != "d" {
		t.Errorf("lines %v", lines)
	}
	f.env.Paths.Log = "" // Linux: journald
	if code := f.run("", "logs"); code != ExitOK || !strings.Contains(f.stdout.String(), "line two") {
		t.Errorf("journal: %d %s", code, f.stdout)
	}
}

func TestUpdateHealthAndRollback(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	f.up.newer = true
	if code := f.run("", "update", "--check", "--json"); code != ExitOK || f.jsonObject(nil)["available"] != true || len(f.up.applied) != 0 {
		t.Errorf("--check: %d %s", code, f.stdout)
	}
	// The service never reports the new version, so update rolls back.
	defer func(d time.Duration) { UpdateHealthWait = d }(UpdateHealthWait)
	UpdateHealthWait = 300 * time.Millisecond
	f.svc.installed = true
	f.serveControl()
	done := make(chan int, 1)
	go func() { done <- f.run("", "update", "--json") }()
	select {
	case code := <-done:
		if code != ExitFailed || f.up.rolledBack != 1 || f.jsonObject(nil)["rolled_back"] != true {
			t.Errorf("exit %d rollbacks %d %s", code, f.up.rolledBack, f.stdout)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("update hung")
	}
	// When the restarted service reports the new version, update keeps it.
	f.daemon.mu.Lock()
	f.daemon.version = "9.9.9"
	f.daemon.mu.Unlock()
	if code := f.run("", "update", "--json"); code != ExitOK || f.up.rolledBack != 1 || f.jsonObject(nil)["applied"] != true {
		t.Errorf("healthy update: exit %d rollbacks %d %s", code, f.up.rolledBack, f.stdout)
	}
}

func TestUninstallPurge(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	os.MkdirAll(f.env.Paths.Cache, 0o755)
	os.MkdirAll(f.env.Paths.State, 0o755)
	if code := f.run("", "uninstall", "--yes"); code != ExitOK {
		t.Fatalf("exit %d %s", code, f.stderr)
	}
	if _, err := os.Stat(f.env.ConfigPath); err != nil {
		t.Error("plain uninstall removed the config")
	}
	if code := f.run("", "uninstall", "--purge", "--yes", "--json"); code != ExitOK {
		t.Fatalf("exit %d %s", code, f.stdout)
	}
	for _, p := range []string{f.env.ConfigPath, f.env.Paths.State, f.env.Paths.Cache} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived --purge", p)
		}
	}
	if f.execUnin != 2 {
		t.Errorf("executors uninstalled %d times", f.execUnin)
	}
	if err := removeOwned("/etc"); err == nil {
		t.Error("removeOwned accepted /etc")
	}
}

func TestCheckRun(t *testing.T) {
	f := newFixture(t)
	var got core.CheckBundle
	f.env.RunBundle = func(ctx context.Context, b core.CheckBundle, out string, log io.Writer) core.CheckResult {
		got = b
		return core.CheckResult{State: "error", Description: "boom"}
	}
	if code := f.run(`{"repo":"a/b","sha":"abc"}`, "check-run", "--out", f.root); code != ExitFailed || got.Repo != "a/b" {
		t.Errorf("exit %d %+v", code, got)
	}
	if code := f.run("not json", "check-run", "--out", f.root); code != ExitUsage {
		t.Errorf("bad bundle: exit %d", code)
	}
}

func TestDevicesAdd(t *testing.T) {
	f := newFixture(t)
	f.setUp("--pool", "home")
	if code := f.run("", "devices", "add", "desktop", "--memory", "4GiB", "--repo", "a/b"); code != ExitOK {
		t.Fatalf("exit %d %s", code, f.stderr)
	}
	if len(f.remote) != 1 || f.remote[0].Pool != "home" || f.remote[0].Memory != "4GiB" || f.remote[0].Channel != "stable" {
		t.Errorf("remote %+v", f.remote)
	}
}

func TestDaemonCommand(t *testing.T) {
	f := newFixture(t)
	var got daemon.Options
	f.env.RunDaemon = func(ctx context.Context, o daemon.Options) error {
		got = o
		o.Log.Info("hello from the daemon")
		return nil
	}
	if code := f.run("", "daemon"); code != ExitOK {
		t.Fatalf("exit %d %s", code, f.stderr)
	}
	if !got.ReloadOnHUP || got.ConfigPath != f.env.ConfigPath || got.Paths != f.env.Paths || got.Version != "1.0.0" {
		t.Errorf("options %+v", got)
	}
	if b, _ := os.ReadFile(f.env.Paths.Log); !strings.Contains(string(b), "hello from the daemon") {
		t.Errorf("log file: %q", b)
	}
	// A self-update ends the process non-zero so every service manager
	// restarts it.
	f.env.RunDaemon = func(ctx context.Context, o daemon.Options) error { return daemon.ErrRestart }
	if code := f.run("", "daemon"); code != ExitFailed {
		t.Errorf("restart exit %d", code)
	}
}

// TestMCPController drives the Controller through mcp.Serve over a pipe.
func TestMCPController(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	// Set before the server starts: its handler reads isRoot.
	f.isRoot = false
	f.serveControl()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f.env.Stdin = inR
	f.env.Stdout = outW
	done := make(chan int, 1)
	go func() {
		done <- Run(f.env, []string{"mcp"})
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		if !sc.Scan() {
			t.Fatalf("no answer to %s: %v", method, sc.Err())
		}
		var resp map[string]any
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			t.Fatalf("%s: not JSON: %s", method, sc.Bytes())
		}
		return resp
	}
	if r := call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}); r["result"] == nil {
		t.Fatalf("initialize %v", r)
	}
	r := call(2, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{}})
	res, _ := r["result"].(map[string]any)
	if res == nil || res["isError"] == true || !strings.Contains(mustJSON(res), `"version":"1.0.0"`) {
		t.Errorf("status %v", r)
	}
	r = call(3, "tools/call", map[string]any{"name": "pause", "arguments": map[string]any{"reason": "call"}})
	res, _ = r["result"].(map[string]any)
	if res == nil || res["isError"] != true || !strings.Contains(mustJSON(res), "not running as root") {
		t.Errorf("pause without root %v", r)
	}
	f.isRoot = true
	r = call(4, "tools/call", map[string]any{"name": "pause", "arguments": map[string]any{"reason": "call"}})
	if res, _ = r["result"].(map[string]any); res == nil || res["isError"] == true || len(f.daemon.paused) != 1 {
		t.Errorf("pause as root %v %v", r, f.daemon.paused)
	}
	r = call(5, "tools/call", map[string]any{"name": "set_budget", "arguments": map[string]any{"everyday_memory": "3GiB"}})
	if res, _ = r["result"].(map[string]any); res == nil || res["isError"] == true || f.config().Budget.Everyday.Memory != 3*config.GiB {
		t.Errorf("set_budget %v", r)
	}
	inW.Close()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("mcp exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mcp did not end at EOF")
	}
	if strings.Contains(f.stderr.String(), canary) {
		t.Error("credential on stderr")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// guard the test suite against a test that touches the real host paths.
func TestFixturePathsAreTemp(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{f.env.ConfigPath, f.env.Paths.Secrets, f.env.Paths.State, f.env.Paths.Cache, f.env.Paths.Log} {
		if !strings.HasPrefix(p, os.TempDir()) && !strings.HasPrefix(p, filepath.Dir(f.root)) {
			t.Errorf("%s is outside the temp dir", p)
		}
	}
}

// The MCP update tool says passing a channel switches the device to it.
func TestMCPUpdateSwitchesChannel(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	f.isRoot = true
	f.up.newer = true
	f.svc.installed = true
	f.serveControl()
	f.daemon.mu.Lock()
	f.daemon.version = "9.9.9"
	f.daemon.mu.Unlock()
	a := &app{env: f.env, ctx: context.Background(), json: true}
	if _, err := (controller{a}).Update(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if got := f.config().Update.Channel; got != "dev" {
		t.Fatalf("channel after an update on dev: %q", got)
	}
	// Omitting the channel keeps the configured one.
	if _, err := (controller{a}).Update(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := f.config().Update.Channel; got != "dev" {
		t.Fatalf("channel after an update with none given: %q", got)
	}
	// An update that rolls back switches nothing.
	defer func(d time.Duration) { UpdateHealthWait = d }(UpdateHealthWait)
	UpdateHealthWait = 300 * time.Millisecond
	f.daemon.mu.Lock()
	f.daemon.version = "1.0.0"
	f.daemon.mu.Unlock()
	(controller{a}).Update(context.Background(), "stable")
	if got := f.config().Update.Channel; got != "dev" {
		t.Fatalf("a rolled-back update switched the channel to %q", got)
	}
}

func TestSetupMacOSGuestsFlag(t *testing.T) {
	f := newFixture(t)
	f.isRoot = true
	if code := f.run("", "setup", "--yes", "--json", "--no-mesh", "--no-gh", "--no-service", "--macos-guests"); code != ExitOK {
		t.Fatalf("exit %d %s %s", code, f.stdout, f.stderr)
	}
	if !f.installOpts.MacOSGuests {
		t.Fatal("--macos-guests did not reach the executor install")
	}
	if code := f.run("", "setup", "--yes", "--json", "--no-mesh", "--no-gh", "--no-service"); code != ExitOK || f.installOpts.MacOSGuests {
		t.Fatalf("without the flag: exit %d opts %+v", code, f.installOpts)
	}
}

// TestDoctorAsUserSkipsSandboxChecks: the sandbox checks read state only
// root can read (the runner's podman socket, the nft table, root-only
// files, Tart's home), so a user's doctor would report a working machine
// as broken. It says it skipped them and how to run them instead.
func TestDoctorAsUserSkipsSandboxChecks(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	built := 0
	f.env.Executors = func(cfg config.Config, bin func(core.Platform) (string, error), log *slog.Logger) ([]core.Executor, []core.Problem) {
		built++
		return nil, []core.Problem{{Check: "podman socket", What: "not readable as this user", Fix: "run sudo tentmaker setup"}}
	}
	f.isRoot = false
	f.run("", "doctor", "--json")
	if built != 0 {
		t.Errorf("user doctor built the executors %d times", built)
	}
	out := f.jsonObject([]string{"doctor"})
	var skip map[string]any
	for _, c := range out["checks"].([]any) {
		c := c.(map[string]any)
		switch c["check"] {
		case "executor":
			skip = c
		case "podman socket":
			t.Errorf("user doctor ran a sandbox check: %v", c)
		}
	}
	if skip == nil || skip["status"] != "warn" || skip["fix"] != "sudo tentmaker doctor" {
		t.Errorf("user doctor does not say the sandbox checks were skipped: %v", out["checks"])
	}

	f.isRoot = true
	f.run("", "doctor", "--json")
	if built != 1 || !strings.Contains(f.stdout.String(), `"check":"podman socket"`) {
		t.Errorf("root doctor built the executors %d times and did not report their problem:\n%s", built, f.stdout)
	}
}

// A join code on the command line is refused without echoing it.
func TestJoinNeverEchoesACodeArgument(t *testing.T) {
	const code = "tm1-abcdefghijklmnopqrstuvwxyz234567@100.64.0.1:7433"
	for _, args := range [][]string{{"join", code}, {"join", code, "--json"}} {
		f := newFixture(t)
		if c := f.run("", args...); c != ExitUsage {
			t.Fatalf("%v: exit %d", args, c)
		}
		if strings.Contains(f.stderr.String()+f.stdout.String(), "abcdefghij") {
			t.Fatalf("%v echoed the code: %q", args, f.stderr.String()+f.stdout.String())
		}
	}
}
