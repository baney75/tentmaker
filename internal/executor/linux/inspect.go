package linux

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/executor/joblog"
)

var _ core.JobInspector = (*Executor)(nil)

// JobLog implements core.JobInspector: the runner's diag logs, or a check
// job's log.txt, from the job's folder.
func (e *Executor) JobLog(ctx context.Context, name string, max int64) (core.JobLog, error) {
	if err := checkName(name); err != nil {
		return core.JobLog{}, err
	}
	return joblog.Read(e.jobDir(name), max)
}

// JobUsage implements core.JobInspector from the job unit's cgroup:
// memory.peak (the most memory it held) and cpu.stat's usage_usec. The
// cgroup goes away when the unit stops, so the worker samples it while the
// job runs and keeps the last reading. The cache size is what the repo
// cache index last measured for the job's repo; the cache outcomes come
// from the slot record, written when the job claimed its cache.
func (e *Executor) JobUsage(ctx context.Context, name string) (core.Usage, error) {
	if err := checkName(name); err != nil {
		return core.Usage{}, err
	}
	var u core.Usage
	if r, ok, err := e.slotOf(name); err == nil && ok {
		u.CacheBytes = e.cache.Size(core.CacheRepo{Forge: r.Forge, Repo: r.Repo})
		u.RepoCache, u.ContainerStore = e.cacheOutcomes(r)
	}
	cg := filepath.Join(filepath.Dir(e.p.CgroupControllers), Slice, unitName(name))
	peak, perr := readInt(filepath.Join(cg, "memory.peak"))
	if perr == nil {
		u.PeakMemoryBytes = peak
	}
	usec, cerr := readStat(filepath.Join(cg, "cpu.stat"), "usage_usec")
	if cerr == nil {
		u.CPUSeconds = float64(usec) / 1e6
	}
	if perr != nil && cerr != nil {
		return u, perr
	}
	return u, nil
}

// readInt reads a cgroup file holding one number. Cgroup files are the
// kernel's, not the job's, so no Root is needed; the size is capped anyway.
func readInt(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func readStat(path, key string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 4096))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New(key + " not in " + path)
}
