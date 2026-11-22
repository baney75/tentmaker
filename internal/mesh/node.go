package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

const (
	// DefaultPort is the peer API's default tailnet port.
	DefaultPort = 7433

	maxBody      = 1 << 20
	ioTimeout    = 10 * time.Second
	whoisTimeout = 5 * time.Second
	selfTTL      = 30 * time.Second
	poolHeader   = "Tentmaker-Pool"
	deviceHeader = "Tentmaker-Device"

	// archiveTimeout bounds one source archive stream; maxArchive caps it.
	archiveTimeout       = 10 * time.Minute
	maxArchive     int64 = 2 << 30
)

// Options configures a Node.
type Options struct {
	Device  string
	Pool    string
	Port    int      // 0 = DefaultPort
	Members []string // extra tailnet login names to serve

	Tailscale Tailscale
	// Broker is nil if this device has no credentials. When it also
	// implements core.ArchiveSource, /v1/archive/<ref> streams from it.
	Broker core.WorkSource
	Status func() any // this device's status for pool views
	// Runs is this device's run history; nil serves an empty one.
	Runs RunsSource

	// Version and Platforms are reported by /v1/hello.
	Version   string
	Platforms []core.Platform

	Log *slog.Logger
}

// Node is this device's place in the mesh: it serves the peer API and is a
// client of every other pool device's API.
type Node struct {
	o       Options
	log     *slog.Logger
	members map[string]bool
	leases  *leases
	client  *http.Client
	stream  *http.Client // no overall timeout: archive streams

	// Seams for tests.
	now           func() time.Time
	retryEvery    time.Duration
	discoverEvery time.Duration
	skipFor       time.Duration
	helloTimeout  time.Duration
	callTimeout   time.Duration
	statusTimeout time.Duration
	peerURL       func(p Peer) string
	listen        func(network, addr string) (net.Listener, error)

	selfMu   sync.Mutex
	self     Peer
	selfAt   time.Time
	selfOK   bool
	boundMu  sync.Mutex
	bound    []string
	peersMu  sync.Mutex
	peers    map[string]*peerState // by tailnet node ID
	discMu   sync.Mutex
	discAt   time.Time
	refusals refusalLog
}

// New returns a Node. It does nothing until Run, Brokers, Claim or Pool.
func New(o Options) *Node {
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n := &Node{
		o:             o,
		log:           log,
		members:       map[string]bool{},
		leases:        newLeases(),
		now:           time.Now,
		retryEvery:    15 * time.Second,
		discoverEvery: 30 * time.Second,
		skipFor:       60 * time.Second,
		helloTimeout:  5 * time.Second,
		callTimeout:   3 * time.Second,
		statusTimeout: 2 * time.Second,
		listen:        net.Listen,
		peers:         map[string]*peerState{},
	}
	for _, m := range o.Members {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			n.members[m] = true
		}
	}
	n.peerURL = func(p Peer) string {
		ip := pickIP(p.IPs)
		if !ip.IsValid() {
			return ""
		}
		return "http://" + netip.AddrPortFrom(ip, uint16(n.o.Port)).String()
	}
	tr := &http.Transport{
		Proxy:                 nil, // tailnet traffic never goes through a proxy
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: ioTimeout,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	}
	n.client = &http.Client{Timeout: ioTimeout, Transport: tr}
	// The broker fetches the archive from the forge before it answers, so
	// the first byte may take longer than a JSON call.
	str := tr.Clone()
	str.ResponseHeaderTimeout = 2 * time.Minute
	n.stream = &http.Client{Transport: str}
	return n
}

// pickIP prefers a peer's IPv4 tailnet address.
func pickIP(ips []netip.Addr) netip.Addr {
	for _, ip := range ips {
		if ip.Is4() {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return netip.Addr{}
}

// Run serves the peer API on this device's tailnet IPs only and keeps the
// peer list fresh. If tailscale is not up it retries every 15 s and logs
// once; it returns only when ctx ends.
func (n *Node) Run(ctx context.Context) error {
	down := false
	for {
		ips, lns, err := n.listenTailnet(ctx)
		if err != nil {
			if !down {
				n.log.Warn("mesh: tailscale is not up; the peer API will start when it is", "err", err, "retry", n.retryEvery.String())
				down = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(n.retryEvery):
			}
			continue
		}
		if down {
			n.log.Info("mesh: tailscale is up")
			down = false
		}
		n.serve(ctx, ips, lns)
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (n *Node) listenTailnet(ctx context.Context) ([]netip.Addr, []net.Listener, error) {
	self, err := n.selfPeer(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	var lns []net.Listener
	var errs []error
	for _, ip := range self.IPs {
		ln, err := n.listen("tcp", netip.AddrPortFrom(ip, uint16(n.o.Port)).String())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		if len(errs) == 0 {
			return nil, nil, errors.New("this device has no tailnet IP")
		}
		return nil, nil, fmt.Errorf("listen on the tailnet: %w", errors.Join(errs...))
	}
	for _, e := range errs {
		n.log.Warn("mesh: can't listen on one tailnet address", "err", e)
	}
	return slices.Clone(self.IPs), lns, nil
}

// serve runs the HTTP server until ctx ends or this device's tailnet IPs
// change (or tailscale stays down for two checks in a row).
func (n *Node) serve(ctx context.Context, ips []netip.Addr, lns []net.Listener) {
	srv := &http.Server{
		Handler:           n.handler(),
		ReadHeaderTimeout: ioTimeout,
		ReadTimeout:       ioTimeout,
		WriteTimeout:      ioTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(n.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	var addrs []string
	for _, ln := range lns {
		addrs = append(addrs, ln.Addr().String())
		go func(ln net.Listener) { _ = srv.Serve(ln) }(ln)
	}
	n.boundMu.Lock()
	n.bound = addrs
	n.boundMu.Unlock()
	n.log.Info("mesh: serving the peer API", "addrs", strings.Join(addrs, ","), "pool", n.o.Pool)
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = srv.Shutdown(sctx)
		cancel()
		n.boundMu.Lock()
		n.bound = nil
		n.boundMu.Unlock()
	}()

	n.discover(ctx)
	t := time.NewTicker(n.discoverEvery)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		self, err := n.selfPeer(ctx, true)
		switch {
		case err != nil:
			fails++
			if fails >= 2 {
				n.log.Warn("mesh: tailscale went down; stopping the peer API", "err", err)
				return
			}
			continue
		case !slices.Equal(self.IPs, ips):
			n.log.Info("mesh: tailnet IPs changed; restarting the peer API")
			return
		}
		fails = 0
		n.leases.prune(n.now())
		n.discover(ctx)
	}
}

// boundAddrs lists the addresses the peer API listens on now.
func (n *Node) boundAddrs() []string {
	n.boundMu.Lock()
	defer n.boundMu.Unlock()
	return slices.Clone(n.bound)
}

// selfPeer returns this device, cached for 30 s unless refresh.
func (n *Node) selfPeer(ctx context.Context, refresh bool) (Peer, error) {
	n.selfMu.Lock()
	if !refresh && n.selfOK && n.now().Sub(n.selfAt) < selfTTL {
		p := n.self
		n.selfMu.Unlock()
		return p, nil
	}
	n.selfMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, whoisTimeout)
	defer cancel()
	p, err := n.o.Tailscale.Self(ctx)
	n.selfMu.Lock()
	defer n.selfMu.Unlock()
	if err != nil {
		n.selfOK = false
		return Peer{}, err
	}
	n.self, n.selfAt, n.selfOK = p, n.now(), true
	return p, nil
}

func isTagged(login string, tags []string) bool {
	return len(tags) > 0 || strings.EqualFold(login, TaggedLogin)
}

// allowed reports whether a device owned by (userID, login, tags) may use
// this node: a login listed in Members, or the same person as Self. Tagged
// devices are never "the same person".
func (n *Node) allowed(self Peer, userID int64, login string, tags []string) bool {
	if l := strings.ToLower(login); l != "" && n.members[l] {
		return true
	}
	if isTagged(login, tags) || isTagged(self.LoginName, self.Tags) {
		return false
	}
	return userID != 0 && userID == self.UserID
}

// handler is the peer API with WhoIs auth in front of every route.
func (n *Node) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := n.authorize(w, r)
		if !ok {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, id))
		if r.URL.Path != "/v1/hello" && r.Header.Get(poolHeader) != n.o.Pool {
			n.refuse(r, "", "pool mismatch")
			writeErr(w, http.StatusForbidden, "this device is in another pool")
			return
		}
		switch r.URL.Path {
		case "/v1/hello":
			n.route(w, r, http.MethodGet, n.handleHello)
		case "/v1/status":
			n.route(w, r, http.MethodGet, n.handleStatus)
		case "/v1/work":
			if n.o.Broker == nil {
				writeErr(w, http.StatusNotFound, "this device is not a broker")
				return
			}
			n.route(w, r, http.MethodPost, n.handleWork)
		case "/v1/claim":
			n.route(w, r, http.MethodPost, n.handleClaim)
		case "/v1/release":
			n.route(w, r, http.MethodPost, n.handleRelease)
		case "/v1/runs":
			n.route(w, r, http.MethodGet, n.handleRuns)
		case "/v1/runs/log":
			n.route(w, r, http.MethodGet, n.handleRunLog)
		case "/v1/runs/tests":
			n.route(w, r, http.MethodGet, n.handleRunTests)
		case "/v1/rerun":
			n.route(w, r, http.MethodPost, n.handleRerun)
		default:
			if ref, ok := strings.CutPrefix(r.URL.Path, "/v1/archive/"); ok {
				n.route(w, r, http.MethodGet, func(w http.ResponseWriter, r *http.Request) { n.handleArchive(w, r, ref) })
				return
			}
			writeErr(w, http.StatusNotFound, "no such endpoint")
		}
	})
}

func (n *Node) route(w http.ResponseWriter, r *http.Request, method string, h http.HandlerFunc) {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeErr(w, http.StatusMethodNotAllowed, "use "+method)
		return
	}
	if r.ContentLength > maxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "body is over 1 MiB")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	h(w, r)
}

// callerKey is the request context key of the caller's WhoIs Identity.
type callerKey struct{}

// authorize runs WhoIs on the caller and fails closed. It returns the
// caller's identity.
func (n *Node) authorize(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), whoisTimeout)
	defer cancel()
	self, err := n.selfPeer(ctx, false)
	if err != nil {
		n.refuse(r, "", "can't read this device's tailnet identity: "+err.Error())
		writeErr(w, http.StatusForbidden, "forbidden")
		return Identity{}, false
	}
	if why := fromSelf(self, r.RemoteAddr); why != "" {
		n.refuse(r, "", why)
		writeErr(w, http.StatusForbidden, "forbidden")
		return Identity{}, false
	}
	id, err := n.o.Tailscale.WhoIs(ctx, r.RemoteAddr)
	if err != nil {
		n.refuse(r, "", "whois failed: "+err.Error())
		writeErr(w, http.StatusForbidden, "forbidden")
		return Identity{}, false
	}
	for _, ip := range id.IPs {
		if slices.Contains(self.IPs, ip.Unmap()) {
			n.refuse(r, id.LoginName+" on "+id.Node, "whois names this device itself")
			writeErr(w, http.StatusForbidden, "forbidden")
			return Identity{}, false
		}
	}
	if !n.allowed(self, id.UserID, id.LoginName, id.Tags) {
		n.refuse(r, id.LoginName+" on "+id.Node, "not this tailnet user or a member")
		writeErr(w, http.StatusForbidden, "forbidden")
		return Identity{}, false
	}
	return id, true
}

// fromSelf says why a request from remote comes from this device itself,
// or "" when it doesn't. A device never calls its own peer API over the
// network, and WhoIs would call such a caller this device, a fully trusted
// peer: it is a local process, such as a CI job on Windows (which doesn't
// filter loopback) or wherever the job fence leaks. An address that doesn't
// parse is refused too.
func fromSelf(self Peer, remote string) string {
	ip, err := netip.ParseAddr(splitHostIP(remote))
	if err != nil {
		return "unreadable source address"
	}
	ip = ip.Unmap().WithZone("")
	switch {
	case ip.IsLoopback(), ip.IsUnspecified():
		return "a request from loopback, so from this device itself"
	case slices.Contains(self.IPs, ip):
		return "a request from this device's own tailnet address"
	}
	return ""
}

// rediscoverAfter is how stale the peer table may be before a request from
// an unknown address runs discovery again, so a device that just joined is
// recognized without letting callers force a discovery on every request.
const rediscoverAfter = 5 * time.Second

// callerIs answers 403 unless device is the name the calling tailnet node
// announced in its /v1/hello. Members are other people's devices, so a
// device name in a body or header is only a claim; binding it to the
// WhoIs-checked node stops one device from reporting results or fetching
// archives as another (review r1 partS first-F4). On success it returns
// the request's context carrying the caller's stable node ID
// (core.WithCallerNode): the broker binds each grant to that node, so a
// node that later announces the same name can't act for the grants of one
// that dropped out of discovery (review r2 partS sol-S2-F2).
func (n *Node) callerIs(w http.ResponseWriter, r *http.Request, device string) (context.Context, bool) {
	id, _ := r.Context().Value(callerKey{}).(Identity)
	ctx, cancel := context.WithTimeout(r.Context(), n.helloTimeout+whoisTimeout)
	defer cancel()
	name, why := n.callerDevice(ctx, id, r.RemoteAddr)
	switch {
	case why != "":
	case name != device:
		why = "it sent another device's name"
	default:
		return core.WithCallerNode(r.Context(), id.ID), true
	}
	n.refuse(r, name, why)
	writeErr(w, http.StatusForbidden, "this tailnet device can't act as "+strconv.Quote(device))
	return nil, false
}

// callerDevice returns the device name the pool peer id announced, or why
// there is none: WhoIs gave no stable node ID, no pool peer is that node at
// remote, the name is this device's own, or another pool peer announced
// the same name.
func (n *Node) callerDevice(ctx context.Context, id Identity, remote string) (name, why string) {
	if id.ID == "" {
		return "", "whois gave no stable node ID"
	}
	ap, err := parseRemote(remote)
	if err != nil {
		return "", err.Error()
	}
	ip := ap.Addr()
	look := func() (string, string, bool) {
		n.peersMu.Lock()
		defer n.peersMu.Unlock()
		me := n.peers[id.ID]
		if me == nil || me.id != id.ID || !slices.Contains(me.ips, ip) {
			return "", "", false
		}
		name := me.hello.Device
		if name == "" || name == n.o.Device {
			return "", "it announced no name or this device's own", true
		}
		for _, st := range n.peers {
			if st != nil && st != me && st.hello.Device == name {
				return "", "another pool device announced the same name", true
			}
		}
		return name, "", true
	}
	if name, why, ok := look(); ok {
		return name, why
	}
	n.ensureWithin(ctx, rediscoverAfter)
	if name, why, ok := look(); ok {
		return name, why
	}
	return "", "it is not a known pool device"
}

// refusalLog keeps a refusal log line per caller and reason to one a minute,
// so a noisy device can't flood the log.
type refusalLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (n *Node) refuse(r *http.Request, who, why string) {
	key := splitHostIP(r.RemoteAddr) + "|" + who + "|" + why
	now := n.now()
	n.refusals.mu.Lock()
	if n.refusals.last == nil || len(n.refusals.last) > 4096 {
		n.refusals.last = map[string]time.Time{}
	}
	if t, ok := n.refusals.last[key]; ok && now.Sub(t) < time.Minute {
		n.refusals.mu.Unlock()
		return
	}
	n.refusals.last[key] = now
	n.refusals.mu.Unlock()
	n.log.Warn("mesh: refused a request", "remote", r.RemoteAddr, "caller", who, "path", r.URL.Path, "why", why)
}

type helloResponse struct {
	Device    string          `json:"device"`
	Pool      string          `json:"pool"`
	Version   string          `json:"version"`
	Broker    bool            `json:"broker"`
	Platforms []core.Platform `json:"platforms"`
}

func (n *Node) hello() helloResponse {
	return helloResponse{
		Device:    n.o.Device,
		Pool:      n.o.Pool,
		Version:   n.o.Version,
		Broker:    n.o.Broker != nil,
		Platforms: n.o.Platforms,
	}
}

func (n *Node) handleHello(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, n.hello())
}

func (n *Node) handleStatus(w http.ResponseWriter, r *http.Request) {
	var st any = map[string]any{}
	if n.o.Status != nil {
		st = n.o.Status()
	}
	writeJSON(w, http.StatusOK, st)
}

func (n *Node) handleWork(w http.ResponseWriter, r *http.Request) {
	var req core.WorkRequest
	if !readJSON(w, r, &req) {
		return
	}
	cctx, ok := n.callerIs(w, r, req.Device)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(cctx, ioTimeout)
	defer cancel()
	resp, err := n.o.Broker.Work(ctx, req)
	if errors.Is(err, core.ErrOtherNode) {
		n.refuse(r, req.Device, err.Error())
		writeErr(w, http.StatusForbidden, "this tailnet device can't act as "+strconv.Quote(req.Device)+": "+err.Error())
		return
	}
	if err != nil {
		n.log.Warn("mesh: broker work failed", "device", req.Device, "err", err)
		writeErr(w, http.StatusBadGateway, "broker: "+err.Error())
		return
	}
	for i := range resp.Offers {
		resp.Offers[i].Secret = resp.Offers[i].Grant.Secret
	}
	n.log.Info("mesh: work", "device", req.Device, "free", req.Free, "offers", len(resp.Offers), "stop", len(resp.Stop), "results", len(req.Results))
	writeJSON(w, http.StatusOK, resp)
}

// handleArchive streams a check's source archive from this device's broker
// to the device the check was granted to (the Tentmaker-Device header).
// The broker checks the device and the ref; anything it doesn't have is 404.
func (n *Node) handleArchive(w http.ResponseWriter, r *http.Request, ref string) {
	as, ok := n.o.Broker.(core.ArchiveSource)
	device := r.Header.Get(deviceHeader)
	if !ok || n.o.Broker == nil || !validRef(ref) || device == "" || len(device) > 128 {
		writeErr(w, http.StatusNotFound, "no such archive")
		return
	}
	cctx, ok := n.callerIs(w, r, device)
	if !ok {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(archiveTimeout + time.Minute))
	ctx, cancel := context.WithTimeout(cctx, archiveTimeout)
	defer cancel()
	lw := &lazyWriter{w: w}
	err := as.FetchArchive(ctx, ref, device, lw)
	switch {
	case err == nil:
		if !lw.wrote {
			lw.start()
		}
	case lw.wrote:
		// Mid-stream: cut the connection so the caller sees a broken
		// stream, never a short archive that looks whole.
		n.log.Warn("mesh: archive stream broke", "device", device, "err", err)
		panic(http.ErrAbortHandler)
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such archive")
	default:
		n.log.Warn("mesh: archive failed", "device", device, "err", err)
		writeErr(w, http.StatusBadGateway, "broker: "+err.Error())
	}
}

// lazyWriter sends the 200 header on the first byte, so an error before
// any byte can still answer with an error status.
type lazyWriter struct {
	w     http.ResponseWriter
	wrote bool
}

func (l *lazyWriter) start() {
	l.wrote = true
	l.w.Header().Set("Content-Type", "application/gzip")
	l.w.Header().Set("Cache-Control", "no-store")
	l.w.WriteHeader(http.StatusOK)
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.wrote {
		l.start()
	}
	return l.w.Write(p)
}

func validRef(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type claimRequest struct {
	JobKey string `json:"job_key"`
	Device string `json:"device"`
	TTLS   int64  `json:"ttl_s"`
}

type claimResponse struct {
	Granted bool   `json:"granted"`
	Holder  string `json:"holder"`
}

type releaseRequest struct {
	JobKey string `json:"job_key"`
	Device string `json:"device"`
}

type releaseResponse struct {
	Released bool `json:"released"`
}

func validKeyDevice(key, device string) string {
	switch {
	case key == "" || len(key) > 512:
		return "job_key must be 1 to 512 bytes"
	case device == "" || len(device) > 128:
		return "device must be 1 to 128 bytes"
	}
	return ""
}

func (n *Node) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if !readJSON(w, r, &req) {
		return
	}
	if msg := validKeyDevice(req.JobKey, req.Device); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if req.TTLS <= 0 || req.TTLS > int64(maxLeaseTTL/time.Second) {
		writeErr(w, http.StatusBadRequest, "ttl_s must be 1 to "+strconv.FormatInt(int64(maxLeaseTTL/time.Second), 10))
		return
	}
	ok, holder := n.leases.claim(req.JobKey, req.Device, time.Duration(req.TTLS)*time.Second, n.now())
	writeJSON(w, http.StatusOK, claimResponse{Granted: ok, Holder: holder})
}

func (n *Node) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req releaseRequest
	if !readJSON(w, r, &req) {
		return
	}
	if msg := validKeyDevice(req.JobKey, req.Device); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	writeJSON(w, http.StatusOK, releaseResponse{Released: n.leases.release(req.JobKey, req.Device)})
}

// readJSON reads the whole body (capped at 1 MiB by route) and decodes it
// as exactly one JSON value. Trailing data, such as a second object, is a
// bad body: a peer that sends it is confused or probing.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "body is over 1 MiB")
			return false
		}
		writeErr(w, http.StatusBadRequest, "can't read the body")
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return false
	}
	return true
}

type errorBody struct {
	Error string `json:"error"`
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code = http.StatusInternalServerError
		b = []byte(`{"error":"can't encode the answer"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}
