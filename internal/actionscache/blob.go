package actionscache

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// The subset of Azure Blob Storage the toolkit's upload (the Azure SDK's
// BlockBlobClient.uploadFile) and download (a plain GET) use:
//
//	PUT  upload/<id>?comp=block&blockid=<b64>  stage a block
//	PUT  upload/<id>?comp=blocklist            commit the staged blocks
//	PUT  upload/<id>                           the whole blob at once
//	GET  blob/<file>                           with Range or x-ms-range
//
// An upload lives in u-<id>/ in the repo's actions folder until Finalize
// moves it to e-<random>, the entry's file.

const azureVersion = "2024-05-04"

// staleStaging is how old an unclaimed staging folder or orphan file must
// be before create sweeps it.
const staleStaging = 6 * time.Hour

func stagingName(id string) string { return "u-" + id }

type upload struct {
	id, job, rk, scope, key, version, ident string
	repo                                    core.CacheRepo
	store                                   Store

	mu       sync.Mutex
	blocks   map[string]int64 // block file name -> size
	order    []string         // block file names, once the list is committed
	whole    bool             // a single PUT arrived
	wholeSum []byte           // its sha256
	total    int64            // bytes reserved against MaxEntry
	busy     bool             // Finalize is running
}

// discard deletes the upload's staging folder.
func (u *upload) discard() {
	root, err := u.store.ActionsRoot(u.repo, false)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.RemoveAll(stagingName(u.id))
}

// drop forgets an upload and deletes what it staged.
func (s *Service) drop(u *upload) {
	s.mu.Lock()
	if s.uploads[u.id] == u {
		delete(s.uploads, u.id)
		if s.reserved[u.ident] == u.id {
			delete(s.reserved, u.ident)
		}
	}
	s.mu.Unlock()
	u.discard()
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// writerOnly hides a destination's ReadFrom, so io.CopyBuffer uses the
// pool's 1 MiB buffer instead of a 32 KiB one.
type writerOnly struct{ io.Writer }

// azureErr writes an Azure Storage error. msg never holds a secret.
func azureErr(w http.ResponseWriter, status int, code, msg string) {
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`,
		xmlText(code), xmlText(msg))
	h := w.Header()
	h.Set("Content-Type", "application/xml")
	h.Set("x-ms-error-code", code)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	azureHeaders(w)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func azureHeaders(w http.ResponseWriter) {
	h := w.Header()
	id, _ := randomHex(16)
	h.Set("x-ms-request-id", id)
	h.Set("x-ms-version", azureVersion)
	h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
}

// blobGrant finds the grant a blob URL's sig names, and checks that the
// connection belongs to the same job.
func (s *Service) blobGrant(r *http.Request) *grant {
	g := s.byCapability(r.URL.Query().Get("sig"))
	if g == nil {
		return nil
	}
	if c := s.jobOf(r); c == nil || c.Job != g.Job {
		return nil
	}
	return g
}

func (s *Service) upload(w http.ResponseWriter, r *http.Request, id string) {
	deadline(w, transferTimeout)
	g := s.blobGrant(r)
	if g == nil || !uploadRe.MatchString(id) {
		azureErr(w, http.StatusNotFound, "ResourceNotFound", "The specified resource does not exist.")
		return
	}
	s.mu.Lock()
	u := s.uploads[id]
	s.mu.Unlock()
	if u == nil || u.job != g.Job {
		azureErr(w, http.StatusNotFound, "ResourceNotFound", "The specified resource does not exist.")
		return
	}
	if r.Method != http.MethodPut {
		azureErr(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "Only PUT is supported here.")
		return
	}
	if ok, _ := s.mayWrite(g); !ok {
		azureErr(w, http.StatusForbidden, "AuthorizationPermissionMismatch", "This job may not write the cache.")
		return
	}
	q := r.URL.Query()
	switch q.Get("comp") {
	case "block":
		s.putBlock(w, r, g, u, q.Get("blockid"))
	case "blocklist":
		s.putBlockList(w, r, u)
	case "":
		s.putWhole(w, r, g, u)
	default:
		azureErr(w, http.StatusBadRequest, "UnsupportedQueryParameter", "Unsupported comp.")
	}
}

// reserve counts n more bytes against the entry and job caps.
func (s *Service) reserve(g *grant, u *upload, n int64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	budget := budgetOf(u.store)
	if u.busy || u.total+n > min(s.o.MaxEntry, budget) {
		return false
	}
	if g.used.Add(n) > min(s.o.MaxJob, budget) {
		g.used.Add(-n)
		return false
	}
	u.total += n
	return true
}

// receive writes exactly n bytes of r's body to name in the staging
// folder, through a temp file, so a cut-off body never leaves a short
// block in place. h, when set, sees every byte.
func receive(u *upload, r *http.Request, name string, n int64, h hash.Hash) error {
	root, err := u.store.ActionsRoot(u.repo, false)
	if err != nil {
		return err
	}
	defer root.Close()
	st, err := root.OpenRoot(stagingName(u.id))
	if err != nil {
		return err
	}
	defer st.Close()
	rnd, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := "p-" + rnd
	f, err := st.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	var dst io.Writer = f
	if h != nil {
		dst = io.MultiWriter(f, h)
	}
	bp := bufPool.Get().(*[]byte)
	got, err := io.CopyBuffer(writerOnly{dst}, io.LimitReader(r.Body, n), *bp)
	bufPool.Put(bp)
	if err == nil && got != n {
		err = io.ErrUnexpectedEOF
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = st.Rename(tmp, name)
	}
	if err != nil {
		_ = st.Remove(tmp)
	}
	return err
}

func (s *Service) bodyLength(w http.ResponseWriter, r *http.Request, g *grant, u *upload) (int64, bool) {
	n := r.ContentLength
	if n < 0 {
		azureErr(w, http.StatusLengthRequired, "MissingContentLengthHeader", "Content-Length is required.")
		return 0, false
	}
	if !s.reserve(g, u, n) {
		azureErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge", "The upload is over this device's cache limit.")
		return 0, false
	}
	return n, true
}

func (s *Service) putBlock(w http.ResponseWriter, r *http.Request, g *grant, u *upload, blockID string) {
	raw, err := base64.StdEncoding.DecodeString(blockID)
	if err != nil || len(raw) == 0 || len(raw) > 64 {
		azureErr(w, http.StatusBadRequest, "InvalidQueryParameterValue", "The block id is invalid.")
		return
	}
	n, ok := s.bodyLength(w, r, g, u)
	if !ok {
		return
	}
	name := "b-" + hex.EncodeToString(raw)
	if err := receive(u, r, name, n, nil); err != nil {
		azureErr(w, http.StatusBadRequest, "InvalidInput", "The block did not arrive whole.")
		return
	}
	u.mu.Lock()
	u.blocks[name] = n
	u.mu.Unlock()
	azureHeaders(w)
	w.Header().Set("Content-Length", "0")
	w.Header().Set("x-ms-request-server-encrypted", "false")
	w.WriteHeader(http.StatusCreated)
}

type blockList struct {
	Items []struct {
		XMLName xml.Name
		ID      string `xml:",chardata"`
	} `xml:",any"`
}

func (s *Service) putBlockList(w http.ResponseWriter, r *http.Request, u *upload) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBlockList+1))
	if err != nil || len(data) > maxBlockList {
		azureErr(w, http.StatusBadRequest, "InvalidXmlDocument", "The block list is unreadable or too large.")
		return
	}
	var bl blockList
	if xml.Unmarshal(data, &bl) != nil || len(bl.Items) == 0 {
		azureErr(w, http.StatusBadRequest, "InvalidXmlDocument", "The block list is invalid.")
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy {
		azureErr(w, http.StatusConflict, "OperationNotAllowedInCurrentState", "The upload is being finalized.")
		return
	}
	order := make([]string, 0, len(bl.Items))
	for _, it := range bl.Items {
		switch it.XMLName.Local {
		case "Latest", "Uncommitted", "Committed":
		default:
			azureErr(w, http.StatusBadRequest, "InvalidXmlDocument", "The block list is invalid.")
			return
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(it.ID))
		name := "b-" + hex.EncodeToString(raw)
		if _, ok := u.blocks[name]; err != nil || !ok {
			azureErr(w, http.StatusBadRequest, "InvalidBlockList", "The block list names a block that was not staged.")
			return
		}
		order = append(order, name)
	}
	u.order = order
	u.whole = false
	azureHeaders(w)
	w.Header().Set("ETag", `"`+u.id+`"`)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", "0")
	w.Header().Set("x-ms-request-server-encrypted", "false")
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) putWhole(w http.ResponseWriter, r *http.Request, g *grant, u *upload) {
	n, ok := s.bodyLength(w, r, g, u)
	if !ok {
		return
	}
	h := sha256.New()
	if err := receive(u, r, "whole", n, h); err != nil {
		azureErr(w, http.StatusBadRequest, "InvalidInput", "The blob did not arrive whole.")
		return
	}
	u.mu.Lock()
	u.whole, u.wholeSum, u.order = true, h.Sum(nil), nil
	u.mu.Unlock()
	azureHeaders(w)
	w.Header().Set("ETag", `"`+u.id+`"`)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", "0")
	w.Header().Set("x-ms-request-server-encrypted", "false")
	w.WriteHeader(http.StatusCreated)
}

// commit turns a finished upload into an entry: it builds the entry file
// (a rename for a single PUT, the blocks in list order otherwise), checks
// its size, and adds it to the index unless the key and version arrived
// in the scope meanwhile. The upload is gone afterward either way.
func (s *Service) commit(u *upload, size int64) (*entry, error) {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return nil, errors.New("already finalizing")
	}
	u.busy = true
	order, whole, sum := append([]string(nil), u.order...), u.whole, u.wholeSum
	sizes := make([]int64, len(order))
	for i, b := range order {
		sizes[i] = u.blocks[b]
	}
	u.mu.Unlock()
	defer s.drop(u)

	root, err := u.store.ActionsRoot(u.repo, false)
	if err != nil {
		return nil, errors.New("the cache folder is gone")
	}
	defer root.Close()
	rnd, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	file := "e-" + rnd
	var total int64
	switch {
	case whole:
		fi, err := root.Stat(stagingName(u.id) + "/whole")
		if err != nil {
			return nil, errors.New("the upload is incomplete")
		}
		total = fi.Size()
		if size > 0 && total != size {
			return nil, fmt.Errorf("the upload has %d bytes, not %d", total, size)
		}
		if err := root.Rename(stagingName(u.id)+"/whole", file); err != nil {
			return nil, errors.New("the entry could not be stored")
		}
	case len(order) > 0:
		for _, n := range sizes {
			total += n
		}
		if size > 0 && total != size {
			return nil, fmt.Errorf("the upload has %d bytes, not %d", total, size)
		}
		if sum, err = assemble(root, stagingName(u.id), order, file); err != nil {
			_ = root.Remove(file)
			return nil, errors.New("the entry could not be stored")
		}
	default:
		return nil, errors.New("nothing was uploaded")
	}

	unlock := s.lockRepo(u.rk)
	defer unlock()
	s.mu.Lock()
	live := s.uploads[u.id] == u
	g := s.byJob[u.job]
	s.mu.Unlock()
	if !live || g == nil {
		_ = root.Remove(file)
		return nil, errors.New("the job ended")
	}
	// Checked again under the repo lock: the job may have been held or
	// lowered while the entry was assembled.
	if ok, why := s.mayWrite(g); !ok {
		_ = root.Remove(file)
		return nil, errors.New(why)
	}
	idx, err := loadIndex(root)
	if err != nil {
		_ = root.Remove(file)
		return nil, errors.New("the cache index is unreadable")
	}
	if idx.find(u.scope, u.key, u.version) != nil {
		_ = root.Remove(file)
		return nil, errors.New("an entry with this key and version already exists")
	}
	e := entry{
		ID: idx.Next, Scope: u.scope, Key: u.key, Version: u.version, File: file,
		Size: total, SHA256: hex.EncodeToString(sum), Created: s.o.Now().UTC(),
	}
	idx.Next++
	idx.Entries = append(idx.Entries, e)
	gone, _ := s.evictLocked(u.rk, idx, budgetOf(u.store), e.ID)
	if err := idx.save(root); err != nil {
		_ = root.Remove(file)
		return nil, errors.New("the cache index could not be written")
	}
	removeFiles(root, gone)
	return &e, nil
}

// assemble writes the blocks, in order, to file and syncs it.
func assemble(root *os.Root, staging string, order []string, file string) ([]byte, error) {
	out, err := root.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	dst := writerOnly{io.MultiWriter(out, h)}
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	for _, b := range order {
		in, err := root.Open(staging + "/" + b)
		if err != nil {
			out.Close()
			return nil, err
		}
		_, err = io.CopyBuffer(dst, in, *bp)
		in.Close()
		if err != nil {
			out.Close()
			return nil, err
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return nil, err
	}
	return h.Sum(nil), out.Close()
}

// sweep evicts repo rk's entries past their age or budget
// (evictLocked), then deletes staging folders no live upload owns and
// entry files the index doesn't name, once they are old (a crash between
// the two writes of a commit, or a daemon restart mid-upload). The caller
// holds the repo lock.
func (s *Service) sweep(root *os.Root, rk string, budget int64) {
	idx, err := loadIndex(root)
	if err != nil {
		return
	}
	if gone, changed := s.evictLocked(rk, idx, budget, 0); changed {
		if idx.save(root) != nil {
			return
		}
		removeFiles(root, gone)
	}
	d, err := root.Open(".")
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	cutoff := s.o.Now().Add(-staleStaging)
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, "u-"):
			s.mu.Lock()
			_, live := s.uploads[strings.TrimPrefix(n, "u-")]
			s.mu.Unlock()
			if live {
				continue
			}
		case fileRe.MatchString(n):
			if idx.byFile(n) != nil {
				continue
			}
		case strings.HasPrefix(n, ".tmp-"):
		default:
			continue
		}
		if fi, err := root.Lstat(n); err == nil && fi.ModTime().Before(cutoff) {
			_ = root.RemoveAll(n)
		}
	}
}

func (s *Service) download(w http.ResponseWriter, r *http.Request, file string) {
	deadline(w, transferTimeout)
	g := s.blobGrant(r)
	if g == nil || g.Access < Read || !fileRe.MatchString(file) {
		azureErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		azureErr(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "Only GET and HEAD are supported here.")
		return
	}
	// Counted before the index is read, so no eviction deletes the file
	// between that read and the open.
	defer s.reading(repoKey(g.Repo), file)()
	root, err := g.Store.ActionsRoot(g.Repo, false)
	if err != nil {
		azureErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
		return
	}
	defer root.Close()
	idx, err := loadIndex(root)
	var e *entry
	if err == nil {
		e = idx.byFile(file)
	}
	// The URL came from a lookup that checked the token; the plain GET
	// carries none, so the binding's two scopes are what count here.
	if e == nil || e.Scope == "" || (e.Scope != g.Ref && e.Scope != g.Default) {
		azureErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
		return
	}
	f, err := root.Open(file)
	if err != nil {
		azureErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != e.Size {
		s.log.Warn("actions cache entry damaged", "repo", g.Repo.Repo, "entry", e.ID)
		azureErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
		return
	}
	if xr := r.Header.Get("x-ms-range"); xr != "" {
		r.Header.Set("Range", xr)
	}
	azureHeaders(w)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("x-ms-blob-type", "BlockBlob")
	h.Set("ETag", `"`+e.SHA256+`"`)
	// ServeContent copies the *os.File with io.CopyN, which the net/http
	// connection turns into sendfile(2).
	http.ServeContent(w, r, "", e.Created, f)
}
