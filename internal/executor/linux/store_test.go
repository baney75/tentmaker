//go:build !windows

package linux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/repocache"
)

// storeEnv is an installed env whose podman reports the bound graphroot
// (or want, when set), and whose systemctl lists the job units in active
// as running.
type storeHost struct {
	mu        sync.Mutex
	active    map[string]bool
	graphroot string
	podnsFail int // fail the PodnsUnit start once this many have run; 0 never
	podns     int
	// remoteFail makes podman's service (podman --remote) fail.
	remoteFail bool
	// pruneFail makes podman volume prune fail.
	pruneFail bool
	// atPodns records, at each PodnsUnit start, which of podman's
	// libpod/tmp files are present (stale is set by the test).
	stale   []string
	atPodns [][]string
}

func storeTestEnv(t *testing.T) (*env, *storeHost) {
	t.Helper()
	v := newEnv(t)
	v.installed(t)
	must(t, os.WriteFile(v.e.baselinePath(), []byte("podman.socket\n"), 0o600))
	sh := &storeHost{active: map[string]bool{}, graphroot: v.e.storeMount()}
	v.f.respond = func(c Cmd) (Result, error) {
		a := strings.Join(c.Args, " ")
		sh.mu.Lock()
		defer sh.mu.Unlock()
		switch {
		case c.Path == "systemd-run" && slices.Contains(c.Args, "--unit="+PodnsUnit):
			sh.podns++
			var present []string
			for _, p := range sh.stale {
				if _, err := os.Stat(p); err == nil {
					present = append(present, filepath.Base(p))
				}
			}
			sh.atPodns = append(sh.atPodns, present)
			if sh.podnsFail > 0 && sh.podns >= sh.podnsFail {
				return Result{Code: 1}, errors.New("podns failed")
			}
		case c.Path == "systemctl" && strings.HasPrefix(a, "list-units") && strings.Contains(a, UnitPrefix):
			var out string
			for n := range sh.active {
				out += unitName(n) + " loaded active running job\n"
			}
			return Result{Stdout: out}, nil
		case strings.Contains(a, "systemctl --user list-units"):
			return Result{Stdout: "podman.socket loaded active listening y\n"}, nil
		case sh.pruneFail && strings.Contains(a, "podman volume prune"):
			return Result{Code: 125, Stderr: "volume in use"}, errors.New("exit status 125")
		case sh.remoteFail && strings.Contains(a, "podman --remote"):
			return Result{Code: 125, Stderr: "Cannot connect to Podman"}, errors.New("exit status 125")
		case strings.Contains(a, "info --format"):
			return Result{Stdout: sh.graphroot + "\n"}, nil
		}
		return Result{}, nil
	}
	return v, sh
}

func (sh *storeHost) run(name string) {
	sh.mu.Lock()
	sh.active[name] = true
	sh.mu.Unlock()
}

// podnsStarts returns the PodnsUnit systemd-run argvs, in order.
func podnsStarts(f *fakeHost) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.cmds {
		if c.Path == "systemd-run" && slices.Contains(c.Args, "--unit="+PodnsUnit) {
			out = append(out, c.Args)
		}
	}
	return out
}

func storePath(t *testing.T, v *env, repo string) string {
	t.Helper()
	p, err := v.e.cache.ContainersPath(core.CacheRepo{Forge: "github", Repo: repo})
	must(t, err)
	return p
}

// The first cached job on an idle device binds its repo's store into
// podman's namespaces (PodnsUnit and the user manager) and points podman,
// the service and the job's own CLI at it; the store sits under root-owned
// folders, never in the job-writable repo folder.
func TestContainerStoreBindsOnIdleDevice(t *testing.T) {
	v, _ := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))

	store := storePath(t, v, "acme/app")
	bind := "BindPaths=" + store + ":" + v.e.storeMount()
	starts := podnsStarts(v.f)
	if len(starts) != 1 || !slices.Contains(starts[0], bind) {
		t.Fatalf("podns starts %q, want one with %s", starts, bind)
	}
	drop, err := os.ReadFile(v.e.managerDropIn(testUID))
	must(t, err)
	if !strings.Contains(string(drop), bind+"\n") {
		t.Fatalf("user manager drop-in lacks the store bind:\n%s", drop)
	}
	conf, err := os.ReadFile(v.e.storageConf())
	must(t, err)
	if !strings.Contains(string(conf), `graphroot = "`+v.e.storeMount()+`"`) ||
		!strings.Contains(string(conf), `runroot = "`+v.e.storeRunRoot(testUID)+`"`) {
		t.Fatalf("storage.conf:\n%s", conf)
	}
	job := findUnit(t, v.f, unitName("chk-app"))
	if !slices.Contains(unitBinds(job), store+":"+v.e.storeMount()) {
		t.Fatalf("job binds %q, want its store too", unitBinds(job))
	}
	if got := unitEnv(job)["CONTAINERS_STORAGE_CONF"]; got != v.e.storageConf() {
		t.Fatalf("job CONTAINERS_STORAGE_CONF=%q", got)
	}
	if v.f.chowns[store] != [2]int{testUID, testGID} {
		t.Fatalf("the store is not handed to the runner: %v", v.f.chowns[store])
	}
	repoDir, _ := v.e.cache.Path(core.CacheRepo{Forge: "github", Repo: "acme/app"})
	if strings.HasPrefix(store, repoDir+"/") {
		t.Fatalf("store %s is in the CI user's repo folder", store)
	}
	// The repo stays protected from eviction while bound.
	k, _ := repocache.Key(core.CacheRepo{Forge: "github", Repo: "acme/app"})
	inUse, err := v.e.cacheInUse()
	must(t, err)
	if !inUse[k] {
		t.Fatal("a bound store is not in use for eviction")
	}
	// Leftover containers are removed after the bind, before the job.
	cmds := v.f.commands()
	at := func(s string) int {
		for i, c := range cmds {
			if strings.Contains(c, s) {
				return i
			}
		}
		t.Fatalf("no %q in:\n%s", s, strings.Join(cmds, "\n"))
		return -1
	}
	if !(at("--unit="+PodnsUnit) < at("podman rm -af") && at("podman rm -af") < at("--unit="+unitName("chk-app"))) {
		t.Fatalf("order of bind, cleanup and job start:\n%s", strings.Join(cmds, "\n"))
	}
}

// While repo app's store is bound and its job runs, another repo's job and
// a cold job neither restart podman nor see the store; another job of app
// shares it without a restart.
func TestContainerStoreStaysWithItsRepo(t *testing.T) {
	v, sh := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	sh.run("chk-app")
	must(t, v.e.Start(ctx, cachedSpec("job-other", "", "acme/other")))
	must(t, v.e.Start(ctx, spec("chk-cold", core.KindCheck)))
	must(t, v.e.Start(ctx, cachedSpec("chk-app2", core.KindCheck, "acme/app")))

	if n := len(podnsStarts(v.f)); n != 1 {
		t.Fatalf("podman restarted %d times, want once (the first bind)", n)
	}
	app := storePath(t, v, "acme/app")
	for _, name := range []string{"job-other", "chk-cold"} {
		c := findUnit(t, v.f, unitName(name))
		if strings.Contains(strings.Join(c.Args, " "), v.e.storeMount()) || strings.Contains(strings.Join(c.Args, " "), app) {
			t.Fatalf("%s names a container store: %q", name, c.Args)
		}
		if _, ok := unitEnv(c)["CONTAINERS_STORAGE_CONF"]; ok {
			t.Fatalf("%s is pointed at the bound store", name)
		}
	}
	if _, err := os.Stat(storePath(t, v, "acme/other")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("another repo's store was made while app's was bound: %v", err)
	}
	c := findUnit(t, v.f, unitName("chk-app2"))
	if !slices.Contains(unitBinds(c), app+":"+v.e.storeMount()) || unitEnv(c)["CONTAINERS_STORAGE_CONF"] == "" {
		t.Fatalf("a second job of the bound repo doesn't share its store: %q", c.Args)
	}
	b, bound, err := v.e.boundStore()
	must(t, err)
	if !bound || b.Repo != "acme/app" {
		t.Fatalf("bound %+v %v, want acme/app", b, bound)
	}
}

// A cached job that finds the device busy (a job running, or another
// Start in flight) doesn't restart podman under it.
func TestContainerStoreNotBoundWhenBusy(t *testing.T) {
	v, sh := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, spec("chk-cold", core.KindCheck)))
	sh.run("chk-cold")
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	if n := len(podnsStarts(v.f)); n != 0 {
		t.Fatalf("podman restarted %d times under a running job", n)
	}
	if _, err := os.Stat(v.e.storageConf()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("storage.conf written without a bind: %v", err)
	}

	v2, _ := storeTestEnv(t)
	v2.e.beginStart("chk-other")
	must(t, v2.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	if n := len(podnsStarts(v2.f)); n != 0 {
		t.Fatalf("podman restarted %d times while another Start was in flight", n)
	}
}

// Reset starts podman with no store, drops the record and storage.conf,
// and wipes only the unbound store: the repo's images stay on disk.
func TestResetUnbindsContainerStore(t *testing.T) {
	v, _ := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	store := storePath(t, v, "acme/app")
	must(t, os.WriteFile(filepath.Join(store, "layer"), []byte("cached"), 0o600))
	before := len(v.f.commands())
	must(t, v.e.Reset(ctx))

	starts := podnsStarts(v.f)
	last := starts[len(starts)-1]
	if strings.Contains(strings.Join(last, " "), "BindPaths=") {
		t.Fatalf("podman restarted with a bind after reset: %q", last)
	}
	drop, err := os.ReadFile(v.e.managerDropIn(testUID))
	must(t, err)
	if strings.Contains(string(drop), "BindPaths") {
		t.Fatalf("user manager drop-in keeps the store:\n%s", drop)
	}
	for _, p := range []string{v.e.storageConf(), v.e.storeStatePath()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survives reset: %v", p, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(store, "layer")); err != nil || string(b) != "cached" {
		t.Fatalf("reset touched the repo's store: %q %v", b, err)
	}
	// rmi -af runs only after podman came back unbound.
	cmds := v.f.commands()[before:]
	podns, rmi := -1, -1
	for i, c := range cmds {
		if strings.Contains(c, "--unit="+PodnsUnit) {
			podns = i
		}
		if strings.Contains(c, "podman rmi -af") && rmi < 0 {
			rmi = i
		}
	}
	if podns < 0 || rmi < 0 || rmi < podns {
		t.Fatalf("reset wiped images before podman was unbound:\n%s", strings.Join(cmds, "\n"))
	}
}

// A bind that podman doesn't take (wrong graphroot) leaves the job without
// the store and podman unbound; when podman can't come back unbound
// either, the job doesn't run.
func TestContainerStoreBindFailsClosed(t *testing.T) {
	v, sh := storeTestEnv(t)
	sh.graphroot = "/var/lib/tentmaker/linux/.local/share/containers/storage"
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	starts := podnsStarts(v.f)
	if len(starts) != 2 || strings.Contains(strings.Join(starts[1], " "), "BindPaths=") {
		t.Fatalf("podns starts %q, want a bind then an unbound start", starts)
	}
	job := findUnit(t, v.f, unitName("chk-app"))
	if strings.Contains(strings.Join(job.Args, " "), v.e.storeMount()) || unitEnv(job)["CONTAINERS_STORAGE_CONF"] != "" {
		t.Fatalf("job uses a store podman didn't take: %q", job.Args)
	}
	for _, p := range []string{v.e.storageConf(), v.e.storeStatePath()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left after a failed bind: %v", p, err)
		}
	}

	v2, sh2 := storeTestEnv(t)
	sh2.graphroot = "elsewhere"
	sh2.podnsFail = 2 // the bind starts, the way back fails
	if err := v2.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")); err == nil {
		t.Fatal("a job started while podman could not leave the store")
	}
	jobStarted := func(name string) bool {
		v2.f.mu.Lock()
		defer v2.f.mu.Unlock()
		for _, c := range v2.f.cmds {
			if c.Path == "systemd-run" && slices.Contains(c.Args, "--unit="+unitName(name)) {
				return true
			}
		}
		return false
	}
	if jobStarted("chk-app") {
		t.Fatal("the job's unit started")
	}
	if _, ok, _ := v2.e.slotOf("chk-app"); ok {
		t.Fatal("the failed job kept its slot")
	}
	if _, bound, _ := v2.e.boundStore(); !bound {
		t.Fatal("the record of a store that may still be bound was dropped")
	}
	// While podman still can't come back unbound, no later job runs: not
	// another job of the same repo (its record is no verified bind), and
	// not a cold one.
	for _, s := range []core.ExecSpec{cachedSpec("chk-app2", core.KindCheck, "acme/app"), spec("chk-cold", core.KindCheck)} {
		if err := v2.e.Start(ctx, s); err == nil {
			t.Fatalf("%s started while podman's store state is unknown", s.Name)
		}
		if jobStarted(s.Name) {
			t.Fatalf("%s's unit started", s.Name)
		}
	}
	// Once podman comes back unbound, jobs run again.
	sh2.mu.Lock()
	sh2.podnsFail = 0
	sh2.mu.Unlock()
	must(t, v2.e.Start(ctx, spec("chk-cold2", core.KindCheck)))
	if _, bound, _ := v2.e.boundStore(); bound {
		t.Fatal("the record survived a successful unbind")
	}
	if c := findUnit(t, v2.f, unitName("chk-cold2")); strings.Contains(strings.Join(c.Args, " "), v2.e.storeMount()) {
		t.Fatalf("a cold job names the store: %q", c.Args)
	}
}

// A verified bind is good only for the boot it was checked in: after a
// reboot podman's namespaces are new, so the next job restarts podman
// before it uses the store again.
func TestContainerStoreRecheckedAfterReboot(t *testing.T) {
	v, _ := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	b, bound, err := v.e.boundStore()
	must(t, err)
	if !bound || !v.e.verified(b) {
		t.Fatalf("record %+v after a good bind, want verified", b)
	}
	must(t, os.WriteFile(filepath.Join(v.o.Paths.Proc, "sys", "kernel", "random", "boot_id"), []byte("another-boot\n"), 0o644))
	if v.e.storeFor("chk-app") != "" {
		t.Fatal("a bind from another boot still names the store")
	}
	must(t, v.e.Start(ctx, cachedSpec("chk-app2", core.KindCheck, "acme/app")))
	starts := podnsStarts(v.f)
	if len(starts) != 3 || strings.Contains(strings.Join(starts[1], " "), "BindPaths=") ||
		!strings.Contains(strings.Join(starts[2], " "), "BindPaths=") {
		t.Fatalf("podns starts %q, want bind, unbind after the reboot, bind again", starts)
	}
	if b, _, _ := v.e.boundStore(); b.Boot != "another-boot" || !b.Verified {
		t.Fatalf("record %+v after the rebind", b)
	}
}

// A cleanup that fails (a volume prune) fails the bind even when no
// container is left: the job runs without the store and podman unbound.
func TestContainerStoreCleanupFailureFailsBind(t *testing.T) {
	v, sh := storeTestEnv(t)
	sh.pruneFail = true
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	starts := podnsStarts(v.f)
	if len(starts) != 2 || strings.Contains(strings.Join(starts[1], " "), "BindPaths=") {
		t.Fatalf("podns starts %q, want a bind then an unbound start", starts)
	}
	job := findUnit(t, v.f, unitName("chk-app"))
	if strings.Contains(strings.Join(job.Args, " "), v.e.storeMount()) || unitEnv(job)["CONTAINERS_STORAGE_CONF"] != "" {
		t.Fatalf("job uses a store whose cleanup failed: %q", job.Args)
	}
	if _, bound, _ := v.e.boundStore(); bound {
		t.Fatal("record left after a failed bind")
	}
	if !strings.Contains(v.logs.String(), "volume in use") {
		t.Fatalf("the log lacks the prune's reason:\n%s", v.logs.String())
	}
}

// podman's namespaces are stopped before a bind or an unbind, so its pause
// pid file is stale. podman 4.9 finds alive missing after a store switch,
// takes its first-boot path, which tries to join the pid in pause.pid
// without the stale check, and fails with "cannot re-exec process to join
// the existing user namespace". So both files go before podman starts.
func TestStoreSwitchDropsStalePauseState(t *testing.T) {
	v, sh := storeTestEnv(t)
	ctx := context.Background()
	tmp := filepath.Join(v.e.runtimeDir(testUID), "libpod", "tmp")
	sh.stale = []string{filepath.Join(tmp, "pause.pid"), filepath.Join(tmp, "alive")}
	plant := func() {
		must(t, os.MkdirAll(tmp, 0o700))
		for _, p := range sh.stale {
			must(t, os.WriteFile(p, []byte("4242"), 0o600))
		}
	}
	plant()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	plant()
	must(t, v.e.Reset(ctx))
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if len(sh.atPodns) != 2 {
		t.Fatalf("podns started %d times, want a bind and an unbind", len(sh.atPodns))
	}
	for i, present := range sh.atPodns {
		if len(present) != 0 {
			t.Fatalf("podns start %d found stale %v", i, present)
		}
	}
}

// A debug command in a kept job sees the job's container store where the
// job did, so its own podman finds what CONTAINERS_STORAGE_CONF names.
func TestDebugUnitSeesTheBoundStore(t *testing.T) {
	v, _ := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	store := storePath(t, v, "acme/app")
	next := v.f.respond
	failed := showResponder("failed", "loaded", false)
	v.f.respond = func(c Cmd) (Result, error) {
		if c.Path == "systemctl" && len(c.Args) > 0 && c.Args[0] == "show" {
			return failed(c)
		}
		return next(c)
	}
	must(t, v.e.Keep(ctx, "chk-app"))
	_, err := v.e.DebugExec(ctx, "chk-app", "podman images", debugSpec())
	must(t, err)
	c := lastSystemdRun(t, v.f)
	if !slices.Contains(unitBinds(c), store+":"+v.e.storeMount()) {
		t.Fatalf("debug unit binds %q, want the store too", unitBinds(c))
	}
	if got := unitEnv(c)["CONTAINERS_STORAGE_CONF"]; got != v.e.storageConf() {
		t.Fatalf("debug CONTAINERS_STORAGE_CONF=%q", got)
	}
}

// A store podman's own CLI sees but the service behind DOCKER_HOST can't
// serve is not bound: podman starts unbound again and the job runs
// without it, and the log says why.
func TestContainerStoreChecksTheService(t *testing.T) {
	v, sh := storeTestEnv(t)
	sh.remoteFail = true
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	starts := podnsStarts(v.f)
	if len(starts) != 2 || !strings.Contains(strings.Join(starts[0], " "), "BindPaths=") ||
		strings.Contains(strings.Join(starts[1], " "), "BindPaths=") {
		t.Fatalf("podns starts %q, want a bind then an unbound start", starts)
	}
	remote := false
	for _, c := range v.f.commands() {
		if strings.Contains(c, "podman --remote --url unix://"+filepath.Join(v.e.runtimeDir(testUID), "podman", "podman.sock")+" info") {
			remote = true
		}
	}
	if !remote {
		t.Fatal("the service behind DOCKER_HOST was not asked")
	}
	if _, err := os.Stat(v.e.storageConf()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("storage.conf survived the failed bind")
	}
	if !strings.Contains(v.logs.String(), "Cannot connect to Podman") {
		t.Fatalf("the log lacks podman's reason:\n%s", v.logs.String())
	}
}

// Reset says whether it failed before or after podman came back unbound:
// only after it is the failure a core.CacheFreedError, which lets the
// worker end its cache hold.
func TestResetErrorSaysWhetherTheStoreIsFree(t *testing.T) {
	v, _ := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	next := v.f.respond
	v.f.respond = func(c Cmd) (Result, error) {
		if strings.Contains(strings.Join(c.Args, " "), "podman images -q") {
			return Result{Stdout: "abc123\n"}, nil
		}
		return next(c)
	}
	err := v.e.Reset(ctx)
	var cf *core.CacheFreedError
	if err == nil || !errors.As(err, &cf) || !strings.Contains(err.Error(), "images left") {
		t.Fatalf("reset after the unbind: %v, want a CacheFreedError", err)
	}

	v2, sh2 := storeTestEnv(t)
	must(t, v2.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	sh2.mu.Lock()
	sh2.podnsFail = sh2.podns + 1
	sh2.mu.Unlock()
	err = v2.e.Reset(ctx)
	if err == nil || errors.As(err, &cf) {
		t.Fatalf("reset whose unbind failed: %v, want a plain error", err)
	}
	// The unbind failed partway, so the bind is no longer verified: while
	// podman still can't come back unbound, neither a job of the same repo
	// nor a cold one starts.
	for _, s := range []core.ExecSpec{cachedSpec("chk-app2", core.KindCheck, "acme/app"), spec("chk-cold", core.KindCheck)} {
		if err := v2.e.Start(ctx, s); err == nil {
			t.Fatalf("%s started after a reset whose unbind failed", s.Name)
		}
	}
}

// writeImages makes podman's image list in store name an image.
func writeImages(t *testing.T, store string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(store, "overlay-images"), 0o700))
	must(t, os.WriteFile(filepath.Join(store, "overlay-images", "images.json"),
		[]byte(`[{"id":"`+strings.Repeat("a", 64)+`","names":["localhost/app:latest"]}]`), 0o600))
}

func usageOf(t *testing.T, v *env, name string) core.Usage {
	t.Helper()
	u, _ := v.e.JobUsage(context.Background(), name)
	return u
}

// Each job's usage says whether its repo cache and container store were
// warm when it claimed its cache (issue #45), and a later reading never
// changes that (review first-F2): the first job of a repo misses its repo
// cache and has no store outcome, even after it builds an image; the next
// job of the repo hits both. A job that ran without the store while it
// held images is a miss; one that ran without it while it was empty stays
// no outcome when images come later; a cold job has neither.
func TestCacheOutcomes(t *testing.T) {
	v, sh := storeTestEnv(t)
	ctx := context.Background()
	must(t, v.e.Start(ctx, cachedSpec("chk-app", core.KindCheck, "acme/app")))
	if u := usageOf(t, v, "chk-app"); u.RepoCache != core.CacheMiss || u.ContainerStore != "" {
		t.Fatalf("first job: repo %q store %q, want miss and none", u.RepoCache, u.ContainerStore)
	}
	// The job fills its cache and builds an image.
	repoDir, _ := v.e.cache.Path(core.CacheRepo{Forge: "github", Repo: "acme/app"})
	must(t, os.WriteFile(filepath.Join(repoDir, "go-build"), []byte("x"), 0o600))
	writeImages(t, storePath(t, v, "acme/app"))
	if u := usageOf(t, v, "chk-app"); u.RepoCache != core.CacheMiss || u.ContainerStore != "" {
		t.Fatalf("first job after its build: repo %q store %q, want miss and none", u.RepoCache, u.ContainerStore)
	}
	sh.run("chk-app")
	must(t, v.e.Start(ctx, cachedSpec("chk-app2", core.KindCheck, "acme/app")))
	if u := usageOf(t, v, "chk-app2"); u.RepoCache != core.CacheHit || u.ContainerStore != core.CacheHit {
		t.Fatalf("second job: repo %q store %q, want hit and hit", u.RepoCache, u.ContainerStore)
	}
	// app's store is bound, so another repo's job runs without its own.
	// While that store is empty the job has no outcome, and images a later
	// job puts there don't make it a miss.
	must(t, v.e.Start(ctx, cachedSpec("job-other0", "", "acme/other")))
	writeImages(t, storePath(t, v, "acme/other"))
	if u := usageOf(t, v, "job-other0"); u.ContainerStore != "" {
		t.Fatalf("unbound job of a store empty at its start: store %q, want none", u.ContainerStore)
	}
	// Now the store holds images and app's is still bound: the next job of
	// the repo runs without them.
	must(t, v.e.Start(ctx, cachedSpec("job-other", "", "acme/other")))
	if u := usageOf(t, v, "job-other"); u.ContainerStore != core.CacheMiss {
		t.Fatalf("unbound job of a warm store: store %q, want miss", u.ContainerStore)
	}
	must(t, v.e.Start(ctx, spec("chk-cold", core.KindCheck)))
	if u := usageOf(t, v, "chk-cold"); u.RepoCache != "" || u.ContainerStore != "" {
		t.Fatalf("cold job: repo %q store %q, want none", u.RepoCache, u.ContainerStore)
	}
}

// The store is the job user's, so a link in it can't make the daemon read
// a file outside it: the image list is read through an os.Root.
func TestContainersWarmIgnoresLinksOut(t *testing.T) {
	v, _ := storeTestEnv(t)
	c := core.CacheRepo{Forge: "github", Repo: "acme/app"}
	store, err := v.e.cache.PrepareContainers(c, os.Getuid(), os.Getgid())
	must(t, err)
	outside := t.TempDir()
	writeImages(t, outside)
	must(t, os.Symlink(filepath.Join(outside, "overlay-images"), filepath.Join(store, "overlay-images")))
	if v.e.cache.ContainersWarm(c) {
		t.Fatal("a link out of the store read as a warm store")
	}
	must(t, os.Remove(filepath.Join(store, "overlay-images")))
	writeImages(t, store)
	if !v.e.cache.ContainersWarm(c) {
		t.Fatal("a store with an image reads as cold")
	}
}
