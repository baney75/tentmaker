package repocache

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Minute); return c.t }

func newStore(t *testing.T, max int64, inUse func() (map[string]bool, error)) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repos")
	c := &clock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	return New(Options{Root: root, Max: max, Now: c.now, InUse: inUse, Owner: os.Geteuid()}), root
}

func repo(name string) core.CacheRepo { return core.CacheRepo{Forge: "github", Repo: name} }

func fill(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKeysAreFlatAndDistinct(t *testing.T) {
	a, _ := Key(repo("group/sub"))
	b, _ := Key(repo("group/sub/name"))
	c, _ := Key(core.CacheRepo{Forge: "forgejo", Repo: "group/sub"})
	same, _ := Key(repo("Group/SUB"))
	if a == b || a == c || b == c {
		t.Fatalf("keys collide: %s %s %s", a, b, c)
	}
	if a != same {
		t.Fatalf("case should not matter: %s vs %s", a, same)
	}
	for _, k := range []string{a, b, c} {
		if strings.ContainsAny(k, `/\.`) || !keyRe.MatchString(k) {
			t.Fatalf("key %q is not one flat hex name", k)
		}
	}
	if _, err := Key(core.CacheRepo{Forge: "github"}); err == nil {
		t.Fatal("an empty repo must have no key")
	}
}

func TestPrepareConfinesRoot(t *testing.T) {
	s, root := newStore(t, 1<<30, nil)
	dir, err := s.Prepare(repo("acme/app"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != root {
		t.Fatalf("cache %s is not directly under %s", dir, root)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("root mode %v, want 0700 so no job can walk to another repo's cache", fi.Mode().Perm())
	}
	other, _ := s.Path(repo("acme/other"))
	if other == dir || filepath.Dir(other) != root {
		t.Fatalf("two repos share or nest a cache: %s %s", dir, other)
	}
	// A loosened root is tightened again.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prepare(repo("acme/app"), os.Getuid(), os.Getgid()); err != nil {
			t.Fatal(err)
		}
		fi, _ = os.Stat(root)
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("root mode %v after Prepare, want 0700", fi.Mode().Perm())
		}
	}
}

func TestPrepareRefusesRootOwnedByAnother(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no uid owners on Windows")
	}
	root := filepath.Join(t.TempDir(), "repos")
	s := New(Options{Root: root, Max: 1 << 30, Owner: os.Geteuid() + 1})
	if _, err := s.Prepare(repo("acme/app"), 1, 1); err == nil {
		t.Fatal("a root owned by another uid must be refused")
	}
}

func TestPrepareRefusesSymlinkedRepoDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, root := newStore(t, 1<<30, nil)
	if _, err := s.Prepare(repo("acme/app"), os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	p, _ := s.Path(repo("acme/evil"))
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(repo("acme/evil"), os.Getuid(), os.Getgid()); err == nil {
		t.Fatalf("Prepare followed a symlink in %s", root)
	}
}

func TestEvictLRUNeverEvictsARepoInUse(t *testing.T) {
	inUse := map[string]bool{}
	s, _ := newStore(t, 5<<20, func() (map[string]bool, error) { return inUse, nil })
	var dirs []string
	names := []string{"acme/oldest", "acme/middle", "acme/newest"}
	for _, n := range names {
		d, err := s.Prepare(repo(n), os.Getuid(), os.Getgid())
		if err != nil {
			t.Fatal(err)
		}
		fill(t, d, 2<<20)
		dirs = append(dirs, d)
	}
	// The oldest has a job running: the middle one goes instead.
	k, _ := Key(repo("acme/oldest"))
	inUse[k] = true
	for _, n := range names {
		if _, err := s.Done(repo(n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(dirs[0]); err != nil {
		t.Fatalf("the repo in use was evicted: %v", err)
	}
	if _, err := os.Stat(dirs[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the least recently used idle repo should be gone: %v", err)
	}
	// Once it is idle, the oldest goes next when another repo needs room.
	delete(inUse, k)
	d, err := s.Prepare(repo("acme/fourth"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, d, 2<<20)
	got, err := s.Done(repo("acme/fourth"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "acme/oldest" {
		t.Fatalf("evicted %v, want [acme/oldest]", got)
	}
	for _, keep := range []string{dirs[2], d} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("recently used repo %s should stay: %v", keep, err)
		}
	}
}

func TestEvictFailsClosedWhenInUseUnknown(t *testing.T) {
	s, _ := newStore(t, 1<<20, func() (map[string]bool, error) { return nil, errors.New("state unreadable") })
	d, err := s.Prepare(repo("acme/app"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, d, 4<<20)
	if _, err := s.Done(repo("acme/app")); err == nil {
		t.Fatal("eviction must report that it could not tell what is in use")
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatalf("nothing may be evicted when in-use is unknown: %v", err)
	}
}

func TestEvictMeasuresFoldersTheIndexLost(t *testing.T) {
	s, root := newStore(t, 1<<20, func() (map[string]bool, error) { return nil, nil })
	d, err := s.Prepare(repo("acme/app"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, d, 4<<20)
	if err := os.Remove(filepath.Join(root, indexName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evict(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an oversized folder the index lost should be evicted: %v", err)
	}
}

func TestDisabled(t *testing.T) {
	s := New(Options{Root: t.TempDir(), Max: -1})
	if s.Enabled() {
		t.Fatal("Max below zero turns the cache off")
	}
	if _, err := s.Prepare(repo("acme/app"), 1, 1); err == nil {
		t.Fatal("Prepare must fail when the cache is off")
	}
}

func TestPrepareContainersSitsUnderDaemonFolders(t *testing.T) {
	s, root := newStore(t, 1<<30, nil)
	store, err := s.PrepareContainers(repo("acme/app"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := s.ContainersPath(repo("acme/app"))
	if store != want {
		t.Fatalf("store %s, ContainersPath %s", store, want)
	}
	repoDir, _ := s.Path(repo("acme/app"))
	if strings.HasPrefix(store, repoDir+string(filepath.Separator)) {
		t.Fatalf("store %s is inside the CI user's repo folder %s, where a job could swap the path for a symlink", store, repoDir)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Every folder from Root down to the store is the daemon's, mode 0700.
	for d := filepath.Dir(store); d != filepath.Dir(root); d = filepath.Dir(d) {
		fi, err := os.Lstat(d)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: mode %v, want a 0700 folder", d, fi.Mode())
		}
	}
	other, _ := s.ContainersPath(repo("acme/other"))
	if filepath.Dir(other) == filepath.Dir(store) {
		t.Fatalf("two repos share a container store folder: %s %s", store, other)
	}
}

func TestPrepareContainersRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, root := newStore(t, 1<<30, nil)
	victim := t.TempDir()
	// The store folder itself, planted as a symlink to another place.
	if _, err := s.PrepareContainers(repo("acme/app"), os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	store, _ := s.ContainersPath(repo("acme/evil"))
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, store); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareContainers(repo("acme/evil"), os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("PrepareContainers accepted a symlink as the store")
	}
	// The key folder above it.
	key, _ := s.ContainersPath(repo("acme/evil2"))
	if err := os.Symlink(victim, filepath.Dir(key)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareContainers(repo("acme/evil2"), os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("PrepareContainers followed a symlinked key folder")
	}
	if ents, _ := os.ReadDir(victim); len(ents) != 0 {
		t.Fatalf("PrepareContainers wrote through a symlink into %s", victim)
	}
	_ = root
}

func TestEvictCountsAndRemovesContainerStores(t *testing.T) {
	inUse := map[string]bool{}
	s, _ := newStore(t, 5<<20, func() (map[string]bool, error) { return inUse, nil })
	old, err := s.PrepareContainers(repo("acme/old"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, old, 4<<20) // only the container store holds anything
	if _, err := s.Done(repo("acme/old")); err != nil {
		t.Fatal(err)
	}
	if got := s.Size(repo("acme/old")); got < 4<<20 {
		t.Fatalf("size %d leaves out the container store", got)
	}
	// A bound store counts as in use: it is never evicted.
	k, _ := Key(repo("acme/old"))
	kn, _ := Key(repo("acme/new"))
	inUse[k], inUse[kn] = true, true
	nw, err := s.Prepare(repo("acme/new"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, nw, 2<<20)
	if _, err := s.Done(repo("acme/new")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("a container store in use was evicted: %v", err)
	}
	delete(inUse, k)
	delete(inUse, kn)
	got, err := s.Evict()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "acme/old" {
		t.Fatalf("evicted %v, want [acme/old]", got)
	}
	if _, err := os.Stat(filepath.Dir(old)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the evicted repo's container store is still there: %v", err)
	}
}
