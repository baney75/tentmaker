package linux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/repocache"
)

// A job with a repo cache gets two things a cold job doesn't:
//
//   - The repo's cache folder, bind-mounted read-write at cacheMount() in the
//     job's own mount namespace only. On the host it sits under a root-only
//     0700 folder (repocache), so no job reaches another repo's cache by
//     path; the bind is the only way in. GOCACHE, GOMODCACHE, the tool cache
//     and friends point into it.
//   - A job folder at a stable path: jobs/<repo key>-<n>, the lowest n no job
//     of that repo holds, instead of jobs/<runner name>. Go keys a package's
//     build cache entries by its source folder, and test results by the
//     environment the tests read (TMPDIR, HOME), so a folder that changes
//     with every job rebuilt the repo's own packages on every warm run
//     (measured: go test -race of this repo, 0.3 s at the same path, 48 s at
//     a new one). The folder is a real host path, so podman sees it too.
//
// The runner name -> slot record lives in <priv>/slots (root only), written
// before the slot folder exists and removed after it is gone.

var slotRe = regexp.MustCompile(`^[0-9a-f]{12}-[0-9]{1,3}$`)

const maxSlotIndex = 999

type slotRec struct {
	Slot  string `json:"slot"`
	Forge string `json:"forge"`
	Repo  string `json:"repo"`
	Key   string `json:"key"`
	// Warm: the repo's cache held something when the job claimed it.
	Warm bool `json:"warm,omitempty"`
	// StoreWarm: the repo's container store was bound for the job and
	// held images when the job claimed it.
	StoreWarm bool `json:"store_warm,omitempty"`
	// StoreMiss: the repo's container store held images when the job
	// claimed its cache, but the job started without it (other jobs used
	// podman, or binding it failed).
	StoreMiss bool `json:"store_miss,omitempty"`
}

// markSlot changes job name's slot record with f. A job with no record is
// left alone. It only feeds metrics, so a failure is logged, never fatal.
func (e *Executor) markSlot(name string, f func(*slotRec)) {
	e.slotMu.Lock()
	defer e.slotMu.Unlock()
	r, ok, err := e.slotOf(name)
	if err != nil || !ok {
		return
	}
	f(&r)
	data, _ := json.Marshal(r)
	if err := config.WriteAtomic(e.slotPath(name), data, 0o600); err != nil {
		e.log.Warn("slot record not updated", "name", name, "error", err)
	}
}

// cacheOutcomes says whether job r started with a warm repo cache and a
// warm container store (core.Usage.RepoCache and ContainerStore). Both
// come from the slot record alone, written when the job claimed its
// cache, so a reading never depends on when it is taken.
func cacheOutcomes(r slotRec) (repo, store string) {
	repo = core.CacheMiss
	if r.Warm {
		repo = core.CacheHit
	}
	switch {
	case r.StoreWarm:
		store = core.CacheHit
	case r.StoreMiss:
		store = core.CacheMiss
	}
	return repo, store
}

func (e *Executor) slotsDir() string    { return filepath.Join(e.priv, "slots") }
func (e *Executor) affinityDir() string { return filepath.Join(e.priv, "slot-affinity") }
func (e *Executor) affinityPath(key string) string {
	return filepath.Join(e.affinityDir(), key[:12]+".json")
}
func (e *Executor) slotPath(name string) string { return filepath.Join(e.slotsDir(), name+".json") }

// cacheMount is where a job sees its repo's cache. On the host it is an
// empty root-owned folder; each job's unit binds its own repo's cache there.
func (e *Executor) cacheMount() string { return filepath.Join(e.root, "repo-cache") }

// slotOf reads a job's slot record. ok is false when the job has none (a
// job with no cache, or one started before slots existed).
func (e *Executor) slotOf(name string) (slotRec, bool, error) {
	data, err := os.ReadFile(e.slotPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return slotRec{}, false, nil
	}
	if err != nil {
		return slotRec{}, false, err
	}
	var r slotRec
	if err := json.Unmarshal(data, &r); err != nil {
		return slotRec{}, false, fmt.Errorf("slot record of %s: %v", name, err)
	}
	if !slotRe.MatchString(r.Slot) {
		return slotRec{}, false, fmt.Errorf("slot record of %s names a bad slot %q", name, trunc(r.Slot, 40))
	}
	if k, err := repocache.Key(core.CacheRepo{Forge: r.Forge, Repo: r.Repo}); err != nil || k != r.Key {
		return slotRec{}, false, fmt.Errorf("slot record of %s doesn't match its repo", name)
	}
	return r, true, nil
}

// slots lists every slot record by runner name.
func (e *Executor) slots() (map[string]slotRec, error) {
	ents, err := os.ReadDir(e.slotsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]slotRec{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]slotRec{}
	for _, ent := range ents {
		name, ok := strings.CutSuffix(ent.Name(), ".json")
		if !ok || !core.NamePattern.MatchString(name) {
			continue
		}
		r, ok, err := e.slotOf(name)
		if err != nil {
			return nil, err
		}
		if ok {
			out[name] = r
		}
	}
	return out, nil
}

// claimSlot records a slot for job name and returns it: the slot c.Job's
// last run used when it is free (jobs of one repo start at the same time,
// and Go's cache only hits at the same folder), else the lowest free one.
// The record exists before the folder, so the folder is never one List
// can't account for.
func (e *Executor) claimSlot(name string, c core.CacheRepo) (slotRec, error) {
	key, err := repocache.Key(c)
	if err != nil {
		return slotRec{}, err
	}
	e.slotMu.Lock()
	defer e.slotMu.Unlock()
	if err := os.MkdirAll(e.slotsDir(), 0o700); err != nil {
		return slotRec{}, err
	}
	held, err := e.slots()
	if err != nil {
		return slotRec{}, err
	}
	taken := map[string]bool{}
	for _, r := range held {
		taken[r.Slot] = true
	}
	free := func(s string) bool {
		if taken[s] {
			return false
		}
		_, err := os.Lstat(filepath.Join(e.jobsDir(), s))
		return err != nil // a leftover folder no record names; Reset or Clean removes it
	}
	aff := e.affinity(key)
	pick := ""
	if s, ok := aff[c.Job]; ok && c.Job != "" && strings.HasPrefix(s, key[:12]+"-") && slotRe.MatchString(s) && free(s) {
		pick = s
	}
	// The lowest free slot no other job of the repo last used, then any.
	last := map[string]bool{}
	for _, s := range aff {
		last[s] = true
	}
	for pass := 0; pick == "" && pass < 2; pass++ {
		for n := 0; n <= maxSlotIndex; n++ {
			s := key[:12] + "-" + strconv.Itoa(n)
			if free(s) && (pass == 1 || !last[s]) {
				pick = s
				break
			}
		}
	}
	if pick == "" {
		return slotRec{}, fmt.Errorf("no free job folder for %s", c.Repo)
	}
	r := slotRec{Slot: pick, Forge: c.Forge, Repo: c.Repo, Key: key}
	data, _ := json.Marshal(r)
	if err := config.WriteAtomic(e.slotPath(name), data, 0o600); err != nil {
		return slotRec{}, err
	}
	if c.Job != "" && aff[c.Job] != pick {
		aff[c.Job] = pick
		e.saveAffinity(key, aff)
	}
	return r, nil
}

// affinity maps a repo's job names to the slot each last used. It is only
// a preference, so a missing or bad file reads as empty.
func (e *Executor) affinity(key string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(e.affinityPath(key))
	if err != nil || json.Unmarshal(data, &out) != nil {
		return map[string]string{}
	}
	return out
}

// maxAffinity bounds the job names kept per repo.
const maxAffinity = 64

func (e *Executor) saveAffinity(key string, aff map[string]string) {
	if len(aff) > maxAffinity {
		return // a repo with this many job names gets no new preferences
	}
	data, err := json.Marshal(aff)
	if err != nil {
		return
	}
	if err := os.MkdirAll(e.affinityDir(), 0o700); err != nil {
		return
	}
	if err := config.WriteAtomic(e.affinityPath(key), data, 0o600); err != nil {
		e.log.Warn("slot preference not saved", "error", err)
	}
}

func (e *Executor) dropSlot(name string) { os.Remove(e.slotPath(name)) }

// cacheInUse names the repos with a job on the host: every slot record,
// running or not yet cleaned. repocache calls it before it evicts.
func (e *Executor) cacheInUse() (map[string]bool, error) {
	held, err := e.slots()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range held {
		out[r.Key] = true
	}
	// A bound container store is mounted in podman's namespaces.
	b, bound, err := e.boundStore()
	if err != nil {
		return nil, err
	}
	if bound {
		out[b.Key] = true
	}
	return out, nil
}

// CachesRepos implements core.RepoCacher.
func (e *Executor) CachesRepos() bool { return e.cache.Enabled() }

// cacheEnv points the tools a job runs at its repo cache. The runner's tool
// cache stays in the slot folder (jobEnv): the runner binds it into job
// containers, and podman runs in a mount namespace that can't see the
// cache mount. The slot is stable per repo, so it stays warm anyway.
func (e *Executor) cacheEnv() []string {
	m := e.cacheMount()
	return []string{
		"GOCACHE=" + filepath.Join(m, "go-build"),
		"GOMODCACHE=" + filepath.Join(m, "go-mod"),
		"npm_config_cache=" + filepath.Join(m, "npm"),
		"PIP_CACHE_DIR=" + filepath.Join(m, "pip"),
		"CARGO_HOME=" + filepath.Join(m, "cargo"),
		"XDG_CACHE_HOME=" + filepath.Join(m, "xdg"),
	}
}
