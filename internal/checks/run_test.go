//go:build unix

package checks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// harness serves a repo archive and runs jobs against it.
type harness struct {
	t      *testing.T
	srv    *httptest.Server
	out    string
	header string
}

func newHarness(t *testing.T, files []entry) *harness {
	t.Helper()
	entries := []entry{{name: "repo-" + testSHA[:7] + "/", typ: '5'}}
	for _, f := range files {
		f.name = "repo-" + testSHA[:7] + "/" + f.name
		entries = append(entries, f)
	}
	data := tarGz(t, entries)
	h := &harness{t: t, header: "Authorization: Bearer test-token"}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(h.srv.Close)
	root := t.TempDir()
	h.out = filepath.Join(root, "checks", "job-1")
	t.Setenv("TENTMAKER_TOOLS", "")
	return h
}

func (h *harness) run(job core.CheckJob) (core.CheckResult, string) {
	h.t.Helper()
	if job.Name == "" {
		job.Name = "test"
	}
	if job.Workflow == "" {
		job.Workflow = ".github/workflows/ci.yml"
	}
	b := core.CheckBundle{Repo: "o/r", SHA: testSHA, ArchiveURL: h.srv.URL + "/archive.tar.gz", Header: h.header, Job: job}
	var live strings.Builder
	res := RunBundle(context.Background(), b, h.out, &live)
	logData, err := os.ReadFile(filepath.Join(h.out, "log.txt"))
	if err != nil {
		h.t.Fatal(err)
	}
	if live.String() != string(logData) {
		h.t.Errorf("live log differs from log.txt:\nlive %q\nfile %q", live.String(), logData)
	}
	var onDisk core.CheckResult
	data, err := os.ReadFile(filepath.Join(h.out, "result.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk.State != res.State || onDisk.Context != res.Context {
		h.t.Fatalf("result.json = %s (%v)", data, err)
	}
	if strings.Contains(string(logData), "test-token") || strings.Contains(string(data), "test-token") {
		h.t.Fatal("the download header leaked into the log or result")
	}
	return res, string(logData)
}

func bash(name, run string) core.CheckStep {
	return core.CheckStep{Name: name, Run: run, Shell: "bash"}
}

func TestRunSuccess(t *testing.T) {
	h := newHarness(t, []entry{{name: "hello.txt", body: "hi from the repo\n"}, {name: "sub/x", body: "x"}})
	res, log := h.run(core.CheckJob{Name: "Go", Steps: []core.CheckStep{
		bash("", "cat hello.txt\necho second line"),
		{Name: "in sub", Run: "pwd; ls", Shell: "bash", WorkingDirectory: "sub"},
		{Name: "sh", Run: "echo from sh", Shell: "sh"},
		bash("sha", "echo sha=${{ github.sha }}"),
	}})
	if res.State != "success" || res.Context != "tentmaker/Go" || res.SHA != testSHA || res.Repo != "o/r" {
		t.Fatalf("res = %+v\n%s", res, log)
	}
	host, _ := os.Hostname()
	if !strings.HasPrefix(res.Description, host+", ") || !strings.HasSuffix(res.Description, "s") {
		t.Fatalf("description = %q", res.Description)
	}
	for _, want := range []string{"$ cat hello.txt\n", "hi from the repo", "second line", "/src/sub\n", "from sh", "sha=" + testSHA} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if !strings.Contains(res.LogTail, "hi from the repo") {
		t.Errorf("log tail = %q", res.LogTail)
	}
	for _, gone := range []string{"source.tar.gz", "runner-temp", "scripts"} {
		if _, err := os.Stat(filepath.Join(h.out, gone)); err == nil {
			t.Errorf("%s left behind", gone)
		}
	}
}

func TestRunFailingStepStops(t *testing.T) {
	h := newHarness(t, nil)
	res, log := h.run(core.CheckJob{Steps: []core.CheckStep{
		bash("ok", "echo first"),
		bash("boom", "echo before; false; echo after-false"),
		bash("never", "echo never-runs"),
	}})
	if res.State != "failure" || !strings.HasPrefix(res.Description, "failed at step boom, ") {
		t.Fatalf("res = %+v", res)
	}
	if strings.Contains(log, "\nafter-false") || strings.Contains(log, "never-runs") {
		t.Fatalf("ran past the failure:\n%s", log)
	}
}

func TestRunPipefail(t *testing.T) {
	h := newHarness(t, nil)
	res, _ := h.run(core.CheckJob{Steps: []core.CheckStep{bash("pipe", "false | cat")}})
	if res.State != "failure" {
		t.Fatalf("pipefail not on: %+v", res)
	}
}

func TestRunContinueOnError(t *testing.T) {
	h := newHarness(t, nil)
	soft := bash("soft", "exit 3")
	soft.ContinueOnError = true
	res, log := h.run(core.CheckJob{Steps: []core.CheckStep{soft, bash("after", "echo still-here")}})
	if res.State != "success" || !strings.Contains(log, "still-here") || !strings.Contains(log, "exit 3") {
		t.Fatalf("res = %+v\n%s", res, log)
	}
}

func TestRunTimeoutKillsTheProcessGroup(t *testing.T) {
	h := newHarness(t, nil)
	start := time.Now()
	res, log := h.run(core.CheckJob{Timeout: time.Second, Steps: []core.CheckStep{
		bash("hang", `sleep 30 & echo $! > "$GITHUB_WORKSPACE/../bg.pid"; sleep 30`),
	}})
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %s", took)
	}
	if res.State != "failure" || !strings.HasPrefix(res.Description, "timed out at step hang") {
		t.Fatalf("res = %+v\n%s", res, log)
	}
	b, err := os.ReadFile(filepath.Join(h.out, "bg.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background sleep %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunRefusesTraversalArchive(t *testing.T) {
	data := tarGz(t, []entry{{name: "repo/../../escape", body: "x"}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	root := t.TempDir()
	out := filepath.Join(root, "checks", "job")
	b := core.CheckBundle{Repo: "o/r", SHA: testSHA, ArchiveURL: srv.URL, Job: core.CheckJob{Name: "t", Steps: []core.CheckStep{bash("x", "echo ran")}}}
	res := RunBundle(context.Background(), b, out, nil)
	if res.State != "error" || !strings.Contains(res.Description, "refusing") {
		t.Fatalf("res = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); err == nil {
		t.Fatal("wrote outside the output folder")
	}
	if strings.Contains(res.LogTail, "ran") && !strings.Contains(res.LogTail, "refusing") {
		t.Fatalf("log = %s", res.LogTail)
	}
}

func TestRunBadHeaderIsAnError(t *testing.T) {
	h := newHarness(t, nil)
	h.header = "Authorization: Bearer wrong"
	res, _ := h.run(core.CheckJob{Steps: []core.CheckStep{bash("x", "echo ran")}})
	if res.State != "error" || !strings.Contains(res.Description, "401") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunMissingToolSaysHowToFixIt(t *testing.T) {
	h := newHarness(t, nil)
	tools := filepath.Join(t.TempDir(), "tools")
	t.Setenv("TENTMAKER_TOOLS", tools)
	res, log := h.run(core.CheckJob{Steps: []core.CheckStep{
		{Name: "Set up go", Tool: map[string]string{"go": "9.99"}},
		bash("never", "echo never-runs"),
	}})
	if res.State != "failure" || !strings.HasPrefix(res.Description, "failed at step Set up go") {
		t.Fatalf("res = %+v", res)
	}
	for _, want := range []string{"go 9.99.x is not installed on this device", "Fix: put go 9.99.x on PATH", filepath.Join(tools, "go-9.99.x", "bin", "go"), "TENTMAKER_TOOLS"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "never-runs") {
		t.Fatal("ran past the missing tool")
	}
}

// fakeTool writes an executable that prints version for any arguments.
func fakeTool(t *testing.T, dir, name, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho " + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunFindsToolsUnderTheToolsFolder(t *testing.T) {
	h := newHarness(t, []entry{{name: "go.mod", body: "module x\n\ngo 1.24\n"}})
	// Default tools folder: <outDir>/../../tools.
	tools := filepath.Join(filepath.Dir(filepath.Dir(h.out)), "tools")
	fakeTool(t, filepath.Join(tools, "zig-0.14.1", "bin"), "zig", "0.14.1")
	fakeTool(t, filepath.Join(tools, "zig-0.15.2", "bin"), "zig", "0.15.2")
	fakeTool(t, filepath.Join(tools, "go-1.25.3", "bin"), "go", "go version go1.25.3 linux/amd64")
	t.Setenv("PATH", "/usr/bin:/bin")
	res, log := h.run(core.CheckJob{Steps: []core.CheckStep{
		{Name: "zig", Tool: map[string]string{"zig": "0.15.2"}},
		{Name: "go", Tool: map[string]string{"go": "file:go.mod"}},
		bash("use", "zig version; go version"),
	}})
	if res.State != "success" {
		t.Fatalf("res = %+v\n%s", res, log)
	}
	for _, want := range []string{"using zig 0.15.2 from " + filepath.Join(tools, "zig-0.15.2", "bin"), "using go 1.25.3", "\n0.15.2\ngo version go1.25.3"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestRunNixShellFallsBackToHostTools(t *testing.T) {
	h := newHarness(t, []entry{{name: "linux/flake.nix", body: "{ devShells.default = mkShell { packages = [ pkgs.zig_0_15 ]; }; }\n"}})
	tools := filepath.Join(t.TempDir(), "tools")
	fakeTool(t, filepath.Join(tools, "zig-0.15.2"), "zig", "0.15.2")
	t.Setenv("TENTMAKER_TOOLS", tools)
	t.Setenv("PATH", "/usr/bin:/bin")
	res, log := h.run(core.CheckJob{Steps: []core.CheckStep{
		bash("dev shell", "nix develop ./linux --command bash -euc 'zig version && echo in-shell'"),
	}})
	if res.State != "success" || !strings.HasSuffix(res.Description, ", host libs for the nix shell") {
		t.Fatalf("res = %+v\n%s", res, log)
	}
	if !strings.Contains(log, "0.15.2\nin-shell") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestRunEnvPrecedence(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("FROM_HOST", "host")
	t.Setenv("LEVEL", "host")
	step := bash("print", `echo "LEVEL=$LEVEL JOBONLY=$JOBONLY HOST=$FROM_HOST SHA=$GITHUB_SHA CI=$CI TM=$TENTMAKER OS=$RUNNER_OS LATER=$LATER"; test -d "$RUNNER_TEMP"; test "$GITHUB_WORKSPACE" = "$PWD"`)
	step.Env = map[string]string{"LEVEL": "step"}
	res, log := h.run(core.CheckJob{
		// Plan merges workflow < job into the job's env.
		Env: map[string]string{"LEVEL": "job", "JOBONLY": "job", "GITHUB_SHA": "spoofed"},
		Steps: []core.CheckStep{
			bash("export", `echo "LATER=from-github-env" >> "$GITHUB_ENV"; printf 'MULTI<<EOF\na\nb\nEOF\n' >> "$GITHUB_ENV"`),
			step,
			bash("multi", `test "$MULTI" = "$(printf 'a\nb')" && echo multi-ok`),
		},
	})
	if res.State != "success" {
		t.Fatalf("res = %+v\n%s", res, log)
	}
	want := "LEVEL=step JOBONLY=job HOST=host SHA=" + testSHA + " CI=true TM=1 OS=" + runnerOS(hostOS()) + " LATER=from-github-env"
	if !strings.Contains(log, want) || !strings.Contains(log, "multi-ok") {
		t.Fatalf("log lacks %q:\n%s", want, log)
	}
}

func TestRunGitLabVariables(t *testing.T) {
	h := newHarness(t, nil)
	res, log := h.run(core.CheckJob{
		Workflow: ".gitlab-ci.yml",
		Env:      map[string]string{"GOPATH": "$CI_PROJECT_DIR/.go", "LIT": "$$HOME", "CHAIN": "${GOPATH}/bin"},
		Steps:    []core.CheckStep{bash("script", `echo "GOPATH=$GOPATH LIT=$LIT CHAIN=$CHAIN SHORT=$CI_COMMIT_SHORT_SHA"`)},
	})
	src := filepath.Join(h.out, "src")
	want := "GOPATH=" + src + "/.go LIT=$HOME CHAIN=" + src + "/.go/bin SHORT=" + testSHA[:7]
	if res.State != "success" || !strings.Contains(log, want) {
		t.Fatalf("log lacks %q:\n%s", want, log)
	}
}

func TestRunRefNameUnknownIsAnError(t *testing.T) {
	h := newHarness(t, nil)
	res, _ := h.run(core.CheckJob{Steps: []core.CheckStep{bash("ref", "echo ${{ github.ref_name }}")}})
	if res.State != "error" || !strings.Contains(res.Description, "ref_name") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunMissingWorkingDirectory(t *testing.T) {
	h := newHarness(t, nil)
	res, log := h.run(core.CheckJob{Steps: []core.CheckStep{{Name: "wd", Run: "true", Shell: "bash", WorkingDirectory: "nope"}}})
	if res.State != "failure" || !strings.Contains(log, "working-directory nope doesn't exist") {
		t.Fatalf("res = %+v\n%s", res, log)
	}
}
