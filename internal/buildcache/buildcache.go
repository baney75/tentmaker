// Package buildcache serves remote build caches to Linux runner jobs from
// the device's own disk: the Bazel HTTP remote cache, the Gradle HTTP build
// cache and the Turborepo remote cache API. A warm hit is a local file read
// over loopback instead of a rebuild or a download from a hosted cache.
//
// It answers beside the GitHub Actions cache, on the same listener
// (internal/actionscache), which names the job behind each connection and
// decides what it may do (Caller): the same grants, the same Hold and the
// same confirmation, so the build cache is exactly as private and as safe
// to write as the Actions cache. A connection no job owns never reaches
// this package; the Actions cache answers it 404.
//
// Fail open for speed, closed for safety. Any error reading the cache is a
// miss. A write the job may not make, or one the device can't keep, is read
// and dropped with a 2xx answer, so no tool turns its remote cache off or
// fails a build over it. Only a malformed write (a Bazel CAS body whose
// sha256 is not its name, a short body) or one over the entry cap is
// refused with a 4xx, which each tool reports as a warning.
//
// Storage is one folder per repo, build/ in the repo's Actions cache folder
// (repocache.ActionsDir), with a folder per kind of entry. Each entry is
// written to build/tmp/ and renamed into place, so a reader never sees a
// partial entry, and carries its body size in a small header, so an entry a
// crash cut short reads as a miss. A repo's entries are capped as a whole
// and the least recently used go first: a hit bumps the file's mtime, at
// most once a minute.
package buildcache

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tools, as metrics label them.
const (
	ToolBazel  = "bazel"
	ToolGradle = "gradle"
	ToolTurbo  = "turbo"
)

// Tools lists every tool in a fixed order.
var Tools = []string{ToolBazel, ToolGradle, ToolTurbo}

// Caps.
const (
	DefaultMaxEntry = 2 << 30 // one entry
	DefaultMaxRepo  = 5 << 30 // one repo's entries together
)

const (
	transferTimeout = time.Hour
	smallTimeout    = time.Minute
	maxEventBody    = 1 << 20

	// StoredHeader says on a write's answer whether the device kept it:
	// "stored" or "not-stored".
	StoredHeader = "X-Tentmaker-Build-Cache"
)

var (
	digestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
)

// kind is one keyspace of entries, kept in its own folder.
type kind struct {
	name   string // folder under build/
	tool   string
	verify bool // the key is the body's sha256 (Bazel CAS)
}

var (
	kindAC     = kind{name: "ac", tool: ToolBazel}
	kindCAS    = kind{name: "cas", tool: ToolBazel, verify: true}
	kindGradle = kind{name: "gradle", tool: ToolGradle}
	kindTurbo  = kind{name: "turbo", tool: ToolTurbo}
	kinds      = []kind{kindAC, kindCAS, kindGradle, kindTurbo}
)

// Caller is the job behind a request, as the Actions cache service named it
// from the connection. Nothing in the request itself picks the repo or the
// right to write.
type Caller struct {
	// Repo is the repo's cache key (repocache.Key); it keys sizes and locks.
	Repo string
	// Open opens the repo's Actions cache folder (repocache.Store.ActionsRoot),
	// making it when create is set. The caller of Open closes the root.
	Open func(create bool) (*os.Root, error)
	// MayWrite says whether the job may save now: a confirmed writer, not
	// held. It is asked when a write starts and again just before the
	// entry is renamed into place; nil means never.
	MayWrite func() bool
	// Budget is the most bytes the repo's build cache may hold; 0 or less
	// means Options.MaxRepo. The smaller of the two wins.
	Budget int64
}

// Options configure a Cache.
type Options struct {
	MaxEntry int64 // 0 means DefaultMaxEntry
	MaxRepo  int64 // 0 means DefaultMaxRepo
	Log      *slog.Logger
	Now      func() time.Time
}

// Cache serves the build cache protocols. It keeps only sizes and counters
// in memory; the entries are files.
type Cache struct {
	o     Options
	log   *slog.Logger
	stats map[string]*counters

	mu    sync.Mutex
	repos map[string]*repoState
}

type counters struct {
	hits, misses, stored, notStored, bytesServed, bytesStored atomic.Int64
}

// Stats are one tool's counters since the daemon started.
type Stats struct {
	Tool        string
	Hits        int64 // GETs answered from the cache
	Misses      int64 // GETs with no entry (or one that could not be read)
	Stored      int64 // writes kept
	NotStored   int64 // writes read and dropped: no write right, or an error
	BytesServed int64
	BytesStored int64
}

// New returns a Cache.
func New(o Options) *Cache {
	if o.MaxEntry <= 0 {
		o.MaxEntry = DefaultMaxEntry
	}
	if o.MaxRepo <= 0 {
		o.MaxRepo = DefaultMaxRepo
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Cache{o: o, log: o.Log.With("component", "build-cache"), stats: map[string]*counters{}, repos: map[string]*repoState{}}
	for _, t := range Tools {
		c.stats[t] = &counters{}
	}
	return c
}

// Stats returns every tool's counters, in Tools order.
func (c *Cache) Stats() []Stats {
	out := make([]Stats, 0, len(Tools))
	for _, t := range Tools {
		s := c.stats[t]
		out = append(out, Stats{Tool: t, Hits: s.hits.Load(), Misses: s.misses.Load(), Stored: s.stored.Load(),
			NotStored: s.notStored.Load(), BytesServed: s.bytesServed.Load(), BytesStored: s.bytesStored.Load()})
	}
	return out
}

// Owns says whether path is one of the build cache's.
func Owns(path string) bool {
	for _, p := range []string{"/ac/", "/cas/", "/cache/", "/v8/artifacts"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Serve answers one request of a job the Actions cache service named.
func (c *Cache) Serve(w http.ResponseWriter, r *http.Request, who Caller) {
	p := r.URL.Path
	var k kind
	var key string
	switch {
	case strings.HasPrefix(p, "/ac/"):
		k, key = kindAC, strings.TrimPrefix(p, "/ac/")
	case strings.HasPrefix(p, "/cas/"):
		k, key = kindCAS, strings.TrimPrefix(p, "/cas/")
	case strings.HasPrefix(p, "/cache/"):
		k, key = kindGradle, strings.TrimPrefix(p, "/cache/")
	case p == "/v8/artifacts/status":
		c.turboStatus(w, r)
		return
	case p == "/v8/artifacts/events":
		c.turboEvents(w, r)
		return
	case strings.HasPrefix(p, "/v8/artifacts/"):
		k, key = kindTurbo, strings.TrimPrefix(p, "/v8/artifacts/")
	default:
		http.NotFound(w, r)
		return
	}
	valid := keyRe.MatchString(key)
	if k.tool == ToolBazel {
		valid = digestRe.MatchString(key)
	}
	if !valid {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		deadline(w, transferTimeout)
		c.get(w, r, who, k, key)
	case http.MethodPut:
		deadline(w, transferTimeout)
		c.put(w, r, who, k, key)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, "the build cache takes GET, HEAD and PUT", http.StatusMethodNotAllowed)
	}
}

// get answers a lookup: 200 with the body, or 404 for anything that is not
// a whole, readable entry.
func (c *Cache) get(w http.ResponseWriter, r *http.Request, who Caller, k kind, key string) {
	st := c.stats[k.tool]
	e, err := c.open(who, k, key)
	if err != nil {
		if r.Method == http.MethodGet {
			st.misses.Add(1)
		}
		http.NotFound(w, r)
		return
	}
	defer e.close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(e.size, 10))
	if k == kindTurbo {
		if e.meta.Tag != "" {
			h.Set("x-artifact-tag", e.meta.Tag)
		}
		if e.meta.Duration != "" {
			h.Set("x-artifact-duration", e.meta.Duration)
		}
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	st.hits.Add(1)
	w.WriteHeader(http.StatusOK)
	// A LimitedReader over the file keeps sendfile.
	n, _ := io.Copy(w, io.LimitReader(e.f, e.size))
	st.bytesServed.Add(n)
}

// put answers a save.
func (c *Cache) put(w http.ResponseWriter, r *http.Request, who Caller, k kind, key string) {
	st := c.stats[k.tool]
	limit := min(c.o.MaxEntry, c.budget(who))
	if r.ContentLength > limit {
		http.Error(w, "the entry is over this device's build cache limit", http.StatusRequestEntityTooLarge)
		return
	}
	if !who.mayWrite() {
		c.drop(w, r, k, key, limit)
		return
	}
	meta := meta{}
	if k == kindTurbo {
		meta.Tag = clip(r.Header.Get("x-artifact-tag"), 1024)
		meta.Duration = clip(r.Header.Get("x-artifact-duration"), 32)
	}
	n, err := c.store(r, who, k, key, meta, limit)
	switch {
	case errors.Is(err, errTooLarge):
		http.Error(w, "the entry is over this device's build cache limit", http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, errDigest):
		http.Error(w, "the body's sha256 is not its name", http.StatusBadRequest)
		return
	case errors.Is(err, errShort):
		http.Error(w, "the body is shorter than its Content-Length", http.StatusBadRequest)
		return
	case errors.Is(err, errNoRight):
		// The right went while the body streamed (held, limited, revoked).
		st.notStored.Add(1)
		c.wrote(w, k, key, "not-stored")
		return
	case err != nil:
		// The device couldn't keep it (a full disk, a folder it may not
		// use). The build goes on; the next lookup is a miss.
		c.log.Warn("build cache entry not stored", "tool", k.tool, "error", err)
		c.drop(w, r, k, key, limit)
		return
	}
	st.stored.Add(1)
	st.bytesStored.Add(n)
	c.wrote(w, k, key, "stored")
}

// drop reads a write's body and answers it as if kept, so the tool keeps
// using the cache; StoredHeader says it was not.
func (c *Cache) drop(w http.ResponseWriter, r *http.Request, k kind, key string, limit int64) {
	n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, limit+1))
	if n > limit {
		http.Error(w, "the entry is over this device's build cache limit", http.StatusRequestEntityTooLarge)
		return
	}
	c.stats[k.tool].notStored.Add(1)
	c.wrote(w, k, key, "not-stored")
}

func (c *Cache) wrote(w http.ResponseWriter, k kind, key, how string) {
	w.Header().Set(StoredHeader, how)
	if k == kindTurbo {
		// The shape of Vercel's answer; turbo reads only the status.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string][]string{"urls": {"/v8/artifacts/" + key}})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// turboStatus is GET /v8/artifacts/status: remote caching is on for any
// job the service named.
func (c *Cache) turboStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"enabled"}`+"\n")
}

// turboEvents is POST /v8/artifacts/events: turbo's usage analytics,
// accepted and ignored.
func (c *Cache) turboEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	deadline(w, smallTimeout)
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxEventBody))
	w.WriteHeader(http.StatusOK)
}

func (who Caller) mayWrite() bool { return who.MayWrite != nil && who.MayWrite() }

func (c *Cache) budget(who Caller) int64 {
	if who.Budget > 0 {
		return min(who.Budget, c.o.MaxRepo)
	}
	return c.o.MaxRepo
}

func deadline(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	t := time.Now().Add(d)
	_ = rc.SetReadDeadline(t)
	_ = rc.SetWriteDeadline(t)
}

func clip(s string, n int) string {
	if len(s) > n || strings.ContainsAny(s, "\r\n\x00") {
		return ""
	}
	return s
}
