package checks

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func plan(t *testing.T, file, forge string) ([]core.CheckJob, []Skip) {
	t.Helper()
	src, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	jobs, skips, err := Plan(file, src, forge)
	if err != nil {
		t.Fatalf("Plan(%s): %v", file, err)
	}
	return jobs, skips
}

func ids(jobs []core.CheckJob) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.ID)
	}
	return out
}

func skipFor(t *testing.T, skips []Skip, job string) string {
	t.Helper()
	for _, s := range skips {
		if s.Job == job {
			return s.Reason
		}
	}
	t.Fatalf("no skip for %q in %+v", job, skips)
	return ""
}

func TestPlanGoCI(t *testing.T) {
	jobs, skips := plan(t, "go-ci.yml", "github")
	if got := ids(jobs); !reflect.DeepEqual(got, []string{"test"}) {
		t.Fatalf("jobs = %v", got)
	}
	j := jobs[0]
	if j.Name != "Go" || j.Platform != (core.Platform{OS: "linux", Arch: "x64"}) || j.Timeout != 20*time.Minute {
		t.Fatalf("job = %+v", j)
	}
	if Context(j) != "tentmaker/Go" {
		t.Fatalf("Context = %q", Context(j))
	}
	// Workflow env < job env, merged by Plan; the step keeps its own.
	if j.Env["LEVEL"] != "job" || j.Env["GOFLAGS"] != "-trimpath" {
		t.Fatalf("env = %v", j.Env)
	}
	if len(j.Steps) != 5 {
		t.Fatalf("steps = %+v", j.Steps)
	}
	if !reflect.DeepEqual(j.Steps[0].Tool, map[string]string{"go": "file:go.mod"}) {
		t.Fatalf("setup-go = %+v", j.Steps[0])
	}
	if j.Steps[2].Env["LEVEL"] != "step" {
		t.Fatalf("step env = %v", j.Steps[2].Env)
	}
	if j.Steps[4].Run != `echo "built ${{ github.sha }} on Linux"` {
		t.Fatalf("expressions: %q", j.Steps[4].Run)
	}
	for _, s := range j.Steps {
		if s.Shell != "bash" {
			t.Fatalf("shell = %q", s.Shell)
		}
	}
	if r := skipFor(t, skips, "integration"); !strings.Contains(r, "services") {
		t.Fatalf("integration: %s", r)
	}
	if r := skipFor(t, skips, "publish"); !strings.Contains(r, "${{ secrets.TOKEN }}") {
		t.Fatalf("publish: %s", r)
	}
	if r := skipFor(t, skips, "lint"); !strings.Contains(r, "golangci/golangci-lint-action@v6") {
		t.Fatalf("lint: %s", r)
	}
}

func TestPlanRustCI(t *testing.T) {
	jobs, skips := plan(t, "rust-ci.yml", "github")
	if got := ids(jobs); !reflect.DeepEqual(got, []string{"test"}) {
		t.Fatalf("jobs = %v", got)
	}
	j := jobs[0]
	if j.Platform != (core.Platform{OS: "linux", Arch: "arm64"}) {
		t.Fatalf("platform = %+v", j.Platform)
	}
	if j.Env["CARGO_TERM_COLOR"] != "always" || j.Env["RUSTFLAGS"] != "-D warnings" {
		t.Fatalf("env = %v", j.Env)
	}
	// checkout, rust-cache and actions/cache drop out; dtolnay maps to a tool.
	if len(j.Steps) != 4 || !reflect.DeepEqual(j.Steps[0].Tool, map[string]string{"rust": "stable"}) {
		t.Fatalf("steps = %+v", j.Steps)
	}
	if j.Steps[3].WorkingDirectory != "crates" || j.Steps[3].Name != "Run cargo test --workspace" {
		t.Fatalf("step = %+v", j.Steps[3])
	}
	if r := skipFor(t, skips, "msrv"); !strings.Contains(r, "fetch-depth") {
		t.Fatalf("msrv: %s", r)
	}
}

func TestPlanNodeMatrix(t *testing.T) {
	jobs, skips := plan(t, "node-matrix.yml", "github")
	if got := ids(jobs); !reflect.DeepEqual(got, []string{"test", "wide", "wide", "wide"}) {
		t.Fatalf("jobs = %v", got)
	}
	j := jobs[0]
	if j.Name != "test (20, ubuntu-latest)" || j.Platform.OS != "linux" {
		t.Fatalf("job = %+v", j)
	}
	if !reflect.DeepEqual(j.Steps[0].Tool, map[string]string{"node": "20"}) {
		t.Fatalf("setup-node = %+v", j.Steps[0])
	}
	if j.Steps[2].Run != "npm test -- --runner=Linux-20" {
		t.Fatalf("run = %q", j.Steps[2].Run)
	}
	// Every combination is its own job with its own status, as on GitHub.
	var wide []string
	for _, j := range jobs[1:] {
		wide = append(wide, j.Name)
	}
	if !reflect.DeepEqual(wide, []string{"wide (18)", "wide (20)", "wide (22)"}) {
		t.Fatalf("wide = %v", wide)
	}
	if r := skipFor(t, skips, "included"); !strings.Contains(r, "${{ matrix.missing }}") {
		t.Fatalf("included: %s", r)
	}
}

func TestPlanReleaseHasNoJobs(t *testing.T) {
	jobs, skips := plan(t, "release.yml", "github")
	if len(jobs) != 0 || len(skips) != 0 {
		t.Fatalf("jobs %v skips %v", jobs, skips)
	}
}

func TestPlanLocalStudio(t *testing.T) {
	jobs, skips := plan(t, "local-studio-ci.yml", "github")
	want := []string{"core", "linux", "channels", "macos", "macos-app", "windows"}
	if got := ids(jobs); !reflect.DeepEqual(got, want) {
		t.Fatalf("jobs = %v", got)
	}
	if len(skips) != 1 || skips[0].Job != "iPhone app" || !strings.Contains(skips[0].Reason, "hashFiles") {
		t.Fatalf("skips = %+v", skips)
	}
	byID := map[string]core.CheckJob{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	core1 := byID["core"]
	if !reflect.DeepEqual(core1.Steps[0].Tool, map[string]string{"zig": "0.15.2"}) || core1.Steps[1].Run != "rustup toolchain install" {
		t.Fatalf("core steps = %+v", core1.Steps)
	}
	if got := byID["linux"].Steps; len(got) != 1 || !strings.HasPrefix(got[0].Run, "nix develop ./linux --command ") {
		t.Fatalf("linux steps = %+v", got)
	}
	if !reflect.DeepEqual(byID["channels"].Steps[0].Tool, map[string]string{"nu": "0.117.0"}) {
		t.Fatalf("channels = %+v", byID["channels"].Steps[0])
	}
	if p := byID["macos"].Platform; p != (core.Platform{OS: "macos", Arch: "arm64"}) {
		t.Fatalf("macos platform = %+v", p)
	}
	app := byID["macos-app"].Steps
	if last := app[len(app)-1]; last.Name != "App (Debug)" || !last.ContinueOnError {
		t.Fatalf("macos-app last step = %+v", last)
	}
	win := byID["windows"]
	if win.Platform != (core.Platform{OS: "windows", Arch: "x64"}) {
		t.Fatalf("windows platform = %+v", win.Platform)
	}
	if s := win.Steps[1]; s.Shell != "pwsh" || s.Run != "rustup toolchain install" {
		t.Fatalf("windows default shell: %+v", s)
	}
	for _, j := range jobs {
		if j.Env["CARGO_TERM_COLOR"] != "always" {
			t.Fatalf("%s: workflow env missing: %v", j.ID, j.Env)
		}
	}
}

func TestPlanGitLab(t *testing.T) {
	jobs, skips := plan(t, "gitlab-ci.yml", "gitlab")
	// Stage order (lint, build, test), file order within a stage.
	if got := ids(jobs); !reflect.DeepEqual(got, []string{"vet", "compile", "unit", "always"}) {
		t.Fatalf("jobs = %v", got)
	}
	byID := map[string]core.CheckJob{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	unit := byID["unit"]
	if unit.Env["LEVEL"] != "job" || unit.Env["CGO_ENABLED"] != "0" || unit.Env["GOPATH"] != "$CI_PROJECT_DIR/.go" {
		t.Fatalf("unit env = %v", unit.Env)
	}
	if unit.Env["CI_JOB_STAGE"] != "test" || unit.Timeout != 90*time.Minute {
		t.Fatalf("unit = %+v", unit)
	}
	if len(unit.Steps) != 3 || unit.Steps[0].Run != "go version\ngo test ./..." ||
		unit.Steps[1].Run != "echo cleanup" || !unit.Steps[1].ContinueOnError {
		t.Fatalf("unit steps = %+v", unit.Steps)
	}
	if v := byID["vet"]; v.Steps[0].Run != "echo default-before\ngo vet ./..." || v.Env["LEVEL"] != "global" || v.Platform.OS != "linux" {
		t.Fatalf("vet = %+v", v)
	}
	if c := byID["compile"]; c.Platform != (core.Platform{OS: "linux", Arch: "arm64"}) {
		t.Fatalf("compile platform = %+v", c.Platform)
	}
	if a := byID["always"]; a.Steps[0].Run != "echo always" {
		t.Fatalf("inherit default false: %+v", a.Steps)
	}
	for job, want := range map[string]string{
		"branch-only":      "if:",
		"deploy":           "manual",
		"with-db":          "services",
		"too-deep":         "one level",
		"missing-template": ".nowhere",
		"old-style":        "only:",
	} {
		if r := skipFor(t, skips, job); !strings.Contains(r, want) {
			t.Errorf("%s: %q lacks %q", job, r, want)
		}
	}
	for _, s := range skips {
		if strings.HasPrefix(s.Job, ".") {
			t.Errorf("hidden job %s listed as skipped", s.Job)
		}
	}
}

func TestPlanGitLabWorkflowRulesAndSpec(t *testing.T) {
	src := []byte("workflow:\n  rules:\n    - if: $CI_PIPELINE_SOURCE == \"push\"\ntest:\n  script: [make]\n")
	jobs, skips, err := Plan(".gitlab-ci.yml", src, "gitlab")
	if err != nil || len(jobs) != 0 || len(skips) != 1 || !strings.Contains(skips[0].Reason, "workflow:rules") {
		t.Fatalf("jobs %v skips %v err %v", jobs, skips, err)
	}
	src = []byte("spec:\n  inputs:\n    target: {default: x}\n---\nbuild:\n  script:\n    - make $[[ inputs.target ]]\nplain:\n  script: [make]\n")
	jobs, skips, err = Plan(".gitlab-ci.yml", src, "gitlab")
	if err != nil || !reflect.DeepEqual(ids(jobs), []string{"plain"}) || len(skips) != 1 || skips[0].Job != "build" {
		t.Fatalf("jobs %v skips %v err %v", ids(jobs), skips, err)
	}
}

func TestPlanTimeouts(t *testing.T) {
	for _, c := range []struct {
		yaml string
		want time.Duration
	}{
		{"", 60 * time.Minute},
		{"timeout-minutes: 15", 15 * time.Minute},
		{"timeout-minutes: 500", 6 * time.Hour},
	} {
		src := "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    " + c.yaml + "\n    steps:\n      - run: make\n"
		jobs, _, err := Plan("w.yml", []byte(src), "github")
		if err != nil || len(jobs) != 1 || jobs[0].Timeout != c.want {
			t.Errorf("%q: jobs %+v err %v", c.yaml, jobs, err)
		}
	}
}

func TestPlanRefusals(t *testing.T) {
	for _, c := range []struct{ name, job, want string }{
		{"job if", "if: github.event_name == 'push'\n    steps: [{run: make}]", "if:"},
		{"container", "container: node:20\n    steps: [{run: make}]", "container"},
		{"step if", "steps:\n      - run: make\n        if: always()", "if:"},
		{"other shell", "steps:\n      - run: print(1)\n        shell: python", "shell: python"},
		{"docker action", "steps:\n      - uses: docker://alpine", "docker://alpine"},
		{"env expression", "env:\n      T: ${{ secrets.T }}\n    steps: [{run: make}]", "${{ secrets.T }}"},
		{"only checkout", "steps:\n      - uses: actions/checkout@v4", "nothing to run"},
		{"unknown key", "steps:\n      - run: make\n        retries: 3", "retries"},
	} {
		src := "on: pull_request\njobs:\n  a:\n    runs-on: ubuntu-latest\n    " + c.job + "\n"
		jobs, skips, err := Plan("w.yml", []byte(src), "github")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(jobs) != 0 || len(skips) != 1 || !strings.Contains(skips[0].Reason, c.want) {
			t.Errorf("%s: jobs %v skips %+v, want reason with %q", c.name, jobs, skips, c.want)
		}
	}
	src := "on: push\ndefaults:\n  run:\n    shell: sh\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: make}]\n"
	if jobs, skips, _ := Plan("w.yml", []byte(src), "github"); len(jobs) != 0 || len(skips) != 1 {
		t.Errorf("workflow defaults: jobs %v skips %v", jobs, skips)
	}
	if _, _, err := Plan("w.yml", []byte("on: push"), "bitbucket"); err == nil {
		t.Error("unknown forge: no error")
	}
	if _, _, err := Plan("w.yml", []byte("on: [push\n"), "github"); err == nil {
		t.Error("broken YAML: no error")
	}
}

func TestPlanUploadArtifactIsANote(t *testing.T) {
	src := "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make\n      - uses: actions/upload-artifact@v4\n        if: always()\n        with:\n          path: ${{ runner.temp }}/out\n"
	jobs, _, err := Plan("w.yml", []byte(src), "github")
	if err != nil || len(jobs) != 1 || len(jobs[0].Steps) != 2 || !strings.Contains(jobs[0].Steps[1].Run, "no-op") {
		t.Fatalf("jobs %+v err %v", jobs, err)
	}
}

func TestRunsOn(t *testing.T) {
	for _, c := range []struct {
		yaml    string
		forgejo bool
		want    core.Platform
		bad     bool
	}{
		{"ubuntu-latest", false, core.Platform{OS: "linux", Arch: "x64"}, false},
		{"ubuntu-24.04-arm", false, core.Platform{OS: "linux", Arch: "arm64"}, false},
		{"macos-latest", false, core.Platform{OS: "macos", Arch: "arm64"}, false},
		{"macos-13", false, core.Platform{OS: "macos", Arch: "x64"}, false},
		{"macos-15-large", false, core.Platform{OS: "macos", Arch: "x64"}, false},
		{"windows-2025", false, core.Platform{OS: "windows", Arch: "x64"}, false},
		{"[self-hosted, tentmaker, linux]", false, core.Platform{OS: "linux"}, false},
		{"[self-hosted, macOS, ARM64]", false, core.Platform{OS: "macos", Arch: "arm64"}, false},
		{"{group: big, labels: [linux, x64]}", false, core.Platform{OS: "linux", Arch: "x64"}, false},
		{"docker", true, core.Platform{OS: "linux"}, false},
		{"docker", false, core.Platform{}, true},
		{"{group: big}", false, core.Platform{}, true},
		{"[self-hosted, linux, windows]", false, core.Platform{}, true},
		{"my-runner", false, core.Platform{}, true},
	} {
		v, err := parseYAML([]byte(c.yaml))
		if err != nil {
			t.Fatal(err)
		}
		p, reason := runsOn(v, exprCtx{}, c.forgejo)
		if c.bad != (reason != "") || (!c.bad && p != c.want) {
			t.Errorf("%s: got %+v %q", c.yaml, p, reason)
		}
	}
}

func TestMatrixCombos(t *testing.T) {
	for _, c := range []struct {
		yaml string
		n    int
	}{
		{"{os: [a, b], v: [1, 2]}", 4},
		{"{os: [a, b], v: [1, 2], exclude: [{os: a}]}", 2},
		{"{os: [a], include: [{os: a, extra: x}]}", 1},
		{"{os: [a], include: [{os: b}]}", 2},
		{"{include: [{os: a}]}", 1},
	} {
		v, _ := parseYAML([]byte(c.yaml))
		got, err := matrixCombos(v)
		if err != nil || len(got) != c.n {
			t.Errorf("%s: %d combos, err %v", c.yaml, len(got), err)
		}
	}
	v, _ := parseYAML([]byte("{os: [a], include: [{os: a, extra: x}]}"))
	got, _ := matrixCombos(v)
	if got[0].get("extra").str() != "x" {
		t.Errorf("include didn't extend the combo: %s", got[0].text())
	}
	v, _ = parseYAML([]byte("matrix: ${{ fromJSON(needs.a.outputs.m) }}"))
	if _, err := matrixCombos(v.get("matrix")); err == nil {
		t.Error("expression matrix: no error")
	}
}

func TestExpressions(t *testing.T) {
	m, _ := parseYAML([]byte("{go: '1.25', cfg: {os: linux}}"))
	c := exprCtx{matrix: m, runnerOS: "macOS", forgejo: true}
	for in, want := range map[string]string{
		"go ${{ matrix.go }}":                "go 1.25",
		"${{matrix.cfg.os}}":                 "linux",
		"${{ matrix['go'] }}":                "1.25",
		"${{ runner.os }}-x":                 "macOS-x",
		"${{ github.sha }}/${{ forge.sha }}": shaExpr + "/" + shaExpr,
		"${{ github.ref_name }}":             refExpr,
		"plain":                              "plain",
	} {
		got, err := c.resolve(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"${{ github.event_name }}", "${{ matrix.cfg }}", "${{ secrets.X }}", "${{ unclosed"} {
		if _, err := c.resolve(in); err == nil {
			t.Errorf("%q resolved", in)
		}
	}
}

func TestYAMLAnchorsAndMerge(t *testing.T) {
	v, err := parseYAML([]byte(".base: &base\n  a: 1\n  b: 2\njob:\n  <<: *base\n  b: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	j := v.get("job")
	if j.get("a").str() != "1" || j.get("b").str() != "3" {
		t.Fatalf("merge: %s", j.text())
	}
}

func TestGitLabDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"1h 30m": 90 * time.Minute, "3 hours": 3 * time.Hour, "45 minutes": 45 * time.Minute, "3600": time.Hour,
	} {
		if got, ok := gitlabDuration(in); !ok || got != want {
			t.Errorf("%q: %v %v", in, got, ok)
		}
	}
	if _, ok := gitlabDuration("soon"); ok {
		t.Error("soon parsed")
	}
	// Each part fits in a Duration but the sum does not. Summed as
	// Durations it wraps to about 0.3s; it must read as a long timeout.
	if got, ok := gitlabDuration("2562047h 2562047h 94m 34s"); !ok || got < maxTimeout {
		t.Errorf("an overflowing sum: %v %v, want at least %v", got, ok, maxTimeout)
	}
	if got, ok := gitlabDuration("99999999999 weeks"); !ok || got < maxTimeout {
		t.Errorf("a huge timeout: %v %v, want at least %v", got, ok, maxTimeout)
	}
}

// A matrix job runs once per combination. Each needs its own status
// context, so a fixed name gets the combination's values, as GitHub shows
// it; a combination checks mode can't run is skipped on its own.
func TestPlanMatrixCombinations(t *testing.T) {
	src := []byte(`on: push
jobs:
  test:
    name: test (${{ matrix.os }})
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - run: go test ./...
  fixed:
    name: Unit tests
    strategy:
      matrix:
        go: ["1.24", "1.25"]
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ matrix.go }}
  mixed:
    strategy:
      matrix:
        os: [ubuntu-latest, plan9-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - run: echo hi
`)
	jobs, skips, err := Plan("ci.yml", src, "github")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range jobs {
		got = append(got, Context(j)+" "+j.Platform.OS)
	}
	want := []string{
		"tentmaker/test (ubuntu-latest) linux", "tentmaker/test (macos-latest) macos", "tentmaker/test (windows-latest) windows",
		"tentmaker/Unit tests (1.24) linux", "tentmaker/Unit tests (1.25) linux",
		"tentmaker/mixed (ubuntu-latest) linux",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("jobs\n got %q\nwant %q", got, want)
	}
	if jobs[4].Steps[0].Run != "echo 1.25" {
		t.Fatalf("step = %q", jobs[4].Steps[0].Run)
	}
	if r := skipFor(t, skips, "mixed (plan9-latest)"); r == "" {
		t.Fatal("no reason")
	}
}

// A YAML alias bomb: nine levels of eight aliases is 8^9 nodes once
// expanded. Plan must refuse it quickly instead of eating the broker's
// memory.
func TestPlanRefusesAliasBomb(t *testing.T) {
	var b strings.Builder
	b.WriteString("l0: &l0 [x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, "l%d: &l%d [*l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d]\n", i, i, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1)
	}
	b.WriteString("on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n")
	for _, forge := range []string{"github", "gitlab"} {
		start := time.Now()
		_, _, err := Plan("x.yml", []byte(b.String()), forge)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Errorf("%s: Plan = %v, want a too-large error", forge, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: Plan took %v", forge, d)
		}
	}
}

// A matrix whose product is astronomically large must be refused before
// it is expanded.
func TestPlanRefusesHugeMatrix(t *testing.T) {
	var b strings.Builder
	b.WriteString("on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "        k%d: [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]\n", i)
	}
	b.WriteString("    steps:\n      - run: echo hi\n")
	start := time.Now()
	jobs, skips, err := Plan("x.yml", []byte(b.String()), "github")
	if err != nil || len(jobs) != 0 || len(skips) != 1 || !strings.Contains(skips[0].Reason, "combinations") {
		t.Fatalf("Plan = %v %v %v", jobs, skips, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Plan took %v", d)
	}
}
