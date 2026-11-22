package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/repocache"
)

func cachedSpec(name, kind, repo string) core.ExecSpec {
	s := spec(name, kind)
	s.Cache = core.CacheRepo{Forge: "github", Repo: repo}
	return s
}

// unitBinds returns the BindPaths= values of a job's systemd-run.
func unitBinds(c Cmd) []string {
	var out []string
	for i, a := range c.Args {
		if a == "-p" && i+1 < len(c.Args) && strings.HasPrefix(c.Args[i+1], "BindPaths=") {
			out = append(out, strings.TrimPrefix(c.Args[i+1], "BindPaths="))
		}
	}
	return out
}

func unitEnv(c Cmd) map[string]string {
	out := map[string]string{}
	for _, a := range c.Args {
		if kv, ok := strings.CutPrefix(a, "--setenv="); ok {
			k, val, _ := strings.Cut(kv, "=")
			out[k] = val
		}
	}
	return out
}

// Each job binds its own repo's cache, and nothing else, at the one mount
// point; the env names only that mount point; the store's root is 0700 and
// a job with no cache gets none of it.
func TestRepoCacheIsConfinedToItsJob(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	must(t, v.e.Start(ctx, cachedSpec("job-other", "", "acme/other")))
	must(t, v.e.Start(ctx, spec("chk-cold", core.KindCheck)))

	mount := v.e.cacheMount()
	root := filepath.Join(v.o.CacheDir, "repos")
	for name, repo := range map[string]string{"chk-app": "acme/app", "job-other": "acme/other"} {
		c := findUnit(t, v.f, unitName(name))
		own, _ := v.e.cache.Path(core.CacheRepo{Forge: "github", Repo: repo})
		binds := unitBinds(c)
		if len(binds) != 1 || binds[0] != own+":"+mount {
			t.Fatalf("%s binds %q, want only %s:%s", name, binds, own, mount)
		}
		env := unitEnv(c)
		for _, k := range []string{"GOCACHE", "GOMODCACHE", "RUNNER_TOOL_CACHE", "AGENT_TOOLSDIRECTORY", "npm_config_cache", "PIP_CACHE_DIR", "CARGO_HOME", "XDG_CACHE_HOME"} {
			if !strings.HasPrefix(env[k], mount+"/") {
				t.Errorf("%s: %s=%q is not under the job's cache mount %s", name, k, env[k], mount)
			}
		}
		// No path in the unit names the store itself except its own bind.
		args := strings.Join(c.Args, "\n")
		if strings.Count(args, root) != 1 {
			t.Errorf("%s: the cache store is named %d times in its unit, want once (its own bind)", name, strings.Count(args, root))
		}
		if v.f.chowns[own] != [2]int{testUID, testGID} {
			t.Errorf("%s: its cache is not handed to the runner: %v", name, v.f.chowns[own])
		}
	}
	// The runner job's script is told its slot folder.
	if c := findUnit(t, v.f, unitName("job-other")); c.Args[len(c.Args)-1] != filepath.Base(v.e.jobDir("job-other")) {
		t.Fatalf("job script gets %q, want the slot %s", c.Args[len(c.Args)-1], filepath.Base(v.e.jobDir("job-other")))
	}
	fi, err := os.Stat(root)
	must(t, err)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache root mode %v, want 0700", fi.Mode().Perm())
	}
	if v.f.chowns[root] != [2]int{} {
		t.Fatalf("the cache root was chowned: %v", v.f.chowns[root])
	}

	cold := findUnit(t, v.f, unitName("chk-cold"))
	if b := unitBinds(cold); len(b) != 0 {
		t.Fatalf("a job with no cache binds %q", b)
	}
	env := unitEnv(cold)
	if _, ok := env["GOCACHE"]; ok {
		t.Fatalf("a job with no cache is pointed at one: GOCACHE=%s", env["GOCACHE"])
	}
	if env["RUNNER_TOOL_CACHE"] != v.e.jobDir("chk-cold")+"/toolcache" {
		t.Fatalf("cold job tool cache %q", env["RUNNER_TOOL_CACHE"])
	}
	if v.e.jobDir("chk-cold") != filepath.Join(v.e.jobsDir(), "chk-cold") {
		t.Fatalf("a job with no cache got a slot: %s", v.e.jobDir("chk-cold"))
	}
}

// A repo's jobs reuse one folder path, so Go's cache keys (which include
// the source folder) match; jobs of the same repo at the same time get
// different folders.
func TestSlotsAreStablePerRepo(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	key, _ := repocache.Key(core.CacheRepo{Forge: "github", Repo: "acme/app"})
	slot := func(n string) string { return filepath.Join(v.e.jobsDir(), key[:12]+"-"+n) }

	must(t, v.e.Start(ctx, cachedSpec("chk-one", core.KindCheck, "acme/app")))
	must(t, v.e.Start(ctx, cachedSpec("chk-two", core.KindCheck, "acme/app")))
	if v.e.jobDir("chk-one") != slot("0") || v.e.jobDir("chk-two") != slot("1") {
		t.Fatalf("slots %s %s", v.e.jobDir("chk-one"), v.e.jobDir("chk-two"))
	}
	c := findUnit(t, v.f, unitName("chk-one"))
	if !strings.Contains(strings.Join(c.Args, " "), "--working-directory="+slot("0")+"/work") {
		t.Fatalf("unit does not run in its slot: %q", c.Args)
	}
	// The slot's result is read from the slot.
	must(t, os.WriteFile(filepath.Join(slot("0"), "out", "result.json"), []byte(`{"state":"success","description":"ok"}`), 0o600))
	if r, err := v.e.CheckResult(ctx, "chk-one"); err != nil || r.State != "success" {
		t.Fatalf("CheckResult from the slot: %+v %v", r, err)
	}
	// List names jobs, not slot folders.
	v.f.respond = showResponder("inactive", "not-found", false)
	got, err := v.e.List(ctx)
	must(t, err)
	if strings.Join(got, ",") != "chk-one,chk-two" {
		t.Fatalf("List = %v", got)
	}

	inUse, err := v.e.cacheInUse()
	must(t, err)
	if !inUse[key] {
		t.Fatal("a repo with jobs on the host is not in use")
	}
	must(t, v.e.Clean(ctx, "chk-one"))
	if _, err := os.Stat(slot("0")); !os.IsNotExist(err) {
		t.Fatal("Clean left the slot folder")
	}
	if _, ok, _ := v.e.slotOf("chk-one"); ok {
		t.Fatal("Clean left the slot record")
	}
	// The next job of the repo gets the freed folder back.
	v.f.respond = nil
	must(t, v.e.Start(ctx, cachedSpec("chk-three", core.KindCheck, "acme/app")))
	if v.e.jobDir("chk-three") != slot("0") {
		t.Fatalf("next job got %s, want the freed %s", v.e.jobDir("chk-three"), slot("0"))
	}
	// A leftover slot folder no record names is listed by its own name, so
	// it is cleaned, and is never handed out.
	must(t, os.Mkdir(slot("2"), 0o700))
	v.f.respond = showResponder("inactive", "not-found", false)
	got, err = v.e.List(ctx)
	must(t, err)
	if strings.Join(got, ",") != "chk-three,chk-two,"+key[:12]+"-2" {
		t.Fatalf("List = %v", got)
	}
	v.f.respond = nil
	must(t, v.e.Start(ctx, cachedSpec("chk-four", core.KindCheck, "acme/app")))
	if v.e.jobDir("chk-four") != slot("3") {
		t.Fatalf("got %s, want %s past the leftover", v.e.jobDir("chk-four"), slot("3"))
	}
}

// A cache that can't be prepared leaves the job cold, not failed.
func TestCacheFailureRunsCold(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	root := filepath.Join(v.o.CacheDir, "repos")
	must(t, os.MkdirAll(filepath.Dir(root), 0o755))
	must(t, os.WriteFile(root, []byte("not a folder"), 0o600))
	must(t, v.e.Start(context.Background(), cachedSpec("chk-app", core.KindCheck, "acme/app")))
	c := findUnit(t, v.f, unitName("chk-app"))
	if b := unitBinds(c); len(b) != 0 {
		t.Fatalf("binds %q with a broken store", b)
	}
	if _, ok, _ := v.e.slotOf("chk-app"); ok {
		t.Fatal("a cold job kept a slot")
	}
	if !strings.Contains(v.logs.String(), "without its repo cache") {
		t.Fatal("no warning that the job runs cold")
	}
}

// cache_max = 0 turns the caches off.
func TestCacheOff(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	v.o.CacheMax = -1
	v.e = New(v.o)
	must(t, v.e.Start(context.Background(), cachedSpec("chk-app", core.KindCheck, "acme/app")))
	if b := unitBinds(findUnit(t, v.f, unitName("chk-app"))); len(b) != 0 {
		t.Fatalf("binds %q with the cache off", b)
	}
}

func TestResetDropsSlots(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	must(t, v.e.Start(context.Background(), cachedSpec("chk-app", core.KindCheck, "acme/app")))
	must(t, os.WriteFile(v.e.baselinePath(), []byte(""), 0o600))
	v.f.respond = func(c Cmd) (Result, error) { return Result{}, nil }
	must(t, v.e.Reset(context.Background()))
	if held, _ := v.e.slots(); len(held) != 0 {
		t.Fatalf("slots after reset: %v", held)
	}
	p, _ := v.e.cache.Path(core.CacheRepo{Forge: "github", Repo: "acme/app"})
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("Reset must keep repo caches: %v", err)
	}
}

// bin/ is hard-linked from the root-owned template, never chowned (that
// would hand the template's inode to the CI user); writable files and the
// top level are copied.
func TestRunnerBinIsHardLinkedNotChowned(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	tpl, err := filepath.EvalSymlinks(v.e.template())
	must(t, err)
	must(t, os.WriteFile(filepath.Join(tpl, "bin", "writable.dll"), []byte("x"), 0o664))
	must(t, os.Chmod(filepath.Join(tpl, "bin", "writable.dll"), 0o664))
	must(t, v.e.Start(context.Background(), spec("job-abc", "")))
	jd := v.e.jobDir("job-abc")
	same := func(a, b string) bool {
		fa, err1 := os.Stat(a)
		fb, err2 := os.Stat(b)
		return err1 == nil && err2 == nil && os.SameFile(fa, fb)
	}
	listener := filepath.Join(jd, "runner", "bin", "Runner.Listener")
	if !same(listener, filepath.Join(tpl, "bin", "Runner.Listener")) {
		t.Fatal("bin/Runner.Listener is not a hard link to the template")
	}
	if _, ok := v.f.chowns[listener]; ok {
		t.Fatal("a hard link to the template was chowned")
	}
	for _, f := range []string{"run.sh", "bin/writable.dll"} {
		p := filepath.Join(jd, "runner", f)
		if same(p, filepath.Join(tpl, f)) {
			t.Fatalf("%s shares the template's inode", f)
		}
		if v.f.chowns[p] != [2]int{testUID, testGID} {
			t.Fatalf("%s is not the runner's: %v", f, v.f.chowns[p])
		}
	}
}

// BenchmarkCopyRunner measures per-job runner setup from a template at
// $TM_RUNNER_TEMPLATE (an unpacked actions-runner release).
func BenchmarkCopyRunner(b *testing.B) {
	tpl := os.Getenv("TM_RUNNER_TEMPLATE")
	if tpl == "" {
		b.Skip("set TM_RUNNER_TEMPLATE to an unpacked actions-runner release")
	}
	for _, mode := range []string{"copy", "link"} {
		b.Run(mode, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				dst := filepath.Join(b.TempDir(), "runner")
				if mode == "link" {
					if _, err := copyRunner(tpl, dst); err != nil {
						b.Fatal(err)
					}
					continue
				}
				must2(b, os.Mkdir(dst, 0o755))
				ents, _ := os.ReadDir(tpl)
				for _, ent := range ents {
					if ent.Name() == "externals" {
						continue
					}
					must2(b, copyTree(filepath.Join(tpl, ent.Name()), filepath.Join(dst, ent.Name()), nil))
				}
			}
		})
	}
}

func must2(b *testing.B, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
}

// Jobs of one repo start together and race for slots; each job name gets
// back the slot its last run used, whatever order they start in.
func TestSlotAffinityByJobName(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	ctx := context.Background()
	key, _ := repocache.Key(core.CacheRepo{Forge: "github", Repo: "acme/app"})
	slot := func(n string) string { return filepath.Join(v.e.jobsDir(), key[:12]+"-"+n) }
	job := func(name, j string) core.ExecSpec {
		s := cachedSpec(name, core.KindCheck, "acme/app")
		s.Cache.Job = j
		return s
	}
	clean := func(names ...string) {
		v.f.respond = showResponder("inactive", "not-found", false)
		for _, n := range names {
			must(t, v.e.Clean(ctx, n))
		}
		v.f.respond = nil
	}
	must(t, v.e.Start(ctx, job("chk-a1", "test")))
	must(t, v.e.Start(ctx, job("chk-b1", "build")))
	if v.e.jobDir("chk-a1") != slot("0") || v.e.jobDir("chk-b1") != slot("1") {
		t.Fatalf("first run: %s %s", v.e.jobDir("chk-a1"), v.e.jobDir("chk-b1"))
	}
	clean("chk-a1", "chk-b1")
	// The other order next time: each still gets its own folder back.
	must(t, v.e.Start(ctx, job("chk-b2", "build")))
	must(t, v.e.Start(ctx, job("chk-a2", "test")))
	if v.e.jobDir("chk-a2") != slot("0") || v.e.jobDir("chk-b2") != slot("1") {
		t.Fatalf("second run: test in %s, build in %s", v.e.jobDir("chk-a2"), v.e.jobDir("chk-b2"))
	}
	clean("chk-a2", "chk-b2")
	// A new job name takes a folder no other job last used.
	must(t, v.e.Start(ctx, job("chk-c1", "lint")))
	if v.e.jobDir("chk-c1") != slot("2") {
		t.Fatalf("new job got %s, want %s", v.e.jobDir("chk-c1"), slot("2"))
	}
	// A preference whose slot is busy falls back to a free one.
	must(t, v.e.Start(ctx, job("chk-c2", "lint")))
	if d := v.e.jobDir("chk-c2"); d == slot("2") || d == v.e.jobDir("chk-c1") {
		t.Fatalf("two jobs share %s", d)
	}
}
