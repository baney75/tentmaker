package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/checks"
)

// TestWorkloadsPlan runs Tentmaker's own planner over each workload file,
// as the bench's device does with .tm-scn/wf.yml: one job, named after the
// workload, nothing skipped, and every step present on GitHub's side too.
func TestWorkloadsPlan(t *testing.T) {
	for _, wi := range workloadInfo {
		path := filepath.Join("..", "..", ".github", "workflows", "bench-"+wi.Name+".yml")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", wi.Name, err)
		}
		jobs, skips, err := checks.Plan(".tm-scn/wf.yml", src, "github")
		if err != nil {
			t.Fatalf("%s: %v", wi.Name, err)
		}
		if len(skips) != 0 {
			t.Fatalf("%s: checks mode skips %+v", wi.Name, skips)
		}
		if len(jobs) != 1 || checks.Context(jobs[0]) != "tentmaker/"+wi.Name {
			t.Fatalf("%s: planned %+v, want one job with context tentmaker/%s", wi.Name, jobs, wi.Name)
		}
		last := jobs[0].Steps[len(jobs[0].Steps)-1]
		if !strings.Contains(last.Run, "scripts/bench/phase.sh report") {
			t.Fatalf("%s: the last step is %q; the report must be last so its marks are in the kept log tail", wi.Name, last.Run)
		}
		if !strings.Contains(string(src), "workflow_call:") {
			t.Fatalf("%s: bench.yml calls the workload; it needs on.workflow_call", wi.Name)
		}
	}
}

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWorkloadsRecord checks what each workload records for the gate, as
// checks mode plans it. first-F1: a Go workload pins the exact Go in go.mod
// (go-version-file is a minimum in checks mode, which took the device's
// newer Go) and records the Go it used. first-F2: a cached workload records
// whether it hit the cache, after its work and before the report.
func TestWorkloadsRecord(t *testing.T) {
	m := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(readRepo(t, "go.mod"))
	if m == nil {
		t.Fatal("no go line in go.mod")
	}
	goMod := m[1]
	for _, wi := range workloadInfo {
		src := readRepo(t, filepath.Join(".github", "workflows", "bench-"+wi.Name+".yml"))
		jobs, _, err := checks.Plan(".tm-scn/wf.yml", []byte(src), "github")
		if err != nil || len(jobs) != 1 {
			t.Fatalf("%s: %v %+v", wi.Name, err, jobs)
		}
		steps := jobs[0].Steps
		var goPin string
		var runs []string
		for _, st := range steps {
			if v, ok := st.Tool["go"]; ok {
				goPin = v
			}
			runs = append(runs, st.Run)
		}
		all := strings.Join(runs, "\n")
		if wi.Go {
			if goPin != goMod {
				t.Errorf("%s: sets up Go %q; want exactly %q, go.mod's", wi.Name, goPin, goMod)
			}
			if regexp.MustCompile(`(?m)^\s+go-version-file:`).MatchString(src) {
				t.Errorf("%s: uses go-version-file, which checks mode reads as a minimum", wi.Name)
			}
			if !strings.Contains(all, `phase.sh meta go "$(go env GOVERSION)"`) {
				t.Errorf("%s: doesn't record its Go", wi.Name)
			}
		}
		if wi.Cached {
			// The step before the report records the hit; the cache
			// workload records it as it restores.
			before := steps[len(steps)-2].Run
			if !strings.Contains(before, "scripts/bench/hit.sh ") && !strings.Contains(all, "scripts/bench/blob.sh hit") {
				t.Errorf("%s: doesn't record whether it hit the cache (the step before the report is %q)", wi.Name, before)
			}
		}
	}
}

// TestCacheKeys: second-F7. With the run attempt in the key, "Re-run failed
// jobs" sent rerun warm reps to a key no cold rep had saved.
func TestCacheKeys(t *testing.T) {
	src := readRepo(t, filepath.Join(".github", "workflows", "bench.yml"))
	keys := regexp.MustCompile(`(?m)^\s+key: (.*)$`).FindAllStringSubmatch(src, -1)
	if len(keys) != 2*len(workloadInfo) {
		t.Fatalf("found %d cache keys, want a cold and a warm one per workload (%d)", len(keys), 2*len(workloadInfo))
	}
	for _, k := range keys {
		if k[1] != "${{ github.run_id }}-${{ matrix.rep }}" {
			t.Errorf("key %q; want the run id and the rep, the same for a rep's cold and warm runs", k[1])
		}
	}
}

// TestUngatedDontFailTheRun: second-F5. A workload the gate leaves out
// can't fail the bench run: its hosted job continues on error and bench.sh
// records its failures without failing. A gated workload's job does fail.
func TestUngatedDontFailTheRun(t *testing.T) {
	m := regexp.MustCompile(`(?m)^UNGATED="([^"]*)"$`).FindStringSubmatch(readRepo(t, filepath.Join("test", "scenarios", "bench.sh")))
	if m == nil {
		t.Fatal("bench.sh has no UNGATED list")
	}
	got := strings.Fields(m[1])
	sort.Strings(got)
	var want []string
	coe := regexp.MustCompile(`(?m)^    continue-on-error: true$`)
	for _, wi := range workloadInfo {
		src := readRepo(t, filepath.Join(".github", "workflows", "bench-"+wi.Name+".yml"))
		if !wi.Gated {
			want = append(want, wi.Name)
		}
		if has := coe.MatchString(src); has == wi.Gated {
			t.Errorf("%s: job continue-on-error is %v; want %v (gated: %v)", wi.Name, has, !wi.Gated, wi.Gated)
		}
	}
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("bench.sh UNGATED = %v; the report leaves out %v", got, want)
	}
}

// fakeEngine is podman or docker for TestDockerBuildRecordsStore: it logs
// each call, answers `image exists` from FAKE_MARK, fails `build` when
// FAKE_FAIL is set, and fails a `pull` from any registry in FAKE_REFUSE.
const fakeEngine = `#!/usr/bin/env bash
echo "$(basename "$0") $*" >>"$FAKE_LOG"
case "$1 ${2:-}" in
"image exists") [ -n "${FAKE_MARK:-}" ] ;;
"build "*) [ -z "${FAKE_FAIL:-}" ] ;;
"pull "*) for r in ${FAKE_REFUSE:-}; do case "$*" in *" $r/"*) exit 1 ;; esac; done ;;
esac
`

// TestDockerBuildRecordsStore: issue 33. Under Tentmaker the Docker workload
// records whether its job got the repo's container store and whether that
// store held a passing earlier run's build, and only a passing run leaves
// the mark. The hosted side records neither. Issue 39: both sides pull the
// pinned base images from the first mirror that serves them and build from
// it, so ECR Public's data limit alone no longer fails the build.
func TestDockerBuildRecordsStore(t *testing.T) {
	const gcr, ecr = "mirror.gcr.io/library", "public.ecr.aws/docker/library"
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	bin := t.TempDir()
	for _, n := range []string{"podman", "docker"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(fakeEngine), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(t.TempDir(), "storage.conf")
	if err := os.WriteFile(conf, []byte("[storage]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name               string
		tentmaker, failing bool
		conf               string // CONTAINERS_STORAGE_CONF
		mark               bool   // the store holds the mark
		meta               string // the bench-meta file after the run
		tagged             bool   // the run left the mark
		refuse             string // registries whose pulls fail
		base               string // the BASE the build gets; "" for no build
	}{
		{"unbound", true, false, "", true, "store_bound 0\ncache_hit 0\nregistry mirror.gcr.io\n", true, "", gcr},
		{"conf named but missing", true, false, conf + ".gone", true, "store_bound 0\ncache_hit 0\nregistry mirror.gcr.io\n", true, "", gcr},
		{"bound and empty", true, false, conf, false, "store_bound 1\ncache_hit 0\nregistry mirror.gcr.io\n", true, "", gcr},
		{"bound and warm", true, false, conf, true, "store_bound 1\ncache_hit 1\nregistry mirror.gcr.io\n", true, "", gcr},
		{"failed build leaves no mark", true, true, conf, true, "store_bound 1\ncache_hit 1\nregistry mirror.gcr.io\n", false, "", gcr},
		{"hosted", false, false, "", false, "registry mirror.gcr.io\n", false, "", gcr},
		{"hosted, first mirror refuses", false, false, "", false, "registry public.ecr.aws\n", false, "mirror.gcr.io", ecr},
		{"tentmaker, first mirror refuses", true, false, conf, false, "store_bound 1\ncache_hit 0\nregistry public.ecr.aws\n", true, "mirror.gcr.io", ecr},
		{"no mirror answers", false, true, "", false, "", false, "mirror.gcr.io public.ecr.aws", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			logf := filepath.Join(tmp, "calls")
			cmd := exec.Command(bash, "scripts/bench/docker-build.sh")
			cmd.Dir = filepath.Join("..", "..")
			cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"RUNNER_TEMP=" + tmp, "FAKE_LOG=" + logf, "GITHUB_SHA=abc"}
			if c.tentmaker {
				cmd.Env = append(cmd.Env, "TENTMAKER=1")
			}
			if c.conf != "" {
				cmd.Env = append(cmd.Env, "CONTAINERS_STORAGE_CONF="+c.conf)
			}
			if c.mark {
				cmd.Env = append(cmd.Env, "FAKE_MARK=1")
			}
			if c.failing && c.base != "" {
				cmd.Env = append(cmd.Env, "FAKE_FAIL=1")
			}
			if c.refuse != "" {
				cmd.Env = append(cmd.Env, "FAKE_REFUSE="+c.refuse)
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != c.failing {
				t.Fatalf("exit error %v, want failing %v:\n%s", err, c.failing, out)
			}
			meta, _ := os.ReadFile(filepath.Join(tmp, "bench-meta"))
			if string(meta) != c.meta {
				t.Errorf("bench-meta = %q, want %q", meta, c.meta)
			}
			calls, _ := os.ReadFile(logf)
			tagged := strings.Contains(string(calls), "podman tag localhost/tm-bench:abc localhost/tm-bench-mark:latest")
			if tagged != c.tagged {
				t.Errorf("left the mark %v, want %v; calls:\n%s", tagged, c.tagged, calls)
			}
			if !c.tentmaker && strings.Contains(string(calls), "podman") {
				t.Errorf("the hosted side ran podman:\n%s", calls)
			}
			built := regexp.MustCompile(`(?m) build --build-arg BASE=(\S+) `).FindStringSubmatch(string(calls))
			switch {
			case c.base == "" && built != nil:
				t.Errorf("built from %s with no mirror answering; calls:\n%s", built[1], calls)
			case c.base != "" && (built == nil || built[1] != c.base):
				t.Errorf("build BASE = %v, want %s; calls:\n%s", built, c.base, calls)
			}
			if c.base != "" && !strings.Contains(string(calls), " pull -q "+c.base+"/golang:1.27-alpine@sha256:") {
				t.Errorf("no pinned pull from %s; calls:\n%s", c.base, calls)
			}
		})
	}
}

// Issue 39: bench.sh's cache reset stops podman's namespace unit before it
// deletes the repo cache. Deleting the bound container store under that
// unit's podman made it spin about 24 s when the daemon's reset stopped it,
// and every cold rep after a reset started that much later.
func TestBenchResetStopsPodnsFirst(t *testing.T) {
	src := readRepo(t, filepath.Join("test", "scenarios", "bench.sh"))
	i := strings.Index(src, "\nreset_caches() {")
	if i < 0 {
		t.Fatal("bench.sh has no reset_caches")
	}
	body := src[i:]
	body = body[:strings.Index(body, "\n}")]
	stop := strings.Index(body, "systemctl stop tentmaker-podns.service")
	del := strings.Index(body, "find /var/cache/tentmaker/repos")
	if stop < 0 || del < 0 || stop > del {
		t.Fatalf("reset_caches must stop tentmaker-podns.service before it deletes the repo cache:\n%s", body)
	}
}
