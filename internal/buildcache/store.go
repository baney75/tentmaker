package buildcache

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
)

// Folder is the build cache's folder in a repo's Actions cache folder.
const Folder = "build"

const (
	tmpFolder = Folder + "/tmp"

	// An entry file is magic, the body size (uint64), the meta length
	// (uint16), the meta JSON, then the body. Big endian.
	magic    = "TMB1"
	hdrFixed = len(magic) + 8 + 2
	maxMeta  = 4096

	touchEvery = time.Minute
	staleTmp   = 6 * time.Hour
)

var (
	errTooLarge = errors.New("the entry is over the cap")
	errDigest   = errors.New("the body's sha256 is not its name")
	errShort    = errors.New("the body is shorter than its Content-Length")
	errTorn     = errors.New("the entry is not whole")
	errNoRight  = errors.New("the job may no longer write")
)

// meta is what an entry keeps beside its body (Turborepo's headers).
type meta struct {
	Tag      string `json:"tag,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// repoState is what the cache knows of one repo's entries.
type repoState struct {
	mu    sync.Mutex // serializes commits and eviction
	known bool       // size was read from disk
	size  int64
}

func (c *Cache) repo(key string) *repoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	rs := c.repos[key]
	if rs == nil {
		rs = &repoState{}
		c.repos[key] = rs
	}
	return rs
}

// entryPath is where key's entry of kind k lives. The name is a hash of
// the key, so no key can name another path.
func entryPath(k kind, key string) string {
	sum := sha256.Sum256([]byte(key))
	return Folder + "/" + k.name + "/" + hex.EncodeToString(sum[:])
}

type entry struct {
	root *os.Root
	f    *os.File
	size int64 // body bytes
	meta meta
}

func (e *entry) close() { e.f.Close(); e.root.Close() }

// open opens a whole entry, positioned at its body, and records the use.
func (c *Cache) open(who Caller, k kind, key string) (*entry, error) {
	if who.Open == nil {
		return nil, fs.ErrNotExist
	}
	root, err := who.Open(false)
	if err != nil {
		return nil, err
	}
	p := entryPath(k, key)
	f, err := root.Open(p)
	if err != nil {
		root.Close()
		return nil, err
	}
	e := &entry{root: root, f: f}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = errTorn
	}
	if err == nil {
		err = e.readHeader(fi.Size())
	}
	if err != nil {
		e.close()
		return nil, err
	}
	if now := c.o.Now(); now.Sub(fi.ModTime()) > touchEvery {
		_ = root.Chtimes(p, now, now)
	}
	return e, nil
}

func (e *entry) readHeader(total int64) error {
	var b [hdrFixed]byte
	if _, err := io.ReadFull(e.f, b[:]); err != nil || string(b[:len(magic)]) != magic {
		return errTorn
	}
	size := binary.BigEndian.Uint64(b[len(magic):])
	ml := int(binary.BigEndian.Uint16(b[len(magic)+8:]))
	if ml > maxMeta || size > uint64(total) || int64(hdrFixed+ml)+int64(size) != total {
		return errTorn
	}
	if ml > 0 {
		mb := make([]byte, ml)
		if _, err := io.ReadFull(e.f, mb); err != nil || json.Unmarshal(mb, &e.meta) != nil {
			return errTorn
		}
	}
	e.size = int64(size)
	return nil
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// writerOnly hides a file's ReadFrom so the copy uses the pooled buffer.
type writerOnly struct{ io.Writer }

// store writes r's body as key's entry of kind k: to a temp file, checked,
// then renamed into place. It returns the body's size.
func (c *Cache) store(r *http.Request, who Caller, k kind, key string, m meta, limit int64) (int64, error) {
	if who.Open == nil || who.Repo == "" {
		return 0, errors.New("no repo")
	}
	root, err := who.Open(true)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	for _, d := range []string{Folder, tmpFolder, Folder + "/" + k.name} {
		if err := root.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return 0, err
		}
	}
	var mb []byte
	if m != (meta{}) {
		if mb, err = json.Marshal(m); err != nil || len(mb) > maxMeta {
			mb = nil
		}
	}
	tmp := tmpFolder + "/" + randomHex(8)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	kept := false
	defer func() {
		if !kept {
			f.Close()
			_ = root.Remove(tmp)
		}
	}()
	hdr := make([]byte, hdrFixed+len(mb))
	copy(hdr, magic)
	binary.BigEndian.PutUint16(hdr[len(magic)+8:], uint16(len(mb)))
	copy(hdr[hdrFixed:], mb)
	if _, err := f.Write(hdr); err != nil {
		return 0, err
	}
	var dst io.Writer = f
	var h hash.Hash
	if k.verify {
		h = sha256.New()
		dst = io.MultiWriter(f, h)
	}
	bp := bufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(writerOnly{dst}, io.LimitReader(r.Body, limit+1), *bp)
	bufPool.Put(bp)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return 0, errShort
	case err != nil:
		return 0, err
	case n > limit:
		return 0, errTooLarge
	case r.ContentLength >= 0 && n != r.ContentLength:
		return 0, errShort
	case h != nil && hex.EncodeToString(h.Sum(nil)) != key:
		return 0, errDigest
	}
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(n))
	if _, err := f.WriteAt(sz[:], int64(len(magic))); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	kept = true // closed; commit removes tmp itself on failure
	if err := c.commit(root, who, tmp, entryPath(k, key), int64(len(hdr))+n); err != nil {
		_ = root.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// commit renames a written entry into place and keeps the repo in budget,
// least recently used first. The entry just written is never evicted.
func (c *Cache) commit(root *os.Root, who Caller, tmp, final string, size int64) error {
	rs := c.repo(who.Repo)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if !who.mayWrite() {
		return errNoRight
	}
	if !rs.known {
		rs.size = c.sweep(root, -1, "")
		rs.known = true
	}
	var old int64
	if fi, err := root.Lstat(final); err == nil {
		old = fi.Size()
	}
	if err := root.Rename(tmp, final); err != nil {
		return err
	}
	rs.size += size - old
	if b := c.budget(who); rs.size > b {
		// Down to 90%, so the next few writes don't each walk the folder.
		rs.size = c.sweep(root, b-b/10, final)
	}
	return nil
}

type file struct {
	path string
	size int64
	mod  time.Time
}

// sweep removes temp files older than staleTmp and, when target is 0 or
// more, the least recently used entries until the rest fit in target. It
// returns the bytes the entries hold after. The caller holds the repo lock.
func (c *Cache) sweep(root *os.Root, target int64, keep string) int64 {
	now := c.o.Now()
	for _, f := range list(root, tmpFolder) {
		if now.Sub(f.mod) > staleTmp {
			_ = root.Remove(f.path)
		}
	}
	var all []file
	var total int64
	for _, k := range kinds {
		fl := list(root, Folder+"/"+k.name)
		for _, f := range fl {
			total += f.size
		}
		all = append(all, fl...)
	}
	if target < 0 || total <= target {
		return total
	}
	slices.SortFunc(all, func(a, b file) int { return cmp.Or(a.mod.Compare(b.mod), cmp.Compare(a.path, b.path)) })
	for _, f := range all {
		if total <= target {
			break
		}
		if f.path == keep {
			continue
		}
		if err := root.Remove(f.path); err == nil {
			total -= f.size
		}
	}
	return total
}

// list returns the regular files in dir, through root.
func list(root *os.Root, dir string) []file {
	d, err := root.Open(dir)
	if err != nil {
		return nil
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	out := make([]file, 0, len(names))
	for _, n := range names {
		p := dir + "/" + n
		fi, err := root.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, file{path: p, size: fi.Size(), mod: fi.ModTime()})
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return hex.EncodeToString(b)
}
