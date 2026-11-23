package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/forge/github"
)

const usageMigrate = `tentmaker migrate [PATH] [--platform linux|macos|windows] [--write] [--pr] [--all] [--json]

Point a checkout's GitHub workflows at the Tentmaker pool. With no flag it
reports what it would change.

  --platform P   linux, macos or windows (default: this device's OS)
  --write        edit .github/workflows in place
  --pr           commit the edits on branch tentmaker/migrate, push it to
                 origin and open a pull request (or update the open one).
                 The working tree must be clean. The token is gh's (as the
                 person who ran sudo), else the Tentmaker service opens it
                 with the token it holds (needs sudo).
  --all          send fork pull requests to the pool too. By default they
                 stay on GitHub-hosted runners; Tentmaker never runs them.
  --json`

// migrateBranch is the branch migrate --pr owns: it is rebuilt from the
// default branch on every run and force-pushed (with a lease).
const migrateBranch = "tentmaker/migrate"

// gitTimeout bounds one git command; gitNetTimeout one that talks to origin.
const (
	gitTimeout    = 30 * time.Second
	gitNetTimeout = 2 * time.Minute
	prTimeout     = 60 * time.Second
)

type migratePRView struct {
	OK          bool                    `json:"ok"`
	Repo        string                  `json:"repo"`
	Dir         string                  `json:"dir"`
	Platform    string                  `json:"platform"`
	All         bool                    `json:"all"`
	Branch      string                  `json:"branch"`
	Base        string                  `json:"base"`
	Commit      string                  `json:"commit,omitempty"`
	Files       []migrateFile           `json:"files"`
	Changed     int                     `json:"changed"`
	PR          *core.PullRequestResult `json:"pr,omitempty"`
	TokenSource string                  `json:"token_source,omitempty"` // gh or service
	Note        string                  `json:"note"`
}

// git runs one git command in dir with a timeout.
func (a *app) git(ctx context.Context, dir string, args ...string) (string, error) {
	return a.gitIn(ctx, dir, nil, args...)
}

func (a *app) gitIn(ctx context.Context, dir string, stdin io.Reader, args ...string) (string, error) {
	if a.env.Git == nil {
		return "", errors.New("git is not available")
	}
	d := gitTimeout
	if len(args) > 0 && (args[0] == "push" || args[0] == "fetch" || args[0] == "ls-remote") {
		d = gitNetTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	out, err := a.env.Git(ctx, dir, stdin, args...)
	return strings.TrimSpace(out), err
}

// migratePR is `tentmaker migrate --pr`: migrate on a fresh tentmaker/migrate
// branch from origin's default branch, commit, push, open or update the PR,
// and put the checkout back on the branch it was on.
func (a *app) migratePR(ctx context.Context, dir, platform string, o github.MigrateOptions) (v migratePRView, err error) {
	if dir == "" {
		wd, err := a.env.Getwd()
		if err != nil {
			return v, fail("pass a PATH", "find the current folder: %v", err)
		}
		dir = wd
	}
	top, err := a.git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return v, fail("run it inside a git checkout of the repo", "%s is not a git checkout: %v", dir, err)
	}
	top = filepath.FromSlash(top)
	origin, err := a.git(ctx, top, "remote", "get-url", "origin")
	if err != nil {
		return v, fail("add the GitHub repo as origin: git remote add origin https://github.com/OWNER/NAME.git", "%s has no origin remote", top)
	}
	repo := repoFromOrigin(origin, "github.com")
	if repo == "" {
		return v, fail("run it in a checkout whose origin is on github.com", "origin %s is not a github.com repo", redactURL(origin))
	}
	if st, err := a.git(ctx, top, "status", "--porcelain"); err != nil {
		return v, fail("", "git status: %v", err)
	} else if st != "" {
		return v, fail("commit or stash your changes, then run it again",
			"the working tree in %s has uncommitted changes; migrate --pr commits only its own edits and refuses a dirty tree", top)
	}
	v = migratePRView{Repo: repo, Dir: top, Platform: platform, All: o.All, Branch: migrateBranch, Files: []migrateFile{}}

	head, err := a.git(ctx, top, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return v, fail("check your network and that you can read "+repo, "read origin's default branch: %v", err)
	}
	base := defaultBranch(head)
	if base == "" {
		return v, fail("check that "+repo+" has a default branch", "origin did not name its default branch")
	}
	v.Base = base
	if _, err := a.git(ctx, top, "fetch", "--quiet", "origin", "+refs/heads/"+base+":refs/remotes/origin/"+base); err != nil {
		return v, fail("check your network and that you can read "+repo, "fetch %s: %v", base, err)
	}

	back, err := a.git(ctx, top, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || back == "" {
		if back, err = a.git(ctx, top, "rev-parse", "HEAD"); err != nil {
			return v, fail("", "find the current commit: %v", err)
		}
	}
	_, noBranch := a.git(ctx, top, "rev-parse", "--verify", "--quiet", "refs/heads/"+migrateBranch)
	if _, err := a.git(ctx, top, "checkout", "--quiet", "-B", migrateBranch, "refs/remotes/origin/"+base); err != nil {
		return v, fail("", "make branch %s: %v", migrateBranch, err)
	}
	defer func() {
		// Cleanup runs even when ctx was canceled mid-push, with its own
		// timeouts (gitIn), so the checkout is never left on our branch.
		cctx := context.WithoutCancel(ctx)
		if err != nil {
			// Drop whatever this run wrote or staged on tentmaker/migrate, so
			// the edits can't follow the checkout back to the user's branch.
			if _, rerr := a.git(cctx, top, "reset", "--quiet", "--hard"); rerr != nil {
				err = errors.Join(err, fmt.Errorf("undo the migrate edits on %s: %v", migrateBranch, rerr))
			}
		}
		if back == migrateBranch {
			return
		}
		if _, cerr := a.git(cctx, top, "checkout", "--quiet", back); cerr != nil {
			cf := fail("git checkout "+back, "go back to %s: %v", back, cerr)
			if err == nil {
				err = cf
			} else {
				err = errors.Join(err, cf)
			}
		}
		if noBranch != nil && v.Changed == 0 { // a branch only this run made
			a.git(cctx, top, "branch", "-D", migrateBranch)
		}
	}()

	mv, err := a.migrate(top, platform, true, o)
	if err != nil {
		return v, err
	}
	v.Platform, v.Files, v.Changed = mv.Platform, mv.Files, mv.Changed
	if !mv.OK {
		return v, fail("fix the workflow error above and run it again", "migrate could not read or write every workflow")
	}
	if v.Changed == 0 {
		v.OK = true
		v.Note = "no " + v.Platform + " job to move, so no commit and no pull request"
		return v, nil
	}

	var changed []string
	for _, f := range v.Files {
		if f.Changed {
			if a.env.IsRoot() {
				if err := ownLikeParent(f.File); err != nil {
					return v, fail("", "give %s back to the checkout's owner: %v", f.File, err)
				}
			}
			rel, err := filepath.Rel(top, f.File)
			if err != nil {
				rel = f.File
			}
			changed = append(changed, filepath.ToSlash(rel))
		}
	}
	if _, err := a.git(ctx, top, append([]string{"add", "--"}, changed...)...); err != nil {
		return v, fail("", "git add: %v", err)
	}
	if _, err := a.gitIn(ctx, top, strings.NewReader(commitMessage(v, changed)), "commit", "--quiet", "--no-verify", "-F", "-"); err != nil {
		return v, fail("set your git name and email (git config --global user.name and user.email), then run it again", "git commit: %v", err)
	}
	if v.Commit, err = a.git(ctx, top, "rev-parse", "HEAD"); err != nil {
		return v, fail("", "read the new commit: %v", err)
	}
	// The lease is what origin has now (nothing, or our last push), so a
	// branch someone pushed to in between is not overwritten.
	remote, err := a.git(ctx, top, "ls-remote", "origin", "refs/heads/"+migrateBranch)
	if err != nil {
		return v, fail("check your network", "read origin's %s: %v", migrateBranch, err)
	}
	lease, _, _ := strings.Cut(remote, "\t")
	if _, err := a.git(ctx, top, "push", "--quiet", "--force-with-lease=refs/heads/"+migrateBranch+":"+strings.TrimSpace(lease),
		"origin", "HEAD:refs/heads/"+migrateBranch); err != nil {
		return v, fail(pushFix(repo, err), "git push: %v", err)
	}

	pr := core.PullRequest{Repo: repo, Head: migrateBranch, Base: base, Title: prTitle, Body: prBody(v)}
	res, source, err := a.openPR(ctx, pr)
	if err != nil {
		return v, err
	}
	v.PR, v.TokenSource, v.OK = &res, source, true
	verb := "opened"
	if !res.Created {
		verb = "updated"
	}
	v.Note = fmt.Sprintf("%s pull request #%d: %s", verb, res.Number, res.URL)
	return v, nil
}

// pushFix names the fix for a failed push. GitHub refuses a push that changes
// .github/workflows from an OAuth token without the workflow scope, which is
// gh's default login, so a plain "can you push" hint would mislead.
func pushFix(repo string, err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "workflow` scope") || strings.Contains(msg, "workflow scope"):
		return "give your gh login the workflow scope (as yourself, not root): gh auth refresh -h github.com -s workflow, then run it again"
	case strings.Contains(msg, "Permission denied (publickey") || strings.Contains(msg, "Host key verification failed"):
		return "git could not use your SSH key: start ssh-agent with your key loaded, or run tentmaker migrate --pr without sudo, then run it again"
	}
	return "check that you can push to " + repo + " (git push works from this checkout), then run it again"
}

// defaultBranch reads `git ls-remote --symref origin HEAD`.
func defaultBranch(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(l), "ref: refs/heads/"); ok {
			b, _, _ := strings.Cut(ref, "\t")
			return strings.TrimSpace(b)
		}
	}
	return ""
}

var urlCredRe = regexp.MustCompile(`(://)[^/@\s]+@`)

// redactURL drops user info (a token in an https origin) from s.
func redactURL(s string) string { return urlCredRe.ReplaceAllString(s, "${1}[redacted]@") }

const prTitle = "Run CI on your own machines with Tentmaker"

func commitMessage(v migratePRView, files []string) string {
	var b strings.Builder
	b.WriteString("Move CI jobs onto Tentmaker runners\n\n")
	b.WriteString("Run these jobs on our own machines through Tentmaker instead of\nGitHub-hosted runners. ")
	if v.All {
		b.WriteString("Made with --all: fork pull requests target the pool\ntoo, and wait there, since Tentmaker never runs them.\n")
	} else {
		b.WriteString("Pull requests from forks stay on\nGitHub-hosted runners.\n")
	}
	b.WriteString("\n")
	for _, f := range v.Files {
		for _, m := range f.Moves {
			rel, _ := filepath.Rel(v.Dir, f.File)
			fmt.Fprintf(&b, "- %s: %s (%s)\n", filepath.ToSlash(rel), m.Job, m.Before)
		}
	}
	b.WriteString("\nMade by tentmaker migrate --pr.\n")
	return b.String()
}

// cell is a value as a Markdown table cell: a code span, with pipes escaped.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	if strings.Contains(s, "`") {
		return strings.ReplaceAll(s, "`", "'")
	}
	return "`" + s + "`"
}

const fence = "```"

// maxPRBody stays under GitHub's 65536-character body limit.
const maxPRBody = 60000

func prBody(v migratePRView) string {
	var b strings.Builder
	b.WriteString("Tentmaker moves these jobs onto your own machines. Each one runs in a throwaway sandbox on a device set up with `tentmaker setup`.\n\n")
	b.WriteString("| Workflow | Job | Before | After |\n| --- | --- | --- | --- |\n")
	example := ""
	var left []string
	rows := 0
	for _, f := range v.Files {
		rel, err := filepath.Rel(v.Dir, f.File)
		if err != nil {
			rel = f.File
		}
		rel = filepath.ToSlash(rel)
		for _, m := range f.Moves {
			if b.Len() > maxPRBody/2 {
				break
			}
			rows++
			fmt.Fprintf(&b, "| %s line %d | %s | %s | %s |\n", cell(rel), m.Line, cell(m.Job), cell(m.Before), cell(m.After))
			if example == "" && !strings.Contains(m.Before, "${{") {
				example = m.Before
			}
		}
		for _, r := range f.Report {
			if strings.HasPrefix(r, "left") {
				left = append(left, fmt.Sprintf("- %s: %s", cell(rel), strings.TrimPrefix(strings.TrimPrefix(r, "left"), " ")))
			}
		}
	}
	if example == "" {
		example = map[string]string{"linux": "ubuntu-latest", "macos": "macos-latest", "windows": "windows-latest"}[v.Platform]
	}
	safe := github.ForkSafeRunsOn(example, v.Platform)
	if v.All {
		b.WriteString("\n### Fork pull requests\n\n")
		b.WriteString("This pull request was made with `--all`, so fork pull requests target the pool too. Tentmaker never runs a job from a fork, so those jobs wait for a runner. To keep them on GitHub-hosted runners, run `tentmaker migrate --pr` again without `--all`; it writes this fork-safe form instead:\n\n")
	} else {
		b.WriteString("\n### Fork pull requests stay on GitHub-hosted runners\n\n")
		b.WriteString("Each moved job uses a fork-safe `runs-on` expression, so a pull request from a fork runs where it did before:\n\n")
	}
	b.WriteString(fence + "yaml\nruns-on: " + safe + "\n" + fence + "\n\n")
	b.WriteString("This is routing, not the security boundary: Tentmaker never runs a job from a fork, whatever the workflow says.\n")
	b.WriteString("\n### GitHub-hosted runners still work\n\n")
	b.WriteString("Nothing in the repo's settings changes. GitHub-hosted runners stay available as the fallback")
	if !v.All {
		b.WriteString(", and fork pull requests keep using them")
	}
	b.WriteString(". To send a job back, set its `runs-on` to the Before value above, or revert this pull request.\n")
	if len(left) > 0 {
		b.WriteString("\n### Left where they are\n\n")
		for _, l := range left {
			if b.Len() > maxPRBody {
				b.WriteString("- (more in `tentmaker migrate` output)\n")
				break
			}
			b.WriteString(l + "\n")
		}
	}
	b.WriteString("\n---\nOpened by `tentmaker migrate --pr`. Running it again rebuilds the `" + migrateBranch + "` branch and updates this pull request.\n")
	return b.String()
}

// openPR opens or updates pr with the same token sources setup uses: gh
// auth token as the invoking user, else the token the service holds,
// through the control API (the token stays in the daemon).
func (a *app) openPR(ctx context.Context, pr core.PullRequest) (core.PullRequestResult, string, error) {
	pushed := "the branch " + migrateBranch + " is pushed; "
	user := ""
	if a.env.IsRoot() && a.env.Getenv != nil {
		user = a.env.Getenv("SUDO_USER")
	}
	if a.env.GHToken != nil && a.env.OpenPR != nil {
		if tok, err := a.env.GHToken(ctx, user); err == nil && tok != "" {
			cctx, cancel := context.WithTimeout(ctx, prTimeout)
			res, err := a.env.OpenPR(cctx, tok, pr)
			cancel()
			if ce := a.rateLimitFail(err); ce != nil {
				ce.fix = pushed + ce.fix
				return res, "gh", ce
			}
			if err != nil {
				return res, "gh", fail(pushed+"check that your gh login can open pull requests on "+pr.Repo+", then run it again",
					"open the pull request with your gh login: %s", strings.ReplaceAll(err.Error(), tok, "[redacted]"))
			}
			return res, "gh", nil
		}
	}
	cfg, exists, cerr := a.loadConfig()
	if exists && cerr == nil {
		var res core.PullRequestResult
		err := a.controlClient(cfg).Call(ctx, "/v1/pr", pr, &res)
		if err == nil {
			return res, "service", nil
		}
		var ce *control.Error
		if errors.As(err, &ce) && ce.Status != 401 && ce.Status != 404 {
			return res, "service", fail(pushed+"fix that, then run it again", "the Tentmaker service could not open the pull request: %v", err)
		}
	}
	return core.PullRequestResult{}, "", fail(pushed+"sign in with `gh auth login` and run it again, or run it with sudo on a device whose service holds a GitHub token",
		"no GitHub token: `gh auth token` gave none, and the Tentmaker service could not be asked")
}

func (a *app) printMigratePR(v migratePRView) {
	if a.json {
		return
	}
	a.printMigration(v.Files, v.Changed > 0)
	if v.Changed > 0 {
		a.out("\ncommitted %s on %s and pushed it to origin\n", shortSHA(v.Commit), v.Branch)
	}
	a.out("%s\n", v.Note)
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// openPRWithToken is the default Env.OpenPR.
func openPRWithToken(ctx context.Context, token string, pr core.PullRequest) (core.PullRequestResult, error) {
	c := github.New(github.Options{Token: func(context.Context) (string, error) { return token, nil }})
	return c.OpenOrUpdatePR(ctx, pr)
}

// gitEnv is the environment git runs with: no prompts of any kind (ssh
// prompts on the terminal and ignores GIT_TERMINAL_PROMPT, so BatchMode),
// and the invoking user's ssh-agent socket, which sudo would drop.
func gitEnv(getenv func(string) string) []string {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if getenv("GIT_SSH_COMMAND") == "" && getenv("GIT_SSH") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	if sock := getenv("SSH_AUTH_SOCK"); sock != "" {
		env = append(env, "SSH_AUTH_SOCK="+sock)
	}
	return env
}

// runGit runs git -C dir args, as user through sudo when user is set and
// not root, with no terminal prompts. It returns stdout; an error carries
// git's stderr with any URL credentials removed.
func runGit(ctx context.Context, user, dir string, stdin io.Reader, args ...string) (string, error) {
	git, err := lookTool("git")
	if err != nil {
		return "", errors.New("git is not installed")
	}
	full := append([]string{"-C", dir}, args...)
	var cmd *exec.Cmd
	if user != "" && user != "root" {
		sudo, err := lookTool("sudo")
		if err != nil {
			return "", errors.New("sudo not found")
		}
		envBin, err := lookTool("env")
		if err != nil {
			return "", errors.New("env not found")
		}
		cmd = exec.CommandContext(ctx, sudo, append(append([]string{"-n", "-H", "-u", user, "--", envBin}, gitEnv(os.Getenv)...), append([]string{git}, full...)...)...)
	} else {
		cmd = exec.CommandContext(ctx, git, full...)
		cmd.Env = append(os.Environ(), gitEnv(os.Getenv)...)
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(redactURL(stderr.String()))
		if len(msg) > 400 {
			msg = msg[:400]
		}
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}
