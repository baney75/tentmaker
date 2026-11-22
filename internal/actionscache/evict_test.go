package actionscache

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/repocache"
)

// budgetStore is a fakeStore with an Actions budget.
type budgetStore struct {
	fakeStore
	n int64
}

func (b budgetStore) ActionsBudget() int64 { return b.n }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// writers makes confirmed main writers on a store with budget bytes, one
// per call (a job may upload at most the budget), on a fake clock.
type writers struct {
	h *harness
	c *clock
	n int
	b int64
}

func budgetJob(t *testing.T, budget int64) (*harness, *writers, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	h := newHarness(t, Options{Now: c.now})
	return h, &writers{h: h, c: c, b: budget}, c
}

func (w *writers) next() *job {
	w.h.t.Helper()
	w.n++
	name := fmt.Sprintf("tm-a-%d", w.n)
	main := BranchScope("main")
	b := Binding{Job: name, Repo: repo, Ref: main, Default: main, Access: Write, Store: budgetStore{w.h.store, w.b}, Confirmed: true}
	if err := w.h.svc.Grant(b); err != nil {
		w.h.t.Fatal(err)
	}
	return w.h.client(name, "")
}

func (w *writers) lookup(key string) downloadResp { return w.next().lookup(key, ver) }

// keys lists the index's keys and checks each entry's file is on disk and
// no other entry file is.
func keys(t *testing.T, h *harness) []string {
	t.Helper()
	root, err := h.store.ActionsRoot(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	idx, err := loadIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	var out, files []string
	for _, e := range idx.Entries {
		out = append(out, e.Key)
		files = append(files, e.File)
	}
	ents, _ := os.ReadDir(filepath.Join(h.store.dir, mustKey(t), repocache.ActionsDir))
	for _, d := range ents {
		if fileRe.MatchString(d.Name()) && !slices.Contains(files, d.Name()) {
			t.Errorf("entry file %s outlived its entry", d.Name())
		}
	}
	slices.Sort(out)
	return out
}

func mustKey(t *testing.T) string {
	k, err := repocache.Key(repo)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func saveAll(t *testing.T, w *writers, c *clock, size int, ks ...string) {
	t.Helper()
	for _, k := range ks {
		if ok, msg := w.next().save(k, ver, randBytes(size), 4096); !ok {
			t.Fatalf("save %s: %s", k, msg)
		}
		c.add(time.Minute)
	}
}

// Entries past the budget leave oldest first; the repo's build caches
// beside them stay (review first-F1).
func TestEntriesFitTheirBudget(t *testing.T) {
	h, j, c := budgetJob(t, 3000)
	build := filepath.Join(h.store.dir, mustKey(t), "go-build", "obj")
	if err := os.MkdirAll(filepath.Dir(build), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(build, []byte("object"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveAll(t, j, c, 1000, "k1", "k2", "k3", "k4")
	if got := keys(t, h); !slices.Equal(got, []string{"k2", "k3", "k4"}) {
		t.Fatalf("entries %v, want k2 k3 k4", got)
	}
	if _, err := os.Stat(build); err != nil {
		t.Fatalf("the build cache went with the entries: %v", err)
	}
	// One job may not upload more than the budget either.
	big := j.next()
	var cr createResp
	if big.twirp("CreateCacheEntry", createReq{Key: "big", Version: ver}, &cr); !cr.OK {
		t.Fatal(cr.Message)
	}
	if code := big.put(cr.SignedUploadURL, randBytes(3001)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an upload over the budget got %d", code)
	}
}

// A restore counts as use: an old entry just restored outlives a newer one
// nobody read.
func TestRestoredEntryOutlivesUnreadOne(t *testing.T) {
	h, j, c := budgetJob(t, 3000)
	saveAll(t, j, c, 1000, "a", "b", "c")
	if d := j.lookup("a"); !d.OK {
		t.Fatal("no hit for a")
	}
	c.add(2 * lookupGrace)
	saveAll(t, j, c, 1000, "d")
	if got := keys(t, h); !slices.Equal(got, []string{"a", "c", "d"}) {
		t.Fatalf("entries %v, want a c d", got)
	}
}

// An entry no restore matched for 7 days leaves at the next save, under
// budget or not; one restored meanwhile stays.
func TestEntriesExpireAfterSevenDays(t *testing.T) {
	h, j, c := budgetJob(t, 1<<30)
	saveAll(t, j, c, 100, "old", "kept")
	c.add(5 * 24 * time.Hour)
	if d := j.lookup("kept"); !d.OK {
		t.Fatal("no hit for kept")
	}
	c.add(3 * 24 * time.Hour)
	saveAll(t, j, c, 100, "new")
	if got := keys(t, h); !slices.Equal(got, []string{"kept", "new"}) {
		t.Fatalf("entries %v, want kept new", got)
	}
}

// An entry being downloaded is never evicted.
func TestEvictionSparesDownloadsInFlight(t *testing.T) {
	h, j, c := budgetJob(t, 2000)
	saveAll(t, j, c, 1000, "a", "b")
	d := j.lookup("a")
	if !d.OK {
		t.Fatal("no hit for a")
	}
	c.add(2 * lookupGrace)
	// The GET has begun and not finished.
	done := h.svc.reading(repoKey(repo), filepath.Base(urlPath(t, d.SignedDownloadURL)))
	c.add(8 * 24 * time.Hour) // a is stale too, and still stays
	saveAll(t, j, c, 1000, "c")
	done()
	if got := keys(t, h); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("entries %v, want a c", got)
	}
	if code, _ := h.client("tm-a-3", "").get(d.SignedDownloadURL); code != http.StatusOK {
		t.Fatalf("download after the eviction: %d", code)
	}
}

func urlPath(t *testing.T, u string) string {
	r, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.URL.Path
}

// A job held to read (another job on the device is not a confirmed writer
// of its ref) cannot finalize an upload it began while it could write,
// and writes again once released (review second-S1).
func TestHeldJobCannotFinalize(t *testing.T) {
	h := newHarness(t, Options{})
	j := h.job("tm-a-1", BranchScope("main"), BranchScope("main"), Write, "")
	var cr createResp
	if j.twirp("CreateCacheEntry", createReq{Key: "k", Version: ver}, &cr); !cr.OK {
		t.Fatal(cr.Message)
	}
	j.upload(cr.SignedUploadURL, []byte("data"), 4096)
	h.svc.Hold("tm-a-1", true)
	var f finalizeResp
	j.twirp("FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": ver, "size_bytes": "4"}, &f)
	if f.OK {
		t.Fatal("a held job finalized its upload")
	}
	if ok, _ := j.save("k2", ver, []byte("x"), 4096); ok {
		t.Fatal("a held job saved")
	}
	if d := j.lookup("k", ver); d.OK {
		t.Fatal("the held upload became an entry")
	}
	h.svc.Hold("tm-a-1", false)
	if ok, msg := j.save("k", ver, []byte("x"), 4096); !ok {
		t.Fatalf("released job could not save: %s", msg)
	}
}
