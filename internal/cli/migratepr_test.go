package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

const ciWorkflow = `name: CI
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  deploy:
    runs-on: ubuntu-latest
    environment: prod
    steps:
      - run: ./deploy
`

// fakeGit plays a checkout whose origin is on GitHub.
type fakeGit struct {
	mu        sync.Mutex
	origin    string
	dirty     string
	branch    string // the branch the checkout is on
	remoteSHA string // origin's tentmaker/migrate, "" when it has none
	calls     []string
	messages  []string
	pushes    int
	pushErr   error // returned by push when set
	commitErr error // returned by commit when set
}

func (g *fakeGit) run(ctx context.Context, dir string, stdin io.Reader, args ...string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, strings.Join(args, " "))
	switch args[0] {
	case "rev-parse":
		switch args[1] {
		case "--show-toplevel":
			return dir + "\n", nil
		case "--verify":
			return "", errors.New("no such branch")
		}
		return "c0ffee0000000000000000000000000000000001\n", nil
	case "remote":
		if g.origin == "" {
			return "", errors.New("no such remote 'origin'")
		}
		return g.origin + "\n", nil
	case "status":
		return g.dirty, nil
	case "ls-remote":
		if args[1] == "--symref" {
			return "ref: refs/heads/main\tHEAD\n1111111111111111111111111111111111111111\tHEAD\n", nil
		}
		if g.remoteSHA == "" {
			return "", nil
		}
		return g.remoteSHA + "\trefs/heads/" + migrateBranch + "\n", nil
	case "symbolic-ref":
		return g.branch + "\n", nil
	case "commit":
		if g.commitErr != nil {
			return "", g.commitErr
		}
		b, _ := io.ReadAll(stdin)
		g.messages = append(g.messages, string(b))
	case "push":
		if g.pushErr != nil {
			return "", g.pushErr
		}
		g.pushes++
		g.remoteSHA = "c0ffee0000000000000000000000000000000001"
	}
	return "", nil
}

func (g *fakeGit) has(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (g *fakeGit) last() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[len(g.calls)-1]
}

// fakePRs is GitHub's pull request API: one open PR per head.
type fakePRs struct {
	mu     sync.Mutex
	open   map[string]core.PullRequestResult
	bodies []string
	tokens []string
	next   int
}

func (p *fakePRs) openOrUpdate(pr core.PullRequest) core.PullRequestResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open == nil {
		p.open = map[string]core.PullRequestResult{}
	}
	p.bodies = append(p.bodies, pr.Body)
	if r, ok := p.open[pr.Head]; ok {
		r.Created = false
		return r
	}
	p.next++
	r := core.PullRequestResult{Number: p.next, URL: "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(p.next), Created: true}
	p.open[pr.Head] = r
	return r
}

// daemonPRs lets the fake daemon open pull requests with "its" token.
var daemonPRs = &fakePRs{}

func (d *fakeDaemon) OpenPR(ctx context.Context, pr core.PullRequest) (core.PullRequestResult, error) {
	return daemonPRs.openOrUpdate(pr), nil
}

func gitFixture(t *testing.T) (*fixture, *fakeGit, *fakePRs, string) {
	f := newFixture(t)
	g := &fakeGit{origin: "git@github.com:octo/app.git", branch: "feature"}
	prs := &fakePRs{}
	f.env.Git = g.run
	f.env.OpenPR = func(ctx context.Context, token string, pr core.PullRequest) (core.PullRequestResult, error) {
		prs.mu.Lock()
		prs.tokens = append(prs.tokens, token)
		prs.mu.Unlock()
		return prs.openOrUpdate(pr), nil
	}
	wf := filepath.Join(f.root, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(wf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wf, []byte(ciWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	return f, g, prs, wf
}

func TestMigratePRCreatesThenUpdates(t *testing.T) {
	f, g, prs, wf := gitFixture(t)
	f.ghValue = canary
	if code := f.run("", "migrate", "--pr", "--json"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	m := f.jsonObject([]string{"migrate", "--pr"})
	pr := m["pr"].(map[string]any)
	if pr["created"] != true || pr["number"] != 1.0 || m["repo"] != "octo/app" || m["base"] != "main" || m["token_source"] != "gh" {
		t.Fatalf("first run: %v", m)
	}
	if len(prs.tokens) != 1 || prs.tokens[0] != canary {
		t.Fatal("the PR was not opened with gh's token")
	}
	if !g.has("checkout --quiet -B tentmaker/migrate refs/remotes/origin/main") ||
		!g.has("add -- .github/workflows/ci.yml") ||
		!g.has("push --quiet --force-with-lease=refs/heads/tentmaker/migrate: origin HEAD:refs/heads/tentmaker/migrate") {
		t.Fatalf("git calls: %v", g.calls)
	}
	if g.last() != "checkout --quiet feature" {
		t.Fatalf("did not go back to the feature branch: %v", g.last())
	}
	if !strings.HasPrefix(g.messages[0], "Move CI jobs onto Tentmaker runners\n\n") || !strings.Contains(g.messages[0], "ci.yml: test (ubuntu-latest)") {
		t.Fatalf("commit message:\n%s", g.messages[0])
	}
	body := prs.bodies[0]
	for _, want := range []string{
		"| Workflow | Job | Before | After |",
		"| `.github/workflows/ci.yml` line 5 | `test` | `ubuntu-latest` | `${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' \\|\\| fromJSON('[\"self-hosted\",\"tentmaker\",\"linux\"]') }}` |",
		"runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || fromJSON('[\"self-hosted\",\"tentmaker\",\"linux\"]') }}",
		"### GitHub-hosted runners still work",
		"fallback",
		"job 'deploy' deploys or holds credentials",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PR body lacks %q:\n%s", want, body)
		}
	}
	if strings.ContainsAny(body, "\u2013\u2014") {
		t.Fatal("PR body has an en or em dash")
	}

	// Run again from a fresh checkout of main: the branch is rebuilt and
	// the open PR is updated, not a second one opened.
	if err := os.WriteFile(wf, []byte(ciWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run("", "migrate", "--pr", "--json"); code != ExitOK {
		t.Fatalf("second run exit %d\n%s", code, f.stdout)
	}
	m = f.jsonObject([]string{"migrate", "--pr"})
	pr = m["pr"].(map[string]any)
	if pr["created"] != false || pr["number"] != 1.0 || len(prs.open) != 1 || len(prs.bodies) != 2 {
		t.Fatalf("second run: %v", m)
	}
	if !g.has("--force-with-lease=refs/heads/tentmaker/migrate:c0ffee0000000000000000000000000000000001") {
		t.Fatalf("second push had no lease on the first: %v", g.calls)
	}
}

func TestMigratePRRefusesADirtyTree(t *testing.T) {
	f, g, prs, wf := gitFixture(t)
	f.ghValue = canary
	g.dirty = " M README.md\n"
	if code := f.run("", "migrate", "--pr", "--json"); code != ExitFailed {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	m := f.jsonObject([]string{"migrate", "--pr"})
	if !strings.Contains(m["error"].(string), "uncommitted changes") || !strings.Contains(m["fix"].(string), "commit or stash") {
		t.Fatalf("%v", m)
	}
	if g.has("checkout") || g.has("push") || len(prs.bodies) != 0 {
		t.Fatalf("a dirty tree was touched: %v", g.calls)
	}
	if b, _ := os.ReadFile(wf); string(b) != ciWorkflow {
		t.Fatal("the workflow was rewritten")
	}
}

func TestMigratePRWithTheServiceToken(t *testing.T) {
	f, g, _, _ := gitFixture(t)
	f.setUp()
	f.serveControl()
	before := len(daemonPRs.bodies)
	if code := f.run("", "migrate", "--pr", "--all", "--json"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	m := f.jsonObject([]string{"migrate", "--pr"})
	if m["token_source"] != "service" || m["all"] != true || g.pushes != 1 {
		t.Fatalf("%v", m)
	}
	body := daemonPRs.bodies[before]
	if !strings.Contains(body, "| `[self-hosted, tentmaker, linux]` |") || !strings.Contains(body, "made with `--all`") {
		t.Fatalf("--all body:\n%s", body)
	}
}

func TestMigratePRWithNoToken(t *testing.T) {
	f, g, _, _ := gitFixture(t)
	if code := f.run("", "migrate", "--pr", "--json"); code != ExitFailed {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	m := f.jsonObject([]string{"migrate", "--pr"})
	if !strings.Contains(m["fix"].(string), "gh auth login") || !strings.Contains(m["fix"].(string), "is pushed") {
		t.Fatalf("%v", m)
	}
	if g.last() != "checkout --quiet feature" {
		t.Fatalf("a failure left the checkout on the migrate branch: %v", g.last())
	}
}

func TestMigratePRNotGitHub(t *testing.T) {
	f, g, _, _ := gitFixture(t)
	g.origin = "https://oauth2:" + canary + "@gitlab.com/octo/app.git"
	if code := f.run("", "migrate", "--pr", "--json"); code != ExitFailed {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.stdout.String(), "is not a github.com repo") {
		t.Fatal(f.stdout.String())
	}
}

func writeGitConfig(t *testing.T, dir, origin string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://github.com/other/thing.git\n[remote \"origin\"]\n\turl = " + origin + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRepoHereParsesOrigins(t *testing.T) {
	for _, origin := range []string{
		"https://github.com/octo/app.git",
		"https://github.com/octo/app",
		"git@github.com:octo/app.git",
		"ssh://git@github.com/octo/app.git",
		"https://x-access-token:" + canary + "@github.com/octo/app.git",
	} {
		f := newFixture(t)
		writeGitConfig(t, f.root, origin)
		if code := f.run("", "setup", "--yes", "--no-service", "--no-mesh", "--repo-here", "--json"); code != ExitOK {
			t.Fatalf("%s: exit %d\n%s", origin, code, f.stdout)
		}
		cfg := f.config()
		if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "octo/app" {
			t.Fatalf("%s: repos %+v", origin, cfg.Repos)
		}
	}

	// A linked worktree: .git is a file, the config lives in the main repo.
	f := newFixture(t)
	main := filepath.Join(f.root, "main")
	writeGitConfig(t, main, "git@github.com:octo/wt.git")
	gitdir := filepath.Join(main, ".git", "worktrees", "wt")
	os.MkdirAll(gitdir, 0o755)
	os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644)
	wt := filepath.Join(f.root, "wt")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644)
	f.env.Getwd = func() (string, error) { return wt, nil }
	if code := f.run("", "setup", "--yes", "--no-service", "--no-mesh", "--repo-here"); code != ExitOK {
		t.Fatalf("worktree: exit %d\n%s", code, f.stdout)
	}
	if cfg := f.config(); len(cfg.Repos) != 1 || cfg.Repos[0].Name != "octo/wt" {
		t.Fatalf("worktree repos %+v", cfg.Repos)
	}

	// No origin on github.com: a usage error that says what to pass.
	f = newFixture(t)
	writeGitConfig(t, f.root, "https://gitlab.com/octo/app.git")
	if code := f.run("", "setup", "--yes", "--no-service", "--no-mesh", "--repo-here", "--json"); code != ExitUsage {
		t.Fatalf("gitlab origin: exit %d", code)
	}
	if m := f.jsonObject(nil); !strings.Contains(m["fix"].(string), "--repo OWNER/NAME") {
		t.Fatalf("%v", m)
	}
}

func TestSetupRepoHereAndPR(t *testing.T) {
	f, g, prs, _ := gitFixture(t)
	writeGitConfig(t, f.root, g.origin)
	f.ghValue = canary
	f.serveControl()
	if code := f.run("", "setup", "--yes", "--no-mesh", "--repo-here", "--pr", "--json"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	m := f.jsonObject(nil)
	pv, _ := m["pr"].(map[string]any)
	if pv == nil || pv["ok"] != true || len(prs.bodies) != 1 {
		t.Fatalf("setup --pr opened no PR: %v", m)
	}
	if !strings.Contains(m["next"].(string), "merge the pull request: https://github.com/octo/app/pull/1") {
		t.Fatalf("next: %v", m["next"])
	}
	if f.config().Repos[0].Name != "octo/app" {
		t.Fatal("repo-here did not add the repo")
	}
}

func TestSetupPRWaitsForDoctor(t *testing.T) {
	f, g, prs, _ := gitFixture(t)
	writeGitConfig(t, f.root, g.origin)
	f.ghValue = canary
	f.env.InstallExecutors = func(ctx context.Context, cfg config.Config, bin func(core.Platform) (string, error), o InstallOptions) error {
		return errors.New("no sandbox here")
	}
	if code := f.run("", "setup", "--yes", "--no-service", "--no-mesh", "--repo-here", "--pr", "--json"); code != ExitFailed {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	if len(prs.bodies) != 0 || g.has("push") {
		t.Fatal("a PR was opened although setup failed")
	}
	if !strings.Contains(f.stdout.String(), "no pull request was opened") {
		t.Fatal(f.stdout.String())
	}
}

// GitHub refuses a workflow change pushed with gh's default OAuth scopes; the
// fix must name the missing scope, and the failed run must undo its edits
// on tentmaker/migrate before it goes back to the user's branch.
func TestMigratePRWorkflowScopeHintAndCleanup(t *testing.T) {
	f, g, _, _ := gitFixture(t)
	f.ghValue = canary
	g.pushErr = errors.New("git push: ! [remote rejected] HEAD -> tentmaker/migrate (refusing to allow an OAuth App to create or update workflow `.github/workflows/ci.yml` without `workflow` scope)")
	if code := f.run("", "migrate", "--pr"); code == ExitOK {
		t.Fatal("a rejected push exited OK")
	}
	if out := f.stdout.String() + f.stderr.String(); !strings.Contains(out, "gh auth refresh -h github.com -s workflow") {
		t.Fatalf("no workflow scope fix in:\n%s", out)
	}
	assertCleanedUp(t, g)
}

func TestMigratePRCleansUpAfterAFailedCommit(t *testing.T) {
	f, g, _, _ := gitFixture(t)
	f.ghValue = canary
	g.commitErr = errors.New("git commit: Please tell me who you are")
	if code := f.run("", "migrate", "--pr"); code == ExitOK {
		t.Fatal("a failed commit exited OK")
	}
	assertCleanedUp(t, g)
}

func assertCleanedUp(t *testing.T, g *fakeGit) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	reset, back := -1, -1
	for i, c := range g.calls {
		switch c {
		case "reset --quiet --hard":
			reset = i
		case "checkout --quiet feature":
			back = i
		}
	}
	if reset < 0 || back < 0 || reset > back {
		t.Fatalf("want reset --hard on the migrate branch, then checkout feature: %v", g.calls)
	}
}

func TestGitEnvKeepsTheAgentAndNeverPrompts(t *testing.T) {
	env := gitEnv(func(k string) string {
		return map[string]string{"SSH_AUTH_SOCK": "/run/user/1000/agent.sock"}[k]
	})
	want := []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes", "SSH_AUTH_SOCK=/run/user/1000/agent.sock"}
	if strings.Join(env, "|") != strings.Join(want, "|") {
		t.Fatalf("gitEnv = %v", env)
	}
	// A user's own GIT_SSH_COMMAND wins.
	env = gitEnv(func(k string) string { return map[string]string{"GIT_SSH_COMMAND": "ssh -i k"}[k] })
	if strings.Join(env, "|") != "GIT_TERMINAL_PROMPT=0" {
		t.Fatalf("gitEnv with GIT_SSH_COMMAND = %v", env)
	}
}

func TestPushFixNamesSSHTrouble(t *testing.T) {
	fix := pushFix("o/n", errors.New("git push: git@github.com: Permission denied (publickey)."))
	if !strings.Contains(fix, "ssh-agent") {
		t.Fatalf("pushFix = %q", fix)
	}
}
