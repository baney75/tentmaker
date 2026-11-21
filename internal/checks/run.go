package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

const (
	logTailBytes  = 64 << 10
	maxDescRunes  = 140
	envFileLimit  = 1 << 20
	stepWaitDelay = 5 * time.Second
)

// RunBundle runs one planned job: it downloads the source archive, unpacks
// it into outDir/src, runs the steps as local-ci.nu does, and writes
// outDir/result.json and outDir/log.txt. log, when not nil, gets a live
// copy of the log. The job's timeout covers the download too.
//
// Tools for setup steps come from PATH or $TENTMAKER_TOOLS/<tool>-<version>/
// (default <outDir>/../../tools); checks mode never downloads toolchains.
func RunBundle(ctx context.Context, b core.CheckBundle, outDir string, log io.Writer) core.CheckResult {
	start := time.Now()
	host, _ := os.Hostname()
	if host == "" {
		host = "this device"
	}
	res := core.CheckResult{Runner: host, Repo: b.Repo, SHA: b.SHA, Context: Context(b.Job)}
	fail := func(desc string) core.CheckResult {
		res.State, res.Description, res.Duration = "error", desc, time.Since(start)
		return res
	}
	out, err := filepath.Abs(outDir)
	if err != nil {
		return fail("can't use the output folder")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fail("can't create the output folder")
	}
	logPath := filepath.Join(out, "log.txt")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fail("can't create log.txt")
	}
	tools := os.Getenv("TENTMAKER_TOOLS")
	if tools == "" {
		tools = filepath.Join(out, "..", "..", "tools")
	}
	r := &runner{
		b:      b,
		ref:    b.Ref,
		out:    out,
		src:    filepath.Join(out, "src"),
		log:    lf,
		host:   host,
		start:  start,
		finder: toolFinder{dir: filepath.Clean(tools), src: filepath.Join(out, "src")},
	}
	t := startTail(logPath, log)
	state, desc := r.run(ctx)
	r.reap()
	r.logf("\ntentmaker: %s: %s\n", state, desc)
	t.stop()
	lf.Close()

	res.State = state
	res.Description = truncRunes(desc, maxDescRunes)
	res.Duration = time.Since(start)
	res.LogTail = readTail(logPath, logTailBytes)
	if data, err := json.MarshalIndent(res, "", "  "); err == nil {
		if err := config.WriteAtomic(filepath.Join(out, "result.json"), append(data, '\n'), 0o644); err != nil {
			res.State, res.Description = "error", "can't write result.json"
		}
	}
	return res
}

type runner struct {
	b       core.CheckBundle
	out     string
	src     string
	scripts string
	temp    string
	ref     string // branch name, when the bundle knows it
	log     *os.File
	host    string
	start   time.Time
	finder  toolFinder
	notes   []string
	pids    []int
}

func (r *runner) logf(format string, a ...any) { fmt.Fprintf(r.log, format, a...) }

func (r *runner) header(cmd string) { r.logf("\n$ %s\n", cmd) }

func (r *runner) secs() int { return int(time.Since(r.start).Round(time.Second) / time.Second) }

func (r *runner) failed(step string) (string, string) {
	return "failure", fmt.Sprintf("failed at step %s, %ds", truncRunes(step, 60), r.secs())
}

func (r *runner) run(ctx context.Context) (state, desc string) {
	job := r.b.Job
	timeout := job.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	r.logf("tentmaker check-run: %s (%s) at %s on %s\n", job.Name, job.Workflow, short(r.b.SHA), r.host)
	if r.ref == "" && usesRef(job) {
		r.logf("tentmaker: the job uses ${{ github.ref_name }}, but the bundle doesn't say which branch this is\n")
		return "error", "the job needs github.ref_name, which the bundle doesn't carry"
	}

	_ = os.RemoveAll(r.src)
	archive := filepath.Join(r.out, "source.tar.gz")
	if local, ok, err := localArchive(r.b.ArchiveURL); ok {
		if err != nil {
			r.logf("tentmaker: couldn't read the source: %v\n", err)
			return "error", "couldn't read the source: " + err.Error()
		}
		archive = local
	} else {
		r.logf("tentmaker: downloading the source\n")
		if err := download(jctx, newDownloadClient(), r.b.ArchiveURL, r.b.Header, archive); err != nil {
			_ = os.Remove(archive)
			r.logf("tentmaker: couldn't download the source: %v\n", err)
			return "error", "couldn't download the source: " + err.Error()
		}
		defer os.Remove(archive)
	}
	err := extract(archive, r.src)
	if err != nil {
		r.logf("tentmaker: couldn't unpack the source: %v\n", err)
		return "error", "couldn't unpack the source: " + err.Error()
	}

	r.temp = filepath.Join(r.out, "runner-temp")
	r.scripts = filepath.Join(r.out, "scripts")
	for _, d := range []string{r.temp, r.scripts} {
		_ = os.RemoveAll(d)
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "error", "can't create " + filepath.Base(d)
		}
		defer os.RemoveAll(d)
	}

	var toolDirs, ghPath []string
	ghEnv := map[string]string{}
	for i, st := range job.Steps {
		name := stepName(st)
		files := r.stepFiles(i)
		env := r.stepEnv(st, toolDirs, ghPath, ghEnv, files)
		pathList, _ := env.get("PATH")

		if len(st.Tool) > 0 {
			keys := make([]string, 0, len(st.Tool))
			for k := range st.Tool {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, tool := range keys {
				spec := st.Tool[tool]
				r.header(strings.TrimSpace("use " + tool + " " + spec))
				dir, v, err := r.finder.find(jctx, tool, spec, pathList)
				if err != nil {
					r.logf("%v\n", err)
					return r.failed(name)
				}
				r.logf("using %s %s from %s\n", tool, v, dir)
				toolDirs = append([]string{dir}, toolDirs...)
				pathList = dir + string(os.PathListSeparator) + pathList
			}
			continue
		}

		script := r.subst(st.Run)
		shell := st.Shell
		if shell == "" {
			shell = "bash"
		}
		if shell != "pwsh" {
			if m := nixDevelop.FindStringSubmatch(script); m != nil && lookPathIn("nix", pathList) == "" {
				dirs, err := r.finder.nixShellTools(jctx, filepath.Join(r.src, filepath.FromSlash(m[1]), "flake.nix"), pathList)
				if err != nil {
					r.header(firstLine(script))
					r.logf("tentmaker: nix isn't installed and the flake's tools aren't on this device either:\n%v\n", err)
					return r.failed(name)
				}
				toolDirs = append(dirs, toolDirs...)
				script = script[len(m[0]):]
				r.note("host libs for the nix shell")
				env = r.stepEnv(st, toolDirs, ghPath, ghEnv, files)
				pathList, _ = env.get("PATH")
			}
		}

		wd := r.src
		if st.WorkingDirectory != "" {
			w := r.subst(st.WorkingDirectory)
			if filepath.IsAbs(w) {
				wd = w
			} else {
				wd = filepath.Join(r.src, filepath.FromSlash(w))
			}
		}
		r.header(firstLine(script))
		if fi, err := os.Stat(wd); err != nil || !fi.IsDir() {
			r.logf("tentmaker: working-directory %s doesn't exist\n", st.WorkingDirectory)
			return r.failed(name)
		}

		code, err := r.exec(jctx, i, shell, script, wd, pathList, env.list())
		if jctx.Err() != nil {
			if ctx.Err() != nil {
				r.logf("tentmaker: cancelled\n")
				return "error", fmt.Sprintf("cancelled at step %s, %ds", truncRunes(name, 60), r.secs())
			}
			r.logf("tentmaker: the job ran past its %s timeout; stopped it\n", timeout)
			return "failure", fmt.Sprintf("timed out at step %s, %ds", truncRunes(name, 60), r.secs())
		}
		if err != nil {
			r.logf("tentmaker: %v\n", err)
			return r.failed(name)
		}
		if err := r.collect(files, ghEnv, &ghPath); err != nil {
			r.logf("tentmaker: %v\n", err)
			return r.failed(name)
		}
		if code != 0 {
			if st.ContinueOnError {
				r.logf("tentmaker: the step failed (exit %d); continue-on-error lets the job go on\n", code)
				continue
			}
			r.logf("tentmaker: the step failed (exit %d)\n", code)
			return r.failed(name)
		}
	}
	desc = fmt.Sprintf("%s, %ds", r.host, r.secs())
	if len(r.notes) > 0 {
		desc += ", " + strings.Join(r.notes, ", ")
	}
	return "success", desc
}

func (r *runner) note(n string) {
	if !contains(r.notes, n) {
		r.notes = append(r.notes, n)
	}
}

func (r *runner) subst(s string) string {
	s = strings.ReplaceAll(s, shaExpr, r.b.SHA)
	return strings.ReplaceAll(s, refExpr, r.ref)
}

func usesRef(job core.CheckJob) bool {
	has := func(s string) bool { return strings.Contains(s, refExpr) }
	for _, v := range job.Env {
		if has(v) {
			return true
		}
	}
	for _, st := range job.Steps {
		if has(st.Run) || has(st.WorkingDirectory) {
			return true
		}
		for _, v := range st.Env {
			if has(v) {
				return true
			}
		}
	}
	return false
}

func stepName(st core.CheckStep) string {
	switch {
	case st.Name != "":
		return st.Name
	case len(st.Tool) > 0:
		for t := range st.Tool {
			return "Set up " + t
		}
	}
	return "Run " + firstLine(st.Run)
}

// stepFiles are the per-step GITHUB_ENV, GITHUB_PATH, GITHUB_OUTPUT and
// GITHUB_STEP_SUMMARY files.
type stepFiles struct{ env, path, output, summary string }

func (r *runner) stepFiles(i int) stepFiles {
	n := fmt.Sprintf("-%d", i+1)
	f := stepFiles{
		env:     filepath.Join(r.scripts, "env"+n),
		path:    filepath.Join(r.scripts, "path"+n),
		output:  filepath.Join(r.scripts, "output"+n),
		summary: filepath.Join(r.scripts, "summary"+n),
	}
	for _, p := range []string{f.env, f.path, f.output, f.summary} {
		_ = os.WriteFile(p, nil, 0o600)
	}
	return f
}

// stepEnv layers the environment: this process < CI and TENTMAKER < the
// job's env (workflow < job, merged by Plan) < GITHUB_ENV from earlier
// steps < the step's env < the runner's own GITHUB_* and RUNNER_* values,
// which a workflow can't override (as on GitHub). PATH gets the tool
// folders and GITHUB_PATH entries in front.
func (r *runner) stepEnv(st core.CheckStep, toolDirs, ghPath []string, ghEnv map[string]string, f stepFiles) *envSet {
	e := newEnvSet(os.Environ())
	e.set("CI", "true")
	e.set("TENTMAKER", "1")
	user := map[string]string{}
	for _, layer := range []map[string]string{r.b.Job.Env, ghEnv, st.Env} {
		for _, k := range sortedKeys(layer) {
			v := r.subst(layer[k])
			e.set(k, v)
			user[e.norm(k)] = v
		}
	}
	protect := func(k, v string) {
		e.set(k, v)
		delete(user, e.norm(k))
	}
	protect("GITHUB_SHA", r.b.SHA)
	protect("GITHUB_WORKSPACE", r.src)
	protect("GITHUB_REPOSITORY", r.b.Repo)
	if r.ref != "" {
		protect("GITHUB_REF_NAME", r.ref)
	}
	protect("RUNNER_OS", runnerOS(hostOS()))
	protect("RUNNER_ARCH", runnerArch())
	protect("RUNNER_TEMP", r.temp)
	protect("GITHUB_ENV", f.env)
	protect("GITHUB_PATH", f.path)
	protect("GITHUB_OUTPUT", f.output)
	protect("GITHUB_STEP_SUMMARY", f.summary)
	if isGitLabJob(r.b.Job) {
		protect("CI_PROJECT_DIR", r.src)
		protect("CI_COMMIT_SHA", r.b.SHA)
		protect("CI_COMMIT_SHORT_SHA", short(r.b.SHA))
		if r.ref != "" {
			protect("CI_COMMIT_REF_NAME", r.ref)
		}
		// GitLab expands $VAR references in variables.
		lookup := func(name string) (string, bool) { return e.get(name) }
		for _, k := range e.order {
			if raw, ok := user[k]; ok {
				e.vals[k] = expandVars(raw, lookup, user, 0)
			}
		}
	}
	base, _ := e.get("PATH")
	parts := append(append(append([]string{}, toolDirs...), ghPath...), base)
	e.set("PATH", strings.Join(nonEmpty(parts), string(os.PathListSeparator)))
	return e
}

// expandVars expands $NAME and ${NAME}; $$ is a literal $. References to
// other user variables expand recursively (a few levels deep); unknown names
// stay as written.
func expandVars(s string, lookup func(string) (string, bool), user map[string]string, depth int) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		if s[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		var name, raw string
		if s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteByte(s[i])
				continue
			}
			name, raw = s[i+2:i+2+end], s[i:i+3+end]
		} else {
			j := i + 1
			for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			name, raw = s[i+1:j], s[i:j]
		}
		if name == "" {
			b.WriteByte(s[i])
			continue
		}
		v, ok := lookup(name)
		if !ok {
			b.WriteString(raw)
		} else if u, isUser := user[normKey(name)]; isUser && depth < 5 {
			b.WriteString(expandVars(u, lookup, user, depth+1))
		} else {
			b.WriteString(v)
		}
		i += len(raw) - 1
	}
	return b.String()
}

// exec runs one step's script and returns its exit code. An error means the
// step couldn't start.
func (r *runner) exec(ctx context.Context, i int, shell, script, dir, pathList string, env []string) (int, error) {
	ext, body := ".sh", script
	if shell == "pwsh" {
		ext = ".ps1"
		body = "$ErrorActionPreference = 'stop'\n" + script + "\nif ((Test-Path -LiteralPath variable:\\LASTEXITCODE)) { exit $LASTEXITCODE }\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	file := filepath.Join(r.scripts, fmt.Sprintf("step-%d%s", i+1, ext))
	if err := os.WriteFile(file, []byte(body), 0o700); err != nil {
		return -1, fmt.Errorf("can't write the step script: %v", err)
	}
	var args []string
	switch shell {
	case "sh":
		args = []string{"-e", filepath.ToSlash(file)}
	case "pwsh":
		args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", ". '" + strings.ReplaceAll(file, "'", "''") + "'"}
	default:
		shell = "bash"
		args = []string{"--noprofile", "--norc", "-eo", "pipefail", filepath.ToSlash(file)}
	}
	bin := lookPathIn(shell, pathList)
	if bin == "" {
		return -1, fmt.Errorf("%s is not installed on this device; the step needs it", shell)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// The inherited PWD names check-run's own folder; shells trust it.
	cmd.Env = append(env, "PWD="+dir)
	cmd.Stdout = r.log
	cmd.Stderr = r.log
	cmd.WaitDelay = stepWaitDelay
	setProcGroup(cmd)
	cmd.Cancel = func() error {
		if err := killTree(cmd.Process.Pid); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("can't start %s: %v", shell, err)
	}
	r.pids = append(r.pids, cmd.Process.Pid)
	err := cmd.Wait()
	if cmd.ProcessState == nil {
		return -1, err
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) && !errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
		return -1, err
	}
	return cmd.ProcessState.ExitCode(), nil
}

// reap kills whatever the steps left running, as the GitHub runner does at
// the end of a job.
func (r *runner) reap() {
	for _, pid := range r.pids {
		reapGroup(pid)
	}
}

// collect reads what a step wrote to GITHUB_ENV and GITHUB_PATH.
func (r *runner) collect(f stepFiles, ghEnv map[string]string, ghPath *[]string) error {
	data, err := readLimited(f.env, envFileLimit)
	if err != nil {
		return err
	}
	pairs, err := parseEnvFile(data)
	if err != nil {
		return err
	}
	for _, p := range pairs {
		ghEnv[p[0]] = p[1]
	}
	data, err = readLimited(f.path, envFileLimit)
	if err != nil {
		return err
	}
	for _, l := range strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			*ghPath = append([]string{l}, *ghPath...)
		}
	}
	return nil
}

func readLimited(p string, limit int64) (string, error) {
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", fmt.Errorf("%s is over 1 MiB", filepath.Base(p))
	}
	return string(b), nil
}

// parseEnvFile reads GITHUB_ENV: NAME=value lines and NAME<<DELIM blocks.
func parseEnvFile(data string) ([][2]string, error) {
	var out [][2]string
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		eq := strings.IndexByte(l, '=')
		if k, delim, ok := strings.Cut(l, "<<"); ok && (eq < 0 || eq > len(k)) {
			var val []string
			found := false
			for i++; i < len(lines); i++ {
				if lines[i] == delim {
					found = true
					break
				}
				val = append(val, lines[i])
			}
			if !found || k == "" {
				return nil, fmt.Errorf("GITHUB_ENV: %s<<%s has no closing %s line", k, delim, delim)
			}
			out = append(out, [2]string{k, strings.Join(val, "\n")})
			continue
		}
		if eq <= 0 {
			return nil, fmt.Errorf("GITHUB_ENV: a line has no NAME=value")
		}
		out = append(out, [2]string{l[:eq], l[eq+1:]})
	}
	return out, nil
}

// envSet is an ordered environment; names are case-insensitive on Windows.
type envSet struct {
	order []string
	names map[string]string
	vals  map[string]string
}

func newEnvSet(pairs []string) *envSet {
	e := &envSet{names: map[string]string{}, vals: map[string]string{}}
	for _, p := range pairs {
		if k, v, ok := strings.Cut(p, "="); ok && k != "" {
			e.set(k, v)
		}
	}
	return e
}

func normKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

func (e *envSet) norm(k string) string { return normKey(k) }

func (e *envSet) set(k, v string) {
	n := normKey(k)
	if _, ok := e.vals[n]; !ok {
		e.order = append(e.order, n)
		e.names[n] = k
	}
	e.vals[n] = v
}

func (e *envSet) get(k string) (string, bool) {
	v, ok := e.vals[normKey(k)]
	return v, ok
}

func (e *envSet) list() []string {
	out := make([]string, 0, len(e.order))
	for _, n := range e.order {
		out = append(out, e.names[n]+"="+e.vals[n])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func nonEmpty(list []string) []string {
	var out []string
	for _, s := range list {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isGitLabJob(j core.CheckJob) bool {
	base := filepath.Base(filepath.FromSlash(j.Workflow))
	return base == ".gitlab-ci.yml" || base == ".gitlab-ci.yaml"
}

func hostOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func runnerArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "ARM64"
	case "amd64":
		return "X64"
	}
	return strings.ToUpper(runtime.GOARCH)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func truncRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-3]) + "..."
}

func readTail(p string, n int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := fi.Size() - n
	if off < 0 {
		off = 0
	}
	b := make([]byte, fi.Size()-off)
	k, _ := f.ReadAt(b, off)
	return string(b[:k])
}

// tail mirrors log.txt to the caller's writer while the job runs. Steps
// write straight to the file (no pipe), so a background process a step
// leaves behind can't hold the step open.
type tail struct {
	done chan struct{}
	wg   sync.WaitGroup
}

func startTail(path string, w io.Writer) *tail {
	t := &tail{done: make(chan struct{})}
	if w == nil {
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return t
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer f.Close()
		buf := make([]byte, 32<<10)
		drain := func() {
			for {
				n, err := f.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n])
				}
				if n == 0 || err != nil {
					return
				}
			}
		}
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-t.done:
				drain()
				return
			case <-tick.C:
				drain()
			}
		}
	}()
	return t
}

func (t *tail) stop() {
	close(t.done)
	t.wg.Wait()
}
