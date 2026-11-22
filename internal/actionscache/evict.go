package actionscache

import (
	"cmp"
	"os"
	"slices"
	"time"
)

// lookupGrace keeps an entry a restore just matched from eviction until
// its download has had time to begin.
const lookupGrace = time.Minute

// touch records that a restore matched entry id of repo rk. It stays in
// memory and reaches the index at the repo's next commit, so a restore
// adds no write. The caller holds the repo lock.
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

// reading counts a download of rk's file in flight until done is called.
// The caller holds the repo lock while it opens the file.
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

// lastUse is when the entry was last saved or restored.
func (e *entry) lastUse() time.Time {
	if e.Used.After(e.Created) {
		return e.Used
	}
	return e.Created
}

// evictable writes rk's recorded restores into idx, then picks entries,
// least recently used first, whose removal lets need more bytes fit in
// budget. It never picks an entry with a download in flight or one a
// restore matched within lookupGrace; when the rest are not enough it
// returns errFull. idx keeps every entry; the caller removes the picked
// ones (dropEntries). The caller holds the repo lock.
func (s *Service) evictable(rk string, idx *index, budget, need int64) (map[int64]bool, error) {
	total := idx.bytes() + need
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if used := s.used[rk]; len(used) > 0 {
		live := make(map[int64]time.Time, len(used))
		for i := range idx.Entries {
			e := &idx.Entries[i]
			if t, ok := used[e.ID]; ok {
				live[e.ID] = t
				if t.After(e.Used) {
					e.Used = t
				}
			}
		}
		s.used[rk] = live // forget ids the index no longer has
	}
	if total <= budget {
		return nil, nil
	}
	order := make([]*entry, 0, len(idx.Entries))
	for i := range idx.Entries {
		order = append(order, &idx.Entries[i])
	}
	slices.SortFunc(order, func(a, b *entry) int {
		if c := a.lastUse().Compare(b.lastUse()); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	drop := map[int64]bool{}
	for _, e := range order {
		if total <= budget {
			break
		}
		if s.reads[rk+"\x00"+e.File] > 0 || now.Sub(e.Used) < lookupGrace {
			continue
		}
		drop[e.ID] = true
		total -= e.Size
	}
	if total > budget {
		return nil, errFull
	}
	return drop, nil
}

// dropEntries removes the entries in ids from idx and returns their files, which
// the caller deletes once idx is saved.
func (s *Service) dropEntries(rk string, idx *index, ids map[int64]bool) (files []string) {
	if len(ids) == 0 {
		return nil
	}
	idx.Entries = slices.DeleteFunc(idx.Entries, func(e entry) bool {
		if ids[e.ID] {
			files = append(files, e.File)
		}
		return ids[e.ID]
	})
	s.mu.Lock()
	for id := range ids {
		delete(s.used[rk], id)
	}
	s.mu.Unlock()
	return files
}

func removeFiles(root *os.Root, files []string) {
	for _, f := range files {
		_ = root.Remove(f)
	}
}
