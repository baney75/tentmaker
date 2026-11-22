package actionscache

import (
	"cmp"
	"os"
	"slices"
	"time"

	"github.com/baney75/tentmaker/internal/repocache"
)

// MaxRepo is GitHub's cap on one repo's Actions cache, and the most a
// repo's entries may hold here.
const MaxRepo = 10 << 30

// entryTTL is how long an entry no restore matched is kept, as on GitHub.
const entryTTL = 7 * 24 * time.Hour

// lookupGrace keeps an entry a restore just matched from eviction until its
// download has had time to begin.
const lookupGrace = time.Minute

// Budgeter is a Store that says how many bytes a repo's entries may hold
// (repocache.Store.ActionsBudget: half of cache_max). The entries live in
// the repo's cache folder, so a budget below the repo cache's cap keeps
// them from pushing the whole folder, build caches and all, out.
type Budgeter interface {
	ActionsBudget() int64
}

// budgetOf is st's budget, at most MaxRepo; a store without one gets half
// of the default repo cache cap.
func budgetOf(st Store) int64 {
	n := int64(repocache.DefaultMax / 2)
	if b, ok := st.(Budgeter); ok {
		n = b.ActionsBudget()
	}
	return max(min(n, MaxRepo), 0)
}

// touch records that a restore matched entry id of repo rk. It is kept in
// memory and written to the index at the repo's next create or commit, so
// a restore adds no write.
func (s *Service) touch(rk string, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.used[rk]
	if m == nil {
		m = map[int64]time.Time{}
		s.used[rk] = m
	}
	m[id] = s.o.Now().UTC()
}

// reading counts a download of rk's file in flight, until done is called.
func (s *Service) reading(rk, file string) (done func()) {
	k := rk + "\x00" + file
	s.mu.Lock()
	s.reads[k]++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.reads[k]--; s.reads[k] <= 0 {
			delete(s.reads, k)
		}
		s.mu.Unlock()
	}
}

// evictLocked writes rk's recorded restores into idx, then drops from it
// the entries no restore matched for entryTTL and, oldest use first, as
// many more as it takes for idx to fit budget. An entry with a download in
// flight, one a restore matched within lookupGrace, and entry keep are
// never dropped. It returns the dropped entries' files, which the caller
// deletes once idx is saved, and whether idx changed. The caller holds the
// repo lock.
func (s *Service) evictLocked(rk string, idx *index, budget, keep int64) (files []string, changed bool) {
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	used := s.used[rk]
	delete(s.used, rk)
	order := make([]*entry, 0, len(idx.Entries))
	var total int64
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if t, ok := used[e.ID]; ok && t.After(e.Used) {
			e.Used, changed = t, true
		}
		order = append(order, e)
		total += e.Size
	}
	slices.SortFunc(order, func(a, b *entry) int {
		if c := a.lastUse().Compare(b.lastUse()); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	drop := map[int64]bool{}
	for _, e := range order {
		busy := e.ID == keep || s.reads[rk+"\x00"+e.File] > 0 || now.Sub(e.Used) < lookupGrace
		if busy || (total <= budget && now.Sub(e.lastUse()) < entryTTL) {
			continue
		}
		drop[e.ID], total = true, total-e.Size
	}
	if len(drop) == 0 {
		return nil, changed
	}
	idx.Entries = slices.DeleteFunc(idx.Entries, func(e entry) bool {
		if drop[e.ID] {
			files = append(files, e.File)
		}
		return drop[e.ID]
	})
	return files, true
}

func removeFiles(root *os.Root, files []string) {
	for _, f := range files {
		_ = root.Remove(f)
	}
}
