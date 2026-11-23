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
	for _, l := range []string{"TURBO_API=http://127.0.0.1:7436\n", "TURBO_TEAM=tentmaker\n", "TURBO_TOKEN=" + buildCacheToken + "\n"} {
		if !strings.Contains(string(env), l) {
			t.Errorf(".env lacks %q:\n%s", l, env)
		}
	}
	rc, err := os.ReadFile(filepath.Join(jd, "home", ".bazelrc"))
	if err != nil || !strings.Contains(string(rc), "build --remote_cache=http://127.0.0.1:7436\n") {
		t.Errorf(".bazelrc: %v\n%s", err, rc)
	}
	init, err := os.ReadFile(filepath.Join(jd, "home", ".gradle", "init.d", "tentmaker-build-cache.gradle"))
	if err != nil || !strings.Contains(string(init), "c.url = 'http://127.0.0.1:7436/cache/'") ||
		!strings.Contains(string(init), "settings.buildCache.remote == null") {
		t.Errorf("gradle init script: %v\n%s", err, init)
	}
	// Handed to the runner user with the rest of the job dir.
	for _, p := range []string{filepath.Join(jd, "home", ".bazelrc"), filepath.Join(jd, "home", ".gradle", "init.d", "tentmaker-build-cache.gradle")} {
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
	if _, err := os.Stat(filepath.Join(pd, "home", ".bazelrc")); err == nil {
		t.Error("a job without the cache has a .bazelrc")
	}
}
