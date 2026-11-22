// Package actionscache serves the GitHub Actions cache to Linux runner jobs
// from the device's own disk. actions/cache, the setup actions' cache: and
// any other @actions/cache user call the cache service v2 (Twirp, under
// ACTIONS_RESULTS_URL) and then upload and download through the Azure Blob
// URLs it hands out. This package answers both on 127.0.0.1:7436, so a
// save or restore runs at local disk speed instead of crossing the
// internet, with no workflow change.
//
// Who is calling. Every Linux job has a network namespace of its own whose
// only way to the host's loopback is its own pasta process (the job's
// uplink unit), so a connection to 7436 comes from exactly one job's
// pasta. The service names the job from the connection (JobOf), never from
// anything the job sends. Jobs share the runner uid, so a job can join
// another's namespaces and pass as it; the worker therefore holds every
// job to read unless all jobs on the device are confirmed writers of one
// repo and ref (Hold). Nothing else could carry a token: the toolkit
// drops the path of ACTIONS_RESULTS_URL when it builds a Twirp URL, so
// there is no per-job URL.
// The upload and download URLs it hands out also carry the job's
// capability, 32 random bytes in the sig parameter (which the toolkit masks
// in logs); a URL works only for the job it was made for, on that job's own
// connection, and only while the job runs.
//
// What a job may do. The worker grants each job a Binding at start: its
// repo, its ref scope, the default branch's scope and an Access (none, read
// or write, from the worker's repo cache decision). A job reads its own
// ref's entries first, then the default branch's, and writes only to its
// own ref; GitHub scopes caches the same way. A JIT runner may take another
// job than the one it was granted for, so until the broker confirms from
// the forge which job it took (Binding.Confirm), the job reads only the
// default branch and writes nothing. The runtime token's claims are never
// trusted: their signature can't be checked here. Entries are immutable: a second create for the
// same key and version in a scope is refused. An upload becomes an entry
// only at Finalize, when its size matches.
//
// Everything else under /twirp/ (artifacts) goes unchanged to
// results-receiver.actions.githubusercontent.com.
package actionscache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/repocache"
)

// DefaultUpstream is where every /twirp/ call that isn't the cache service
// goes (the artifact service and anything newer).
const DefaultUpstream = "https://results-receiver.actions.githubusercontent.com"

// Caps on what one job may upload. MaxJob is also capped at the repo's
// Actions budget (budgetOf).
const (
	DefaultMaxEntry = 10 << 30 // GitHub's own cap on one cache entry
	DefaultMaxJob   = 20 << 30
)

const (
	twirpPrefix  = "/twirp/"
	cachePrefix  = twirpPrefix + "github.actions.results.api.v1.CacheService/"
	uploadPrefix = "/devstoreaccount1/upload/"
	blobPrefix   = "/devstoreaccount1/blob/"

	maxTwirpBody = 64 << 10
	maxBlockList = 4 << 20

	twirpTimeout    = time.Minute
	transferTimeout = time.Hour
	proxyTimeout    = 5 * time.Minute
)

// Access is what a job may do with its repo's Actions cache.
type Access int

const (
	None  Access = iota // no cache; the artifact passthrough still works
	Read                // restore only
	Write               // restore, and save to its own ref
)

func (a Access) String() string {
	switch a {
	case Read:
		return "read"
	case Write:
		return "write"
	}
	return "none"
}

// ParseAccess reads String's words; anything else is None.
func ParseAccess(s string) Access {
	switch s {
	case "read":
		return Read
	case "write":
		return Write
	}
	return None
}

// Store opens a repo's entry folder (core.ActionsCacher, backed by
// repocache.Store.ActionsRoot).
type Store interface {
	ActionsRoot(repo core.CacheRepo, create bool) (*os.Root, error)
}

// Binding is what one running job may reach.
type Binding struct {
	Job     string         // runner name
	Repo    core.CacheRepo // whose entries
	Ref     string         // the job's own scope (RefScope); "" means none
	Default string         // the default branch's scope (BranchScope); "" unknown
	Access  Access
	Store   Store
	// Confirmed says the broker has read from the forge which job the
	// runner took and the binding was narrowed to it (Confirm). Until then
	// the job reads only the default branch and writes nothing.
	Confirmed bool
}

// Options configure a Service.
type Options struct {
	// Upstream replaces DefaultUpstream (tests).
	Upstream string
	// HTTP makes the passthrough calls; nil means a client with dial,
	// TLS and header timeouts. Each call also has proxyTimeout.
	HTTP *http.Client
	// JobOf names the job behind a connection to the listener, from the
	// connection's two ends, choosing among jobs (ProcPeers.JobOf on
	// Linux). Nil means no job is ever named, so everything is 404.
	JobOf    func(remote, local netip.AddrPort, jobs []string) (string, bool)
	MaxEntry int64 // 0 means DefaultMaxEntry
	MaxJob   int64 // 0 means DefaultMaxJob
	Log      *slog.Logger
	Now      func() time.Time
}

type grant struct {
	Binding
	capability string
	// Shared by every grant a job's binding is replaced with, so bytes an
	// upload in flight adds are never lost.
	used *atomic.Int64 // bytes this job has uploaded
}

func (g *grant) with(b Binding) *grant {
	return &grant{Binding: b, capability: g.capability, used: g.used}
}

// Service is the cache service. It keeps grants and uploads in memory
// only; the worker grants its running jobs again after a restart.
type Service struct {
	o     Options
	log   *slog.Logger
	proxy *httputil.ReverseProxy
	addr  atomic.Value // string

	mu       sync.Mutex
	byJob    map[string]*grant
	byCap    map[string]*grant
	uploads  map[string]*upload // upload id -> upload
	reserved map[string]string  // entry identity -> upload id
	repoMu   map[string]*sync.Mutex
	held     map[string]bool                // jobs held to read (Hold)
	used     map[string]map[int64]time.Time // repo key -> entry id -> restored, not yet in the index
	reads    map[string]int                 // repo key and file -> downloads in flight
}

// New returns a Service. It serves nothing until Serve.
func New(o Options) *Service {
	if o.Upstream == "" {
		o.Upstream = DefaultUpstream
	}
	if o.MaxEntry <= 0 {
		o.MaxEntry = DefaultMaxEntry
	}
	if o.MaxJob <= 0 {
		o.MaxJob = DefaultMaxJob
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
		}}
	}
	s := &Service{
		o:        o,
		log:      o.Log.With("component", "actions-cache"),
		byJob:    map[string]*grant{},
		byCap:    map[string]*grant{},
		uploads:  map[string]*upload{},
		reserved: map[string]string{},
		repoMu:   map[string]*sync.Mutex{},
		held:     map[string]bool{},
		used:     map[string]map[int64]time.Time{},
		reads:    map[string]int{},
	}
	s.addr.Store("")
	s.proxy = s.newProxy()
	return s
}

// Addr is where Serve listens ("" until it does).
func (s *Service) Addr() string { return s.addr.Load().(string) }

// URL is the base URL a job's .env points ACTIONS_RESULTS_URL at, or ""
// while the service isn't listening.
func (s *Service) URL() string {
	a := s.Addr()
	if a == "" {
		return ""
	}
	return "http://" + a + "/"
}

// Grant registers a job, or updates its binding. A job keeps its
// capability across updates, so URLs it holds keep working.
func (s *Service) Grant(b Binding) error {
	if !core.NamePattern.MatchString(b.Job) {
		return errors.New("actions cache: bad job name")
	}
	if b.Access > None && (b.Store == nil || b.Repo.IsZero()) {
		return errors.New("actions cache: a binding with access needs a repo and a store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.byJob[b.Job]; g != nil {
		ng := g.with(b)
		s.byJob[b.Job], s.byCap[g.capability] = ng, ng
		return nil
	}
	c, err := randomHex(32)
	if err != nil {
		return err
	}
	g := &grant{Binding: b, capability: c, used: new(atomic.Int64)}
	s.byJob[b.Job], s.byCap[c] = g, g
	return nil
}

// Limit lowers a job's access to at most a (never raises it). The worker
// calls it when a job of another repo starts beside one that may read: a
// job can read its neighbors' memory, so a cache is only as private as the
// least trusted job on the device.
func (s *Service) Limit(job string, a Access) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.byJob[job]
	if g == nil || g.Access <= a {
		return
	}
	b := g.Binding
	b.Access = a
	ng := g.with(b)
	s.byJob[job], s.byCap[g.capability] = ng, ng
}

// Hold holds a job to read, or lets it write again (held false), without
// touching its binding. The worker holds every job while another job on the
// device is not a confirmed writer of the same repo and ref: jobs share the
// runner uid, so one can enter another's network namespace and its
// connections then name the other job (review second-S1).
func (s *Service) Hold(job string, held bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held {
		s.held[job] = true
	} else {
		delete(s.held, job)
	}
}

// mayWrite is mayWrite with the job's Hold.
func (s *Service) mayWrite(g *grant) (bool, string) {
	if ok, why := mayWrite(g); !ok {
		return false, why
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[g.Job] {
		return false, "another job on this device is not a confirmed writer of this branch"
	}
	return true, ""
}

// Revoke forgets a job: its connections and URLs get 404 from now on, and
// its unfinished uploads are deleted.
func (s *Service) Revoke(job string) {
	s.mu.Lock()
	g := s.byJob[job]
	if g == nil {
		s.mu.Unlock()
		return
	}
	delete(s.byJob, job)
	delete(s.byCap, g.capability)
	delete(s.held, job)
	var drop []*upload
	for id, u := range s.uploads {
		if u.job == job {
			drop = append(drop, u)
			delete(s.uploads, id)
			if s.reserved[u.ident] == id {
				delete(s.reserved, u.ident)
			}
		}
	}
	s.mu.Unlock()
	for _, u := range drop {
		u.discard()
	}
}

// Granted reports the access a job holds, and whether it is registered.
func (s *Service) Granted(job string) (Access, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.byJob[job]
	if g == nil {
		return None, false
	}
	return g.Access, true
}

func (s *Service) byCapability(c string) *grant {
	if len(c) != 64 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byCap[c]
}

func (s *Service) grantOf(job string) *grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byJob[job]
}

func (s *Service) jobs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.byJob))
	for j := range s.byJob {
		out = append(out, j)
	}
	return out
}

// lockRepo serializes index changes of one repo. Transfers never hold it.
func (s *Service) lockRepo(key string) func() {
	s.mu.Lock()
	m := s.repoMu[key]
	if m == nil {
		m = &sync.Mutex{}
		s.repoMu[key] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ---------------------------------------------------------------- serving

type connKey struct{}

type connInfo struct {
	mu  sync.Mutex
	job string
}

// ConnContext gives each connection a slot that remembers its job, so the
// lookup runs once per connection, not once per request.
func (s *Service) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, &connInfo{})
}

// jobOf names the job behind r's connection, and returns its grant.
func (s *Service) jobOf(r *http.Request) *grant {
	ci, _ := r.Context().Value(connKey{}).(*connInfo)
	if ci != nil {
		ci.mu.Lock()
		defer ci.mu.Unlock()
		if ci.job != "" {
			if g := s.grantOf(ci.job); g != nil {
				return g
			}
		}
	}
	if s.o.JobOf == nil {
		return nil
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	var local netip.AddrPort
	if ta, ok := la.(*net.TCPAddr); ok {
		local = ta.AddrPort()
	}
	job, ok := s.o.JobOf(unmap(remote), unmap(local), s.jobs())
	if !ok {
		return nil
	}
	g := s.grantOf(job)
	if g != nil && ci != nil {
		ci.job = job
	}
	return g
}

func unmap(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// ServeHTTP routes a request. Only loopback Host names are served (no DNS
// rebinding), and nothing is served to a connection no job owns.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
		http.Error(w, "the actions cache answers only on loopback", http.StatusForbidden)
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, cachePrefix):
		s.twirp(w, r, strings.TrimPrefix(p, cachePrefix))
	case strings.HasPrefix(p, twirpPrefix):
		s.passthrough(w, r)
	case strings.HasPrefix(p, uploadPrefix):
		s.upload(w, r, strings.TrimPrefix(p, uploadPrefix))
	case strings.HasPrefix(p, blobPrefix):
		s.download(w, r, strings.TrimPrefix(p, blobPrefix))
	default:
		http.NotFound(w, r)
	}
}

// Serve answers on ln until ctx ends. It sets no server-wide read or write
// timeout: each request sets its own deadlines (a 1 GiB restore takes
// longer than a Twirp call).
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ConnContext:       s.ConnContext,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	s.addr.Store(ln.Addr().String())
	defer s.addr.Store("")
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func deadline(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	t := time.Now().Add(d)
	_ = rc.SetReadDeadline(t)
	_ = rc.SetWriteDeadline(t)
}

// ---------------------------------------------------------------- passthrough

func (s *Service) newProxy() *httputil.ReverseProxy {
	up, err := url.Parse(s.o.Upstream)
	if err != nil {
		up = &url.URL{Scheme: "https", Host: "invalid.invalid"}
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = up.Host
		},
		Transport: s.o.HTTP.Transport,
		ErrorLog:  slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// err names the URL, never a header.
			s.log.Warn("results passthrough failed", "path", r.URL.Path, "error", err)
			writeTwirpError(w, http.StatusBadGateway, "unavailable", "the results service is unreachable from this device")
		},
	}
}

// passthrough forwards a /twirp/ call that isn't the cache service, with
// its Authorization header, to the results service. Only a known job's
// connection may use it.
func (s *Service) passthrough(w http.ResponseWriter, r *http.Request) {
	if s.jobOf(r) == nil {
		writeTwirpError(w, http.StatusNotFound, "not_found", "no Tentmaker job on this connection")
		return
	}
	deadline(w, proxyTimeout+time.Minute)
	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func repoKey(c core.CacheRepo) string {
	k, err := repocache.Key(c)
	if err != nil {
		return ""
	}
	return k
}
