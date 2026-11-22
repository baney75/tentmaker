package actionscache

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The cache service v2 messages, in the JSON form Twirp uses (proto field
// names). Unknown fields (metadata) are ignored.
type createReq struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type createResp struct {
	OK              bool   `json:"ok"`
	SignedUploadURL string `json:"signed_upload_url,omitempty"`
	Message         string `json:"message,omitempty"`
}

type finalizeReq struct {
	Key       string    `json:"key"`
	Version   string    `json:"version"`
	SizeBytes flexInt64 `json:"size_bytes"`
}

type finalizeResp struct {
	OK      bool   `json:"ok"`
	EntryID string `json:"entry_id,omitempty"`
	Message string `json:"message,omitempty"`
}

type downloadReq struct {
	Key         string   `json:"key"`
	RestoreKeys []string `json:"restore_keys"`
	Version     string   `json:"version"`
}

type downloadResp struct {
	OK                bool   `json:"ok"`
	SignedDownloadURL string `json:"signed_download_url,omitempty"`
	MatchedKey        string `json:"matched_key,omitempty"`
}

// flexInt64 is an int64 sent as a JSON string (proto3 JSON) or a number.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(n)
	return nil
}

// Limits GitHub sets on keys, and one on versions (a sha256 hex in
// practice).
const (
	maxKeyLen     = 512
	maxVersionLen = 512
	maxRestore    = 10
)

func validKey(k string) bool {
	return k != "" && len(k) <= maxKeyLen && !strings.Contains(k, ",") && utf8.ValidString(k) && !strings.ContainsRune(k, 0)
}

func validVersion(v string) bool {
	return v != "" && len(v) <= maxVersionLen && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

// denied is the message prefix the toolkit treats as a refused save: it
// warns and the step passes.
const denied = "cache write denied: "

func (s *Service) twirp(w http.ResponseWriter, r *http.Request, method string) {
	deadline(w, twirpTimeout)
	if r.Method != http.MethodPost {
		writeTwirpError(w, http.StatusNotFound, "bad_route", "the cache service takes POST")
		return
	}
	g := s.jobOf(r)
	if g == nil {
		writeTwirpError(w, http.StatusNotFound, "not_found", "no Tentmaker job on this connection")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTwirpBody+1))
	if err != nil || len(body) > maxTwirpBody {
		writeTwirpError(w, http.StatusBadRequest, "malformed", "the request body is unreadable or too large")
		return
	}
	switch method {
	case "CreateCacheEntry":
		var q createReq
		if json.Unmarshal(body, &q) != nil || !validKey(q.Key) || !validVersion(q.Version) {
			writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "a cache key or version is missing or invalid")
			return
		}
		writeJSON(w, s.create(g, r, q))
	case "FinalizeCacheEntryUpload":
		var q finalizeReq
		if json.Unmarshal(body, &q) != nil || !validKey(q.Key) || !validVersion(q.Version) || q.SizeBytes < 0 {
			writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "a cache key, version or size is missing or invalid")
			return
		}
		writeJSON(w, s.finalize(g, r, q))
	case "GetCacheEntryDownloadURL":
		var q downloadReq
		if json.Unmarshal(body, &q) != nil || !validKey(q.Key) || !validVersion(q.Version) || len(q.RestoreKeys) > maxRestore {
			writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "a cache key, restore key or version is missing or invalid")
			return
		}
		for _, k := range q.RestoreKeys {
			if !validKey(k) {
				writeTwirpError(w, http.StatusBadRequest, "invalid_argument", "a restore key is invalid")
				return
			}
		}
		writeJSON(w, s.lookup(g, r, q))
	default:
		writeTwirpError(w, http.StatusNotFound, "bad_route", "no such cache method")
	}
}

func ident(rk, scope, key, version string) string {
	return rk + "\x00" + scope + "\x00" + key + "\x00" + version
}

func (s *Service) create(g *grant, r *http.Request, q createReq) createResp {
	if g.Access == None {
		return createResp{Message: denied + "this Tentmaker job has no cache"}
	}
	if ok, why := s.mayWrite(g, r); !ok {
		return createResp{Message: denied + why}
	}
	rk := repoKey(g.Repo)
	if rk == "" {
		return createResp{Message: denied + "the repo has no cache key"}
	}
	root, err := g.Store.ActionsRoot(g.Repo, true)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("actions cache folder unusable", "repo", g.Repo.Repo, "error", err)
		}
		return createResp{Message: denied + "this device has no cache for the repo yet"}
	}
	defer root.Close()
	id, err := randomHex(16)
	if err != nil {
		return createResp{Message: denied + "no randomness"}
	}
	who := ident(rk, g.Ref, q.Key, q.Version)

	unlock := s.lockRepo(rk)
	defer unlock()
	s.sweep(root)
	idx, err := loadIndex(root)
	if err != nil {
		s.log.Warn("actions cache index unreadable", "repo", g.Repo.Repo, "error", err)
		return createResp{Message: denied + "the cache index is unreadable"}
	}
	if idx.find(g.Ref, q.Key, q.Version) != nil {
		// The toolkit says another job may be creating it, and moves on.
		return createResp{Message: "an entry with this key and version already exists"}
	}
	if err := root.Mkdir(stagingName(id), 0o700); err != nil {
		s.log.Warn("actions cache staging failed", "repo", g.Repo.Repo, "error", err)
		return createResp{Message: denied + "the cache folder is not writable"}
	}
	u := &upload{
		id: id, job: g.Job, repo: g.Repo, rk: rk, scope: g.Ref, key: q.Key, version: q.Version,
		ident: who, store: g.Store, blocks: map[string]int64{},
	}
	s.mu.Lock()
	if s.byJob[g.Job] == nil || s.reserved[who] != "" {
		s.mu.Unlock()
		_ = root.RemoveAll(stagingName(id))
		return createResp{Message: "another job is creating an entry with this key and version"}
	}
	s.uploads[id], s.reserved[who] = u, id
	s.mu.Unlock()
	return createResp{OK: true, SignedUploadURL: "http://" + r.Host + uploadPrefix + id + "?sig=" + g.capability}
}

func (s *Service) finalize(g *grant, r *http.Request, q finalizeReq) finalizeResp {
	rk := repoKey(g.Repo)
	who := ident(rk, g.Ref, q.Key, q.Version)
	s.mu.Lock()
	u := s.uploads[s.reserved[who]]
	s.mu.Unlock()
	if u == nil || u.job != g.Job {
		return finalizeResp{Message: "no upload of this key and version is open for this job"}
	}
	// The grant may have been lowered since the upload began.
	if ok, why := s.mayWrite(g, r); !ok {
		s.drop(u)
		return finalizeResp{Message: denied + why}
	}
	e, err := s.commit(u, int64(q.SizeBytes))
	if err != nil {
		s.log.Info("actions cache entry not saved", "job", g.Job, "repo", g.Repo.Repo, "reason", err.Error())
		return finalizeResp{Message: "the entry was not saved: " + err.Error()}
	}
	s.log.Info("actions cache entry saved", "job", g.Job, "repo", g.Repo.Repo, "scope", e.Scope, "bytes", e.Size)
	return finalizeResp{OK: true, EntryID: strconv.FormatInt(e.ID, 10)}
}

func (s *Service) lookup(g *grant, r *http.Request, q downloadReq) downloadResp {
	if g.Access < Read {
		return downloadResp{}
	}
	scopes := s.readScopes(g, r)
	if len(scopes) == 0 {
		return downloadResp{}
	}
	root, err := g.Store.ActionsRoot(g.Repo, false)
	if err != nil {
		return downloadResp{}
	}
	defer root.Close()
	idx, err := loadIndex(root)
	if err != nil {
		return downloadResp{}
	}
	e := idx.match(scopes, q.Key, q.RestoreKeys, q.Version)
	if e == nil {
		return downloadResp{}
	}
	return downloadResp{
		OK:                true,
		SignedDownloadURL: "http://" + r.Host + blobPrefix + e.File + "?sig=" + g.capability,
		MatchedKey:        e.Key,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		writeTwirpError(w, http.StatusInternalServerError, "internal", "encoding failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// writeTwirpError writes a Twirp error. msg must never hold a secret.
func writeTwirpError(w http.ResponseWriter, status int, code, msg string) {
	data, _ := json.Marshal(map[string]string{"code": code, "msg": msg})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
