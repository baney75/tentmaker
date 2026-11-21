// Package checks is Tentmaker's checks mode: it plans a repo's CI workflow
// jobs, runs them directly (no forge runner) and reports results the broker
// posts as commit statuses. It works with GitHub, Forgejo, Gitea and GitLab.
//
// It generalizes Local Studio's local-ci.nu: a job runs only when checks
// mode can reproduce it faithfully, and anything it can't (other actions,
// services, containers, expressions it can't evaluate) skips the whole job
// with a plain reason instead of running something different.
package checks

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// Skip is a workflow job Plan won't run, with the reason in plain words.
// A skipped job gets no commit status.
type Skip struct{ Job, Reason string }

const (
	defaultTimeout = 60 * time.Minute
	maxTimeout     = 6 * time.Hour
)

// Context is the commit status context for a check job.
func Context(job core.CheckJob) string {
	name := job.Name
	if name == "" {
		name = job.ID
	}
	return "tentmaker/" + name
}

// Plan reads one workflow file into the jobs checks mode can run.
// forge is "github", "forgejo" or "gitea" for Actions workflow YAML, or
// "gitlab" for .gitlab-ci.yml. A workflow that doesn't run on push or
// pull_request returns no jobs. Platform.Arch is empty when the workflow
// doesn't pin one (self-hosted labels without an arch, GitLab jobs).
func Plan(workflowPath string, src []byte, forge string) ([]core.CheckJob, []Skip, error) {
	switch forge {
	case "github", "forgejo", "gitea":
		return planActions(workflowPath, src, forge == "forgejo")
	case "gitlab":
		return planGitLab(workflowPath, src)
	}
	return nil, nil, fmt.Errorf("checks: unknown forge %q", forge)
}

func planActions(path string, src []byte, forgejo bool) ([]core.CheckJob, []Skip, error) {
	root, err := parseYAML(src)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if !root.isMap() {
		return nil, nil, fmt.Errorf("%s: a workflow must be a mapping", path)
	}
	if !triggered(root.get("on")) {
		return nil, nil, nil
	}
	jobs := root.get("jobs")
	if !jobs.isMap() {
		return nil, nil, fmt.Errorf("%s: no jobs", path)
	}
	var out []core.CheckJob
	var skips []Skip
	for _, id := range jobs.keys {
		j := jobs.m[id]
		label := id
		if n := j.get("name").str(); n != "" && !strings.Contains(n, "${{") {
			label = n
		}
		if root.has("defaults") {
			skips = append(skips, Skip{label, "the workflow sets defaults:, which checks mode can't reproduce"})
			continue
		}
		planned, skipped, reason := planActionsJob(path, id, j, root.get("env"), forgejo)
		if reason != "" {
			skips = append(skips, Skip{label, reason})
			continue
		}
		out = append(out, planned...)
		skips = append(skips, skipped...)
	}
	return out, skips, nil
}

// triggered reports whether on: includes push or pull_request.
func triggered(on *val) bool {
	switch {
	case on.isScalar():
		return on.s == "push" || on.s == "pull_request"
	case on.isSeq():
		for _, e := range on.seq {
			if e.str() == "push" || e.str() == "pull_request" {
				return true
			}
		}
	case on.isMap():
		return on.has("push") || on.has("pull_request")
	}
	return false
}

var jobKeysIgnored = map[string]bool{
	"name": true, "runs-on": true, "steps": true, "env": true, "timeout-minutes": true,
	"strategy": true, "permissions": true, "concurrency": true, "needs": true, "outputs": true,
	"continue-on-error": true,
}

// maxCombos caps one job's matrix, as GitHub does.
const maxCombos = 256

// planActionsJob plans one workflow job: one check job per matrix
// combination, each with its own status, as GitHub runs them. A
// combination checks mode can't run is skipped on its own; reason is set
// when the whole job can't run.
func planActionsJob(path, id string, j, wfEnv *val, forgejo bool) (jobs []core.CheckJob, skips []Skip, reason string) {
	if !j.isMap() {
		return nil, nil, "the job is not a mapping"
	}
	for _, k := range j.keys {
		if !jobKeysIgnored[k] {
			_, reason := planCombo(path, id, j, wfEnv, forgejo, nil, false)
			return nil, nil, reason
		}
	}
	st := j.get("strategy")
	if st == nil {
		job, reason := planCombo(path, id, j, wfEnv, forgejo, nil, false)
		if reason != "" {
			return nil, nil, reason
		}
		return []core.CheckJob{job}, nil, ""
	}
	if st.isScalar() {
		return nil, nil, "can't evaluate the strategy " + st.s
	}
	m := st.get("matrix")
	if m == nil {
		job, reason := planCombo(path, id, j, wfEnv, forgejo, nil, false)
		if reason != "" {
			return nil, nil, reason
		}
		return []core.CheckJob{job}, nil, ""
	}
	combos, err := matrixCombos(m)
	if err != nil {
		return nil, nil, err.Error()
	}
	switch {
	case len(combos) == 0:
		return nil, nil, "the matrix has no combinations"
	case len(combos) > maxCombos:
		return nil, nil, fmt.Sprintf("the matrix has %d combinations; GitHub allows at most %d", len(combos), maxCombos)
	}
	multi := len(combos) > 1
	for _, c := range combos {
		job, reason := planCombo(path, id, j, wfEnv, forgejo, c, multi)
		if reason != "" {
			if !multi {
				return nil, nil, reason
			}
			skips = append(skips, Skip{id + " (" + comboValues(c) + ")", reason})
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, skips, ""
}

// comboValues is a combination's values as GitHub shows them: "20, ubuntu-latest".
func comboValues(c *val) string {
	parts := make([]string, len(c.keys))
	for i, k := range c.keys {
		parts[i] = c.m[k].text()
	}
	return strings.Join(parts, ", ")
}

// planCombo plans one job for one matrix combination (nil: no matrix).
// multi says the job has several combinations, so a name that doesn't
// change with the matrix gets the combination's values to stay unique.
func planCombo(path, id string, j, wfEnv *val, forgejo bool, combo *val, multi bool) (core.CheckJob, string) {
	for _, k := range j.keys {
		if jobKeysIgnored[k] {
			continue
		}
		switch k {
		case "if":
			return core.CheckJob{}, "the job has if: " + j.m[k].text() + ", which checks mode can't evaluate"
		case "services":
			return core.CheckJob{}, "the job needs service containers (services:)"
		case "container":
			return core.CheckJob{}, "the job runs in a container (container:)"
		case "uses":
			return core.CheckJob{}, "the job calls a reusable workflow"
		case "defaults":
			return core.CheckJob{}, "the job sets defaults:, which checks mode can't reproduce"
		case "environment":
			return core.CheckJob{}, "the job deploys to an environment"
		default:
			return core.CheckJob{}, "the job uses " + k + ":, which checks mode doesn't support"
		}
	}

	ctx := exprCtx{forgejo: forgejo, matrix: combo}

	platform, reason := runsOn(j.get("runs-on"), ctx, forgejo)
	if reason != "" {
		return core.CheckJob{}, reason
	}
	ctx.runnerOS = runnerOS(platform.OS)

	name := id
	if n := j.get("name"); n != nil {
		r, err := ctx.resolve(n.str())
		if err != nil {
			return core.CheckJob{}, "name: " + err.Error()
		}
		name = r
		if multi && !strings.Contains(n.str(), "${{") {
			name += " (" + comboValues(combo) + ")"
		}
	} else if combo != nil && len(combo.keys) > 0 {
		name = id + " (" + comboValues(combo) + ")"
	}

	env := map[string]string{}
	for _, layer := range []*val{wfEnv, j.get("env")} {
		if reason := mergeEnv(env, layer, ctx); reason != "" {
			return core.CheckJob{}, reason
		}
	}

	timeout := defaultTimeout
	if t := j.get("timeout-minutes"); t != nil {
		r, err := ctx.resolve(t.str())
		if err != nil {
			return core.CheckJob{}, "timeout-minutes: " + err.Error()
		}
		if mins, err := strconv.ParseFloat(strings.TrimSpace(r), 64); err == nil && mins > 0 {
			timeout = time.Duration(mins * float64(time.Minute))
		}
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	steps := j.get("steps")
	if !steps.isSeq() {
		return core.CheckJob{}, "the job has no steps"
	}
	var out []core.CheckStep
	runnable := false
	for i, s := range steps.seq {
		step, reason := planStep(s, ctx, platform)
		if reason != "" {
			return core.CheckJob{}, fmt.Sprintf("step %d: %s", i+1, reason)
		}
		if step != nil {
			out = append(out, *step)
			if step.Run != "" || step.Tool != nil {
				runnable = true
			}
		}
	}
	if !runnable {
		return core.CheckJob{}, "the job has nothing to run"
	}
	return core.CheckJob{
		Workflow: path,
		ID:       id,
		Name:     name,
		Platform: platform,
		Env:      env,
		Steps:    out,
		Timeout:  timeout,
	}, ""
}

func mergeEnv(env map[string]string, layer *val, ctx exprCtx) string {
	if layer.isNull() {
		return ""
	}
	if !layer.isMap() {
		return "env " + layer.text() + " is not a mapping of literal values"
	}
	for _, k := range layer.keys {
		v := layer.m[k]
		if !v.isScalar() {
			return "env " + k + " is not a literal value"
		}
		r, err := ctx.resolve(v.str())
		if err != nil {
			return "env " + k + ": " + err.Error()
		}
		env[k] = r
	}
	return ""
}

var stepKeys = map[string]bool{
	"name": true, "id": true, "run": true, "shell": true, "env": true, "working-directory": true,
	"continue-on-error": true, "timeout-minutes": true, "uses": true, "with": true, "if": true,
}

// Actions checks mode drops: the archive is the checkout, and caches only
// make CI faster.
var droppedActions = map[string]bool{
	"actions/checkout":          true,
	"actions/cache":             true,
	"actions/cache/restore":     true,
	"actions/cache/save":        true,
	"swatinem/rust-cache":       true,
	"cachix/install-nix-action": true,
}

// setupActions maps a setup action to its tool and the with: keys holding
// the version and the version file.
var setupActions = map[string]struct{ tool, version, file string }{
	"actions/setup-go":     {"go", "go-version", "go-version-file"},
	"actions/setup-node":   {"node", "node-version", "node-version-file"},
	"actions/setup-python": {"python", "python-version", "python-version-file"},
	"actions/setup-java":   {"java", "java-version", "java-version-file"},
	"mlugg/setup-zig":      {"zig", "version", ""},
	"hustcer/setup-nu":     {"nu", "version", ""},
}

// planStep returns the step to run, nil for a step checks mode drops, or a
// reason the whole job can't run.
func planStep(s *val, ctx exprCtx, platform core.Platform) (*core.CheckStep, string) {
	if !s.isMap() {
		return nil, "the step is not a mapping"
	}
	for _, k := range s.keys {
		if !stepKeys[k] {
			return nil, "the step uses " + k + ":, which checks mode doesn't support"
		}
	}
	uses := strings.TrimSpace(s.get("uses").str())
	if uses != "" && s.has("run") {
		return nil, "the step has both uses: and run:"
	}
	if uses != "" {
		action, ref, _ := strings.Cut(uses, "@")
		key := strings.ToLower(action)
		if droppedActions[key] {
			if key == "actions/checkout" {
				if r := checkoutReason(s.get("with")); r != "" {
					return nil, r
				}
			}
			return nil, ""
		}
		if key == "actions/upload-artifact" {
			return &core.CheckStep{
				Name:  "Upload artifact (skipped)",
				Run:   "echo 'tentmaker: actions/upload-artifact is a no-op in checks mode; nothing was uploaded'",
				Shell: defaultShell(platform),
			}, ""
		}
		if r := stepIf(s); r != "" {
			return nil, r
		}
		tool, version, reason := setupTool(key, ref, s.get("with"), ctx)
		if reason != "" {
			return nil, reason
		}
		if tool == "" {
			return nil, "the step uses the action " + uses + ", which checks mode can't run"
		}
		name := "Set up " + tool
		if version != "" {
			name += " " + version
		}
		if n := s.get("name"); n != nil {
			if r, err := ctx.resolve(n.str()); err == nil {
				name = r
			}
		}
		return &core.CheckStep{Name: name, Shell: defaultShell(platform), Tool: map[string]string{tool: version}}, ""
	}

	if !s.has("run") {
		return nil, "the step has neither run: nor uses:"
	}
	if r := stepIf(s); r != "" {
		return nil, r
	}
	step := &core.CheckStep{Env: map[string]string{}}
	var err error
	if step.Run, err = ctx.resolve(s.get("run").str()); err != nil {
		return nil, err.Error() + " in: " + firstLine(s.get("run").str())
	}
	step.Name = "Run " + firstLine(step.Run)
	if n := s.get("name"); n != nil {
		if step.Name, err = ctx.resolve(n.str()); err != nil {
			return nil, "name: " + err.Error()
		}
	}
	shell := ""
	if sh := s.get("shell"); sh != nil {
		if shell, err = ctx.resolve(sh.str()); err != nil {
			return nil, "shell: " + err.Error()
		}
	}
	switch shell {
	case "":
		step.Shell = defaultShell(platform)
	case "bash", "sh", "pwsh":
		step.Shell = shell
	default:
		return nil, "shell: " + shell + " is not bash, sh or pwsh"
	}
	if wd := s.get("working-directory"); wd != nil {
		if step.WorkingDirectory, err = ctx.resolve(wd.str()); err != nil {
			return nil, "working-directory: " + err.Error()
		}
	}
	if r := mergeEnv(step.Env, s.get("env"), ctx); r != "" {
		return nil, r
	}
	if len(step.Env) == 0 {
		step.Env = nil
	}
	if c := s.get("continue-on-error"); c != nil {
		r, err := ctx.resolve(c.str())
		if err != nil {
			return nil, "continue-on-error: " + err.Error()
		}
		step.ContinueOnError = isTrue(r)
	}
	return step, ""
}

// stepIf accepts only the default condition.
func stepIf(s *val) string {
	c := s.get("if")
	if c == nil {
		return ""
	}
	e := strings.TrimSpace(c.str())
	if strings.HasPrefix(e, "${{") && strings.HasSuffix(e, "}}") {
		e = strings.TrimSpace(e[3 : len(e)-2])
	}
	if e == "success()" {
		return ""
	}
	return "the step has if: " + c.text() + ", which checks mode can't evaluate"
}

func checkoutReason(with *val) string {
	if with.isNull() {
		return ""
	}
	for _, k := range with.keys {
		v := strings.TrimSpace(with.m[k].str())
		switch k {
		case "repository", "ref", "path":
			if v != "" {
				return "actions/checkout with " + k + ": checks a different tree out than the source archive"
			}
		case "submodules", "lfs":
			if v != "" && v != "false" {
				return "actions/checkout with " + k + ": " + v + " needs files the source archive doesn't have"
			}
		case "fetch-depth":
			if v != "" && v != "1" {
				return "actions/checkout with fetch-depth: " + v + " needs git history; the source archive has none"
			}
		}
	}
	return ""
}

func setupTool(action, ref string, with *val, ctx exprCtx) (tool, version, reason string) {
	pick := func(key string) (string, string) {
		v := with.get(key)
		if v.isNull() {
			return "", ""
		}
		if !v.isScalar() {
			return "", key + " lists several versions; checks mode runs one"
		}
		r, err := ctx.resolve(v.str())
		if err != nil {
			return "", key + ": " + err.Error()
		}
		r = strings.TrimSpace(r)
		if strings.ContainsAny(r, "\n,") {
			return "", key + " lists several versions; checks mode runs one"
		}
		return r, ""
	}
	if action == "dtolnay/rust-toolchain" {
		tc := ref
		if tc == "" || tc == "master" || tc == "main" || isHexSHA(tc) || strings.HasPrefix(tc, "v1") {
			v, r := pick("toolchain")
			if r != "" {
				return "", "", r
			}
			tc = v
		}
		if tc == "" {
			return "", "", "dtolnay/rust-toolchain needs a toolchain in its ref or with.toolchain"
		}
		return "rust", tc, ""
	}
	sa, ok := setupActions[action]
	if !ok {
		return "", "", ""
	}
	v, r := pick(sa.version)
	if r != "" {
		return "", "", r
	}
	if v == "" && sa.file != "" {
		f, r := pick(sa.file)
		if r != "" {
			return "", "", r
		}
		if f != "" {
			v = "file:" + f
		}
	}
	return sa.tool, v, ""
}

func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func defaultShell(p core.Platform) string {
	if p.OS == "windows" {
		return "pwsh"
	}
	return "bash"
}

func runnerOS(os string) string {
	switch os {
	case "linux":
		return "Linux"
	case "macos":
		return "macOS"
	case "windows":
		return "Windows"
	}
	return ""
}

// runsOn maps runs-on to a platform: GitHub-hosted labels by name, and
// self-hosted label lists by their OS and arch labels.
func runsOn(v *val, ctx exprCtx, forgejo bool) (core.Platform, string) {
	var raw []string
	switch {
	case v.isMap():
		l := v.get("labels")
		if l == nil {
			return core.Platform{}, "runs-on picks a runner group, which checks mode can't map to a platform"
		}
		var ok bool
		if raw, ok = l.strings(); !ok {
			return core.Platform{}, "runs-on labels must be strings"
		}
	default:
		var ok bool
		if raw, ok = v.strings(); !ok || len(raw) == 0 {
			return core.Platform{}, "the job has no runs-on"
		}
	}
	var p core.Platform
	setOS := func(os string) string {
		if p.OS != "" && p.OS != os {
			return "runs-on names two systems (" + p.OS + " and " + os + ")"
		}
		p.OS = os
		return ""
	}
	for _, l := range raw {
		r, err := ctx.resolve(l)
		if err != nil {
			return core.Platform{}, "runs-on: " + err.Error()
		}
		ll := strings.ToLower(strings.TrimSpace(r))
		var bad string
		switch {
		case strings.HasPrefix(ll, "ubuntu"):
			bad = setOS("linux")
			p.Arch = hostedArch(ll)
		case strings.HasPrefix(ll, "macos-") || ll == "macos-latest":
			bad = setOS("macos")
			p.Arch = macArch(ll)
		case strings.HasPrefix(ll, "windows-"):
			bad = setOS("windows")
			p.Arch = hostedArch(ll)
		case ll == "linux":
			bad = setOS("linux")
		case ll == "macos" || ll == "osx" || ll == "darwin":
			bad = setOS("macos")
		case ll == "windows":
			bad = setOS("windows")
		case forgejo && (ll == "docker" || ll == "native"):
			bad = setOS("linux")
		case ll == "x64" || ll == "amd64" || ll == "x86_64":
			p.Arch = "x64"
		case ll == "arm64" || ll == "aarch64":
			p.Arch = "arm64"
		}
		if bad != "" {
			return core.Platform{}, bad
		}
	}
	if p.OS == "" {
		return core.Platform{}, "runs-on " + strings.Join(raw, ", ") + " doesn't name a system checks mode knows (linux, macos, windows)"
	}
	return p, ""
}

func hostedArch(label string) string {
	if strings.HasSuffix(label, "-arm") || strings.HasSuffix(label, "-arm64") {
		return "arm64"
	}
	return "x64"
}

// macArch: macos-13 and older and the -large sizes are Intel; the rest are
// Apple silicon.
func macArch(label string) string {
	switch {
	case strings.HasSuffix(label, "-xlarge"):
		return "arm64"
	case strings.HasSuffix(label, "-large"):
		return "x64"
	}
	rest := strings.TrimPrefix(label, "macos-")
	num, _, _ := strings.Cut(rest, "-")
	num, _, _ = strings.Cut(num, ".")
	if n, err := strconv.Atoi(num); err == nil && n < 14 {
		return "x64"
	}
	return "arm64"
}
