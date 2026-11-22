// Package repocache keeps each repo's persistent build cache on a device:
// Go's build and module caches, npm, pip and cargo downloads, and the tool
// cache setup actions fill. Jobs used to start with all of them empty, so
// every job downloaded and compiled everything again.
//
// Layout: <root>/<key>/, one folder per repo, where <root> is owned by the
// daemon (root) with mode 0700, so no job can walk from one repo's cache to
// another's. A repo's folder belongs to the CI user and reaches a job only
// through a bind mount the executor sets up for that job. The folder name is
// a hash of the forge and repo, so names never nest (GitLab's group/sub/name
// can't land inside group/sub's cache).
//
// The total is capped. After a job, the store measures that repo's folder
// and deletes whole repos, least recently used first, until the total fits;
// a repo with a job on the host is never deleted.
package repocache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// DefaultMax is the cap when Options.Max is zero (config cache_max).
const DefaultMax = 10 << 30

const indexName = "index.json"

var keyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Options configure a Store.
type Options struct {
	Root string // <cache_dir>/repos
	Max  int64  // total bytes; 0 means DefaultMax, below 0 means no cache
	// Chown hands a repo's folder to the CI user (Lchown in production).
	Chown func(path string, uid, gid int) error
	// InUse returns the keys of repos that have a job on the host. The store
	// calls it under its lock, right before it deletes anything, so a job
	// that registers itself before Prepare is never evicted under its feet.
	// An error means no eviction.
	InUse func() (map[string]bool, error)
	Now   func() time.Time
	// Owner is the uid that must own Root (the daemon's: root). Tests set
	// their own; -1 means os.Geteuid().
	Owner int
}

// Store is one device's repo caches.
type Store struct {
	o  Options
	mu sync.Mutex
}

type entry struct {
	Forge    string    `json:"forge"`
	Repo     string    `json:"repo"`
	LastUsed time.Time `json:"last_used"`
	Bytes    int64     `json:"bytes"`
}

type index struct {
	Repos map[string]*entry `json:"repos"`
}

// New returns a Store. It touches nothing until a method is called.
func New(o Options) *Store {
	if o.Max == 0 {
		o.Max = DefaultMax
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Owner < 0 {
		o.Owner = os.Geteuid()
	}
	if o.InUse == nil {
		o.InUse = func() (map[string]bool, error) { return nil, errors.New("no in-use source") }
	}
	return &Store{o: o}
}

// Enabled reports whether the store keeps caches at all.
func (s *Store) Enabled() bool { return s != nil && s.o.Max > 0 && s.o.Root != "" }

// Key is the folder name for a repo: the first 128 bits of the SHA-256 of
// the lowercased forge and repo. Forges compare repo names without case.
func Key(c core.CacheRepo) (string, error) {
	if c.IsZero() {
		return "", errors.New("no repo named")
	}
	for _, v := range []string{c.Forge, c.Repo} {
		if strings.ContainsAny(v, "\x00\n") || len(v) > 512 {
			return "", fmt.Errorf("bad cache name %q", trunc(v, 60))
		}
	}
	sum := sha256.Sum256([]byte(strings.ToLower(c.Forge) + "\x00" + strings.ToLower(c.Repo)))
	return hex.EncodeToString(sum[:16]), nil
}

// Path returns where repo c's cache lives on the host.
func (s *Store) Path(c core.CacheRepo) (string, error) {
	k, err := Key(c)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.o.Root, k), nil
}

// ensureRoot makes Root a real folder owned by the daemon with mode 0700,
// or says why it can't be used.
func (s *Store) ensureRoot() error {
	if err := os.MkdirAll(filepath.Dir(s.o.Root), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(s.o.Root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(s.o.Root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", s.o.Root)
	}
	if uid, ok := ownerOf(fi); ok && uid != s.o.Owner {
		return fmt.Errorf("%s is owned by uid %d, not %d; refusing to share caches through it", s.o.Root, uid, s.o.Owner)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(s.o.Root, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Prepare makes repo c's folder (owned by uid:gid, mode 0700), marks the
// repo used now and returns the folder. Call it after the job is
// registered with InUse.
func (s *Store) Prepare(c core.CacheRepo, uid, gid int) (string, error) {
	if !s.Enabled() {
		return "", errors.New("the repo cache is off")
	}
	k, err := Key(c)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureRoot(); err != nil {
		return "", fmt.Errorf("repo cache: %w", err)
	}
	dir := filepath.Join(s.o.Root, k)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("repo cache: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("repo cache: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("repo cache: %s is not a folder", dir)
	}
	if s.o.Chown != nil {
		if err := s.o.Chown(dir, uid, gid); err != nil {
			return "", fmt.Errorf("repo cache: chown %s: %w", dir, err)
		}
	}
	idx := s.load()
	e := idx.Repos[k]
	if e == nil {
		e = &entry{}
		idx.Repos[k] = e
	}
	e.Forge, e.Repo, e.LastUsed = c.Forge, c.Repo, s.o.Now().UTC()
	if err := s.save(idx); err != nil {
		return "", fmt.Errorf("repo cache: %w", err)
	}
	return dir, nil
}

// Done records repo c's size after a job and evicts. It returns the repos
// it deleted.
func (s *Store) Done(c core.CacheRepo) ([]string, error) {
	if !s.Enabled() {
		return nil, nil
	}
	k, err := Key(c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.load()
	if e := idx.Repos[k]; e != nil {
		e.Bytes = usage(filepath.Join(s.o.Root, k))
	}
	return s.evictLocked(idx)
}

// Evict deletes least recently used repos until the total fits the cap.
func (s *Store) Evict() ([]string, error) {
	if !s.Enabled() {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evictLocked(s.load())
}

func (s *Store) evictLocked(idx *index) ([]string, error) {
	// Folders the index doesn't know (a lost index) are measured and go
	// first; index entries with no folder are dropped.
	ents, err := os.ReadDir(s.o.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	onDisk := map[string]bool{}
	for _, d := range ents {
		if !keyRe.MatchString(d.Name()) {
			continue
		}
		onDisk[d.Name()] = true
		if idx.Repos[d.Name()] == nil {
			idx.Repos[d.Name()] = &entry{Bytes: usage(filepath.Join(s.o.Root, d.Name()))}
		}
	}
	var total int64
	keys := make([]string, 0, len(idx.Repos))
	for k, e := range idx.Repos {
		if !onDisk[k] {
			delete(idx.Repos, k)
			continue
		}
		total += e.Bytes
		keys = append(keys, k)
	}
	var evicted []string
	if total > s.o.Max {
		inUse, err := s.o.InUse()
		if err != nil {
			_ = s.save(idx)
			return nil, fmt.Errorf("repo cache: can't tell which repos have jobs, so nothing is evicted: %w", err)
		}
		sort.Slice(keys, func(a, b int) bool {
			ea, eb := idx.Repos[keys[a]], idx.Repos[keys[b]]
			if !ea.LastUsed.Equal(eb.LastUsed) {
				return ea.LastUsed.Before(eb.LastUsed)
			}
			return keys[a] < keys[b]
		})
		for _, k := range keys {
			if total <= s.o.Max {
				break
			}
			if inUse[k] {
				continue
			}
			e := idx.Repos[k]
			if err := os.RemoveAll(filepath.Join(s.o.Root, k)); err != nil {
				_ = s.save(idx)
				return evicted, fmt.Errorf("repo cache: evict %s: %w", k, err)
			}
			total -= e.Bytes
			name := e.Repo
			if name == "" {
				name = k
			}
			evicted = append(evicted, name)
			delete(idx.Repos, k)
		}
	}
	return evicted, s.save(idx)
}

// Usage is the total the index knows, in bytes.
func (s *Store) Usage() int64 {
	if !s.Enabled() {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, e := range s.load().Repos {
		n += e.Bytes
	}
	return n
}

// Size is what the index last measured for repo c's cache, 0 when it
// doesn't know.
func (s *Store) Size(c core.CacheRepo) int64 {
	if !s.Enabled() {
		return 0
	}
	k, err := Key(c)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.load().Repos[k]; e != nil {
		return e.Bytes
	}
	return 0
}

// load reads the index; a missing or unreadable one starts empty (the
// folders on disk are measured again at the next eviction).
func (s *Store) load() *index {
	idx := &index{Repos: map[string]*entry{}}
	data, err := os.ReadFile(filepath.Join(s.o.Root, indexName))
	if err != nil {
		return idx
	}
	if json.Unmarshal(data, idx) != nil || idx.Repos == nil {
		return &index{Repos: map[string]*entry{}}
	}
	for k, e := range idx.Repos {
		if !keyRe.MatchString(k) || e == nil {
			delete(idx.Repos, k)
		}
	}
	return idx
}

func (s *Store) save(idx *index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteAtomic(filepath.Join(s.o.Root, indexName), append(data, '\n'), 0o600)
}

// usage is the disk space a tree takes. It never follows a symlink.
func usage(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			n += diskBytes(fi)
		}
		return nil
	})
	return n
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
