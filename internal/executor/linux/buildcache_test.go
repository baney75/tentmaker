//go:build !windows

package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartRunnerUsesBuildCache(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	withDLL(t, v, true)
	s := spec("job-bc", "")
	s.ActionsCache = "http://127.0.0.1:7436/"
	must(t, v.e.Start(context.Background(), s))
	jd := v.e.jobDir("job-bc")

	env, _ := os.ReadFile(filepath.Join(jd, "runner", ".env"))
	for _, l := range []string{"TURBO_API=http://127.0.0.1:7436\n", "TENTMAKER_BAZEL_CACHE=http://127.0.0.1:7436\n", "TURBO_TEAM=tentmaker\n", "TURBO_TOKEN=" + buildCacheToken + "\n"} {
		if !strings.Contains(string(env), l) {
			t.Errorf(".env lacks %q:\n%s", l, env)
		}
	}
	// No rc file and no bind: any rc that sets --remote_cache splits a
	// repo's remote execution from its cache (review r2 first-F3), and a
	// bind over a missing /etc file would touch the host (r2 first-F4).
	// Bazel opts in through TENTMAKER_BAZEL_CACHE.
	for _, p := range []string{filepath.Join(jd, "home", ".bazelrc"), filepath.Join(jd, "bazel.bazelrc")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s written: an rc --remote_cache overrides the repo's remote execution", p)
		}
	}
	if args := strings.Join(findUnit(t, v.f, unitName("job-bc")).Args, " "); strings.Contains(args, "bazel") {
		t.Errorf("job unit binds a bazelrc: %s", args)
	}
	init, err := os.ReadFile(filepath.Join(jd, "home", ".gradle", "init.d", "tentmaker-build-cache.gradle"))
	if err != nil || !strings.Contains(string(init), "c.url = 'http://127.0.0.1:7436/cache/'") ||
		!strings.Contains(string(init), "settings.buildCache.remote == null") ||
		strings.Contains(string(init), "buildCacheEnabled") {
		t.Errorf("gradle init script: %v\n%s", err, init)
	}
	// Handed to the runner user with the rest of the job dir.
	for _, p := range []string{filepath.Join(jd, "home", ".gradle", "init.d", "tentmaker-build-cache.gradle")} {
		if c, ok := v.f.chowns[p]; !ok || c == [2]int{0, 0} {
			t.Errorf("%s not chowned to the runner: %v", p, c)
		}
	}
	if args := strings.Join(findUnit(t, v.f, unitName("job-bc")).Args, " "); strings.Contains(args, "TURBO_") {
		t.Errorf("cache variables in argv: %s", args)
	}

	// A job without the cache gets none of it.
	must(t, v.e.Start(context.Background(), spec("job-nobc", "")))
	pd := v.e.jobDir("job-nobc")
	if env, _ := os.ReadFile(filepath.Join(pd, "runner", ".env")); strings.Contains(string(env), "TURBO_") {
		t.Errorf("a job without the cache has build cache variables:\n%s", env)
	}
}
