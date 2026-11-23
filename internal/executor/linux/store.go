package linux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/repocache"
)

// A repo's container store keeps podman's images, layers and build cache
// between that repo's jobs, so a docker build or a container step starts
// warm: base images are already pulled and unchanged layers are reused.
//
// Rootless podman is one per device: every podman command, the service
// behind DOCKER_HOST included, runs in the pause process's mount namespace
// (podns.go). So the store is bound to the device, not to a job. The bind:
//
//   - The store lives at <cache_dir>/repos/.containers/<key>/storage
//     (repocache.PrepareContainers). Every folder above it is root's with
//     mode 0700, so a job can't reach another repo's store by path and can't
//     swap a folder on the way for a symlink.
//   - With no job on the device, the first cached job of repo R restarts
//     podman's namespaces (startPodman) with BindPaths=<store>:storeMount()
//     in PodnsUnit and the user manager, and the runner's storage.conf
//     naming storeMount() as podman's graphroot. Only those mount
//     namespaces see R's store; on the host storeMount() is an empty
//     root-owned folder.
//   - Later jobs of R use it while it is bound. A job of any other repo
//     never binds it, and a cold job (no repo cache) doesn't name it.
//   - Reset, which runs when nothing does, starts podman unbound again.
//
// While bound, any process the runner's uid runs can reach the store
// through /proc/<pause pid>/root, so the worker holds the device for R
// from the first cached job until the next reset (dispatch, Held).
// Binding when other jobs run would restart podman under them, so a cached
// job that finds the device busy and the store unbound runs without it.

// storeMount is where podman's graphroot is while a store is bound. On the
// host it is an empty root-owned folder.
func (e *Executor) storeMount() string { return filepath.Join(e.root, "container-store") }

// storageConf is the runner's storage.conf: present only while a store is
// bound. Its folder is root's, so no job can change it.
func (e *Executor) storageConf() string {
	return filepath.Join(e.root, ".config", "containers", "storage.conf")
}

// storeRunRoot is podman's runroot while a store is bound, apart from the
// default one, whose state belongs to the unbound store.
func (e *Executor) storeRunRoot(uid int) string {
	return filepath.Join(e.runtimeDir(uid), "containers-store")
}

func (e *Executor) storeStatePath() string { return filepath.Join(e.priv, "container-store.json") }

func storageConfText(graphroot, runroot string) string {
	return "# Written by tentmaker. A repo's container store is bound; see\n" +
		"# internal/executor/linux/store.go.\n" +
		"[storage]\n" +
		"graphroot = " + strconvQuote(graphroot) + "\n" +
		"runroot = " + strconvQuote(runroot) + "\n"
}

// strconvQuote quotes a path for TOML. Paths here never hold a quote or a
// backslash (they are built from fixed names and a hex key).
func strconvQuote(s string) string { return `"` + s + `"` }

type storeRec struct {
	Forge string `json:"forge"`
	Repo  string `json:"repo"`
	Key   string `json:"key"`
	// Verified is set only once checkStore passed for this bind, and Boot
	// is the boot it passed in. Any other record is a bind in progress, a
	// bind whose way back failed, or one a reboot undid: podman's state is
	// unknown, and no job starts until podman is back unbound.
	Verified bool   `json:"verified,omitempty"`
	Boot     string `json:"boot,omitempty"`
}

// bootID names this boot, or "" when it can't be read.
func (e *Executor) bootID() string {
	b, err := os.ReadFile(filepath.Join(e.p.Proc, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// verified says whether r is a bind checkStore passed in this boot.
func (e *Executor) verified(r storeRec) bool {
	boot := e.bootID()
	return r.Verified && boot != "" && r.Boot == boot
}

func (e *Executor) writeStoreRec(r storeRec) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return config.WriteAtomic(e.storeStatePath(), data, 0o600)
}

// boundStore reads which repo's store podman may have bound. The record is
// written before a bind and removed only after podman started unbound, so
// it never says "none" while a store is mounted.
func (e *Executor) boundStore() (storeRec, bool, error) {
	data, err := os.ReadFile(e.storeStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return storeRec{}, false, nil
	}
	if err != nil {
		return storeRec{}, false, err
	}
	var r storeRec
	if err := json.Unmarshal(data, &r); err != nil {
		return storeRec{}, false, fmt.Errorf("container store record: %v", err)
	}
	if k, err := repocache.Key(core.CacheRepo{Forge: r.Forge, Repo: r.Repo}); err != nil || k != r.Key {
		return storeRec{}, false, errors.New("container store record doesn't match its repo")
	}
	return r, true, nil
}

// storeFor is the host path of the store job name uses, or "": the job has
// a repo cache and that repo's store is bound.
func (e *Executor) storeFor(name string) string {
	r, ok, err := e.slotOf(name)
	if err != nil || !ok {
		return ""
	}
	b, bound, err := e.boundStore()
	if err != nil || !bound || b.Key != r.Key || !e.verified(b) {
		return ""
	}
	p, err := e.cache.ContainersPath(core.CacheRepo{Forge: r.Forge, Repo: r.Repo})
	if err != nil {
		return ""
	}
	return p
}

// storeEnv points a job's own podman at the bound store, so `podman build`
// and `docker build` share it. The job's XDG_CONFIG_HOME is its own and
// would not find the runner's storage.conf.
func (e *Executor) storeEnv() []string {
	return []string{"CONTAINERS_STORAGE_CONF=" + e.storageConf()}
}

// beginStart registers a job whose Start is in flight. It waits for a
// bind in progress, so no job starts while podman restarts.
func (e *Executor) beginStart(name string) {
	e.podMu.Lock()
	defer e.podMu.Unlock()
	if e.starting == nil {
		e.starting = map[string]bool{}
	}
	e.starting[name] = true
}

func (e *Executor) endStart(name string) {
	e.podMu.Lock()
	defer e.podMu.Unlock()
	delete(e.starting, name)
}

// idleLocked says whether no job but name is starting or running, so
// podman can restart under nobody. Errors read as busy.
func (e *Executor) idleLocked(ctx context.Context, name string) bool {
	for n := range e.starting {
		if n != name {
			return false
		}
	}
	units, err := e.jobUnits(ctx)
	if err != nil {
		return false
	}
	for n, st := range units {
		if n != name && st != "inactive" && st != "failed" {
			return false
		}
	}
	return true
}

// claimStore gives a job with a repo cache its repo's container store:
// already bound, or bound now when the device is idle. A failed bind
// leaves the job without it (it is only faster). An error means podman's
// state is unknown (a record that isn't a verified bind, and podman could
// not be brought back unbound), and the job must not run: that holds for
// every job, cold ones too, until an unbind or a reset succeeds.
func (e *Executor) claimStore(ctx context.Context, spec core.ExecSpec, uid, gid int) error {
	e.podMu.Lock()
	defer e.podMu.Unlock()
	b, bound, err := e.boundStore()
	if err != nil || (bound && !e.verified(b)) {
		why := "the container store bind was never verified"
		if err != nil {
			why = err.Error()
		}
		if !e.idleLocked(ctx, spec.Name) {
			return fmt.Errorf("podman's container store is in an unknown state (%s) and other jobs run; waiting for a reset", why)
		}
		e.log.Warn("podman restarts unbound: its container store is in an unknown state", "name", spec.Name, "error", why)
		if err := e.unbindLocked(ctx, uid, gid); err != nil {
			return err
		}
		bound = false
	}
	r, ok, err := e.slotOf(spec.Name)
	if err != nil || !ok {
		return nil
	}
	c := core.CacheRepo{Forge: r.Forge, Repo: r.Repo}
	// The job's container store outcome is a fact of this moment: set
	// once the job's start with or without its store is decided.
	withStore := false
	defer func() { e.markStore(spec.Name, c, withStore) }()
	if bound && b.Key == r.Key {
		withStore = true
		return nil
	}
	if !e.idleLocked(ctx, spec.Name) {
		e.log.Info("job runs without its container store: other jobs use podman", "name", spec.Name, "repo", r.Repo)
		return nil
	}
	src, err := e.cache.PrepareContainers(c, uid, gid)
	if err != nil {
		e.log.Warn("job runs without its container store", "name", spec.Name, "repo", r.Repo, "error", err)
		return nil
	}
	t0 := time.Now()
	rec := storeRec{Forge: r.Forge, Repo: r.Repo, Key: r.Key}
	if err := e.writeStoreRec(rec); err != nil {
		e.log.Warn("job runs without its container store", "name", spec.Name, "error", err)
		return nil
	}
	why := ""
	if err := e.startPodman(ctx, uid, gid, src); err != nil {
		why = err.Error()
	} else if why = e.checkStore(ctx, uid); why == "" {
		rec.Verified, rec.Boot = true, e.bootID()
		if rec.Boot == "" {
			why = "can't read the boot id"
		} else if err := e.writeStoreRec(rec); err != nil {
			why = err.Error()
		}
	}
	if why == "" {
		e.log.Info("container store bound", "name", spec.Name, "repo", r.Repo, "took", time.Since(t0).Round(time.Millisecond).String())
		withStore = true
		return nil
	}
	e.log.Warn("job runs without its container store", "name", spec.Name, "repo", r.Repo, "error", why)
	return e.unbindLocked(ctx, uid, gid)
}

// checkStore makes sure podman uses the bound store with a job's view, and
// that no container, pod or volume a past job left in it survives. It says
// what is wrong, or "".
func (e *Executor) checkStore(ctx context.Context, uid int) string {
	if p := e.podnsProblem(ctx, uid); p != "" {
		return p
	}
	// Both ways a job reaches podman: its own CLI, and the service behind
	// DOCKER_HOST, which the socket starts on demand and which must find
	// the same store.
	sock := "unix://" + filepath.Join(e.runtimeDir(uid), "podman", "podman.sock")
	for _, via := range [][]string{nil, {"--remote", "--url", sock}} {
		args := append(append([]string{"podman"}, via...), "info", "--format", "{{.Store.GraphRoot}}")
		res, err := e.h.Run(ctx, e.asRunner(uid, time.Minute, args...))
		what := "podman"
		if via != nil {
			what = "podman's service"
		}
		if err != nil {
			return fmt.Sprintf("%s info: %v: %s", what, err, trunc(strings.TrimSpace(res.Stderr), 300))
		}
		if got := strings.TrimSpace(res.Stdout); got != e.storeMount() {
			return fmt.Sprintf("%s's graphroot is %q, not %s", what, trunc(got, 120), e.storeMount())
		}
	}
	for _, args := range [][]string{
		{"podman", "pod", "rm", "-af"},
		{"podman", "rm", "-af"},
		{"podman", "volume", "prune", "-f"},
		{"podman", "network", "prune", "-f"},
	} {
		// A volume or network prune that fails leaves what a past job made
		// for this one to find, and nothing lists it afterwards.
		if res, err := e.h.Run(ctx, e.asRunner(uid, 10*time.Minute, args...)); err != nil {
			return fmt.Sprintf("%s: %v: %s", strings.Join(args, " "), err, trunc(strings.TrimSpace(res.Stderr), 300))
		}
	}
	if res, err := e.h.Run(ctx, e.asRunner(uid, time.Minute, "podman", "ps", "-aq")); err != nil || strings.TrimSpace(res.Stdout) != "" {
		return "containers left in the store"
	}
	return ""
}

// unbindLocked starts podman with no store bound and drops the record.
// It marks a verified record unverified first: once the unbind begins,
// podman's state no longer matches it, and an unbind that fails partway
// must leave every later Start refusing until one succeeds.
func (e *Executor) unbindLocked(ctx context.Context, uid, gid int) error {
	if b, bound, err := e.boundStore(); err == nil && bound && b.Verified {
		b.Verified, b.Boot = false, ""
		if err := e.writeStoreRec(b); err != nil {
			return fmt.Errorf("container store record: %v", err)
		}
	}
	if err := e.startPodman(ctx, uid, gid, ""); err != nil {
		return fmt.Errorf("podman could not start again without the container store: %v", err)
	}
	if err := os.Remove(e.storeStatePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// setStore writes the runner's storage.conf for store (the host path of a
// repo's store) or removes it for none. podman's namespaces must be
// stopped. Either way it clears what a reboot would: podman's alive file,
// so podman refreshes the state of containers on record (their processes
// are gone), and its pause pid file, whose process is gone too. Both go
// together: podman 4.9 with no alive file joins the pid in pause.pid
// without checking it, and fails ("cannot re-exec process to join the
// existing user namespace") on a stale one. A bind also clears its own
// runroot.
func (e *Executor) setStore(uid int, store string) error {
	tmp := filepath.Join(e.runtimeDir(uid), "libpod", "tmp")
	for _, p := range []string{e.pausePIDFile(uid), filepath.Join(tmp, "alive")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if store == "" {
		if err := os.Remove(e.storageConf()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(e.storeMount(), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(e.storeRunRoot(uid)); err != nil {
		return err
	}
	return writeRoot(e.h, e.storageConf(), []byte(storageConfText(e.storeMount(), e.storeRunRoot(uid))), 0o644)
}

// storeBind is the mount property that shows store at storeMount(), or
// none.
func (e *Executor) storeBind(store string) []string {
	if store == "" {
		return nil
	}
	return []string{"BindPaths=" + store + ":" + e.storeMount()}
}

// markStore records job name's container store outcome as it claims its
// cache, when its repo's store holds images: a hit when the job starts
// with the store bound, a miss when it starts without it. A store with no
// image yet is no outcome.
func (e *Executor) markStore(name string, c core.CacheRepo, bound bool) {
	if !e.cache.ContainersWarm(c) {
		return
	}
	e.markSlot(name, func(r *slotRec) {
		if bound {
			r.StoreWarm = true
		} else {
			r.StoreMiss = true
		}
	})
}
