package actionscache

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
)

const indexName = "index.json"

var (
	fileRe   = regexp.MustCompile(`^e-[0-9a-f]{32}$`)
	uploadRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// entry is one committed cache entry. Its data is the file File in the
// repo's actions folder, written once and never changed.
type entry struct {
	ID      int64     `json:"id"`
	Scope   string    `json:"scope"`
	Key     string    `json:"key"`
	Version string    `json:"version"`
	File    string    `json:"file"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	Created time.Time `json:"created"`
	// Used is when a restore last matched the entry; zero means never.
	Used time.Time `json:"used,omitzero"`
}

// index is index.json in a repo's actions folder.
type index struct {
	Next    int64   `json:"next"`
	Entries []entry `json:"entries"`
}

// loadIndex reads a repo's index. A missing one is empty; one that can't be
// parsed is empty too (its files are then unreachable and leave with the
// repo when it is evicted).
func loadIndex(root *os.Root) (*index, error) {
	data, err := root.ReadFile(indexName)
	if errors.Is(err, fs.ErrNotExist) {
		return &index{Next: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	idx := &index{}
	if json.Unmarshal(data, idx) != nil {
		return &index{Next: 1}, nil
	}
	keep := idx.Entries[:0]
	for _, e := range idx.Entries {
		if fileRe.MatchString(e.File) && e.Size >= 0 {
			keep = append(keep, e)
		}
	}
	idx.Entries = keep
	if idx.Next < 1 {
		idx.Next = 1
	}
	return idx, nil
}

func (idx *index) save(root *os.Root) error {
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return config.WriteAtomicIn(root, indexName, append(data, '\n'), 0o600)
}

func (idx *index) find(scope, key, version string) *entry {
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.Scope == scope && e.Key == key && e.Version == version {
			return e
		}
	}
	return nil
}

func (idx *index) byFile(name string) *entry {
	for i := range idx.Entries {
		if idx.Entries[i].File == name {
			return &idx.Entries[i]
		}
	}
	return nil
}

// match finds what a restore gets, the way GitHub searches: in each scope
// in turn, the exact key, then each restore key as a prefix, newest first.
// The version (paths and compression) must always match.
func (idx *index) match(scopes []string, key string, restore []string, version string) *entry {
	for _, sc := range scopes {
		if e := idx.find(sc, key, version); e != nil {
			return e
		}
		for _, rk := range restore {
			var best *entry
			for i := range idx.Entries {
				e := &idx.Entries[i]
				if e.Scope != sc || e.Version != version || !strings.HasPrefix(e.Key, rk) {
					continue
				}
				if best == nil || e.Created.After(best.Created) || (e.Created.Equal(best.Created) && e.ID > best.ID) {
					best = e
				}
			}
			if best != nil {
				return best
			}
		}
	}
	return nil
}
