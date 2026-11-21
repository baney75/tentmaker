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
)

// Options configures a Node.
type Options struct {
	Device  string
	Pool    string
	Port    int      // 0 = DefaultPort
	Members []string // extra tailnet login names to serve

	Tailscale Tailscale
	Broker    core.WorkSource // nil if this device has no credentials
	Status    func() any      // this device's status for pool views

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
	n.client = &http.Client{
		Timeout: ioTimeout,
		Transport: &http.Transport{
			Proxy:                 nil, // tailnet traffic never goes through a proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: ioTimeout,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
		},
	}
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
		if !n.authorize(w, r) {
			return
		}
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
		default:
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

// authorize runs WhoIs on the caller and fails closed.
func (n *Node) authorize(w http.ResponseWriter, r *http.Request) bool {
	ctx, cancel := context.WithTimeout(r.Context(), whoisTimeout)
	defer cancel()
	self, err := n.selfPeer(ctx, false)
	if err != nil {
		n.refuse(r, "", "can't read this device's tailnet identity: "+err.Error())
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	id, err := n.o.Tailscale.WhoIs(ctx, r.RemoteAddr)
	if err != nil {
		n.refuse(r, "", "whois failed: "+err.Error())
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	if !n.allowed(self, id.UserID, id.LoginName, id.Tags) {
		n.refuse(r, id.LoginName+" on "+id.Node, "not this tailnet user or a member")
		writeErr(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
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
	ctx, cancel := context.WithTimeout(r.Context(), ioTimeout)
	defer cancel()
	resp, err := n.o.Broker.Work(ctx, req)
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

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "body is over 1 MiB")
			return false
		}
		writeErr(w, http.StatusBadRequest, "bad JSON body")
		return false
	}
	// Drain the rest so an oversized trailer is still caught.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "body is over 1 MiB")
			return false
		}
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
