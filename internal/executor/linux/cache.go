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
}

func (e *Executor) slotsDir() string            { return filepath.Join(e.priv, "slots") }
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

// claimSlot records a slot for job name and returns it. The record exists
// before the folder, so the folder is never one List can't account for.
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
	for n := 0; n <= maxSlotIndex; n++ {
		s := key[:12] + "-" + strconv.Itoa(n)
		if taken[s] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(e.jobsDir(), s)); err == nil {
			continue // a leftover folder no record names; Reset or Clean removes it
		}
		r := slotRec{Slot: s, Forge: c.Forge, Repo: c.Repo, Key: key}
		data, _ := json.Marshal(r)
		if err := config.WriteAtomic(e.slotPath(name), data, 0o600); err != nil {
			return slotRec{}, err
		}
		return r, nil
	}
	return slotRec{}, fmt.Errorf("no free job folder for %s", c.Repo)
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
	return out, nil
}

// cacheEnv points the tools a job runs at its repo cache.
func (e *Executor) cacheEnv() []string {
	m := e.cacheMount()
	return []string{
		"GOCACHE=" + filepath.Join(m, "go-build"),
		"GOMODCACHE=" + filepath.Join(m, "go-mod"),
		"RUNNER_TOOL_CACHE=" + filepath.Join(m, "toolcache"),
		"AGENT_TOOLSDIRECTORY=" + filepath.Join(m, "toolcache"),
		"npm_config_cache=" + filepath.Join(m, "npm"),
		"PIP_CACHE_DIR=" + filepath.Join(m, "pip"),
		"CARGO_HOME=" + filepath.Join(m, "cargo"),
		"XDG_CACHE_HOME=" + filepath.Join(m, "xdg"),
	}
}
