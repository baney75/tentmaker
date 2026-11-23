package mesh

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// Lending machines to a pool (docs/DESIGN.md, Mesh: lent devices).
//
// The pool owner's device makes a one-time invite (Invite) and keeps only a
// hash of its code. Another tailnet user's device redeems it (Join) by
// calling POST /v1/join on that device, which names the caller with WhoIs
// and records it in the guest table (mesh-guests.json) with the invite's
// trust and repos. The owner's devices share the guest table with each
// other. The guest keeps its side in mesh-lent.json: the pool, the host
// device and the owner's tailnet user, from its own WhoIs.

const (
	guestsFile = "mesh-guests.json"
	lentFile   = "mesh-lent.json"

	codePrefix = "tm1-"
	codeBytes  = 20 // 160 random bits

	// MinInviteTTL and MaxInviteTTL bound an invite's life.
	MinInviteTTL = time.Minute
	MaxInviteTTL = 30 * 24 * time.Hour
	// DefaultInviteTTL is an invite's life when none is given.
	DefaultInviteTTL = 24 * time.Hour

	maxInvites      = 100
	maxGuests       = 1000
	revokedKeep     = 30 * 24 * time.Hour
	joinWindow      = 10 * time.Minute
	joinFailsNode   = 5  // failed joins per node per joinWindow
	joinFailsAll    = 30 // failed joins from everyone per joinWindow
	maxLendRepos    = 100
	maxPoolNameSize = 128
)

// Relation kinds of a tailnet node to this device.
const (
	kindOwner = "owner" // the same tailnet user, or a mesh.members login
	kindGuest = "guest" // a node in this pool's guest table
	kindHost  = "host"  // on a lent device: a device of the pool owner
)

// relation is what a tailnet node is to this device.
type relation struct {
	kind string
	// same: the same tailnet user as this device (not a member, not
	// tagged). Only such a device's wake facts are honored, and only it may
	// change the guest table.
	same bool
	lend core.Lend // a guest's
}

// relationKey is the request context key of the caller's relation.
type relationKey struct{}

func callerRelation(ctx context.Context) relation {
	r, _ := ctx.Value(relationKey{}).(relation)
	return r
}

// Guest is a lent device in this pool's guest table. Its identity is the
// WhoIs answer at join: stable node ID, user ID and login.
type Guest struct {
	Node      string    `json:"node"`
	UserID    int64     `json:"user_id"`
	Login     string    `json:"login"`
	Device    string    `json:"device,omitempty"` // the name it announces; for people only
	Trust     string    `json:"trust"`
	Repos     []string  `json:"repos,omitempty"`
	InvitedBy string    `json:"invited_by,omitempty"`
	Joined    time.Time `json:"joined"`
	Revoked   time.Time `json:"revoked,omitzero"`
	Updated   time.Time `json:"updated"`
}

func (g Guest) active() bool { return g.Revoked.IsZero() }

// lend is the guest's trust as a core.Lend; a revoked guest's is revoked.
func (g Guest) lend() core.Lend {
	if !g.active() {
		return core.Lend{Trust: core.TrustRevoked}
	}
	return core.Lend{Trust: g.Trust, Repos: append([]string(nil), g.Repos...)}
}

// invite is an open invite. Only the code's SHA-256 is kept.
type invite struct {
	Hash    string    `json:"hash"`
	Trust   string    `json:"trust"`
	Repos   []string  `json:"repos,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

type guestFile struct {
	Version int       `json:"version"`
	Invites []*invite `json:"invites"`
	Guests  []*Guest  `json:"guests"`
}

// guestTable is the pool's guests and this device's open invites.
type guestTable struct {
	mu      sync.Mutex
	invites []*invite
	guests  map[string]*Guest // by node ID
	fails   map[string][]time.Time
	allFail []time.Time
}

// Lent is this device's side of a pool it joined as a lent device.
type Lent struct {
	Pool        string    `json:"pool"`
	Host        string    `json:"host"`      // the inviting device's name
	HostNode    string    `json:"host_node"` // its stable node ID, from WhoIs
	HostAddr    string    `json:"host_addr"` // its peer API address (ip:port)
	OwnerUserID int64     `json:"owner_user_id"`
	OwnerLogin  string    `json:"owner_login"`
	Trust       string    `json:"trust"`
	Repos       []string  `json:"repos,omitempty"`
	Joined      time.Time `json:"joined"`
}

func (l *Lent) lend() core.Lend {
	return core.Lend{Trust: l.Trust, Repos: append([]string(nil), l.Repos...)}
}

// ---------------------------------------------------------------- files

func (n *Node) guestsPath() string {
	if n.o.StateDir == "" {
		return ""
	}
	return filepath.Join(n.o.StateDir, guestsFile)
}

func (n *Node) lentPath() string {
	if n.o.StateDir == "" {
		return ""
	}
	return filepath.Join(n.o.StateDir, lentFile)
}

// loadLend reads the guest table and this device's lent state. A damaged
// guest table is empty (fail closed: no guest admitted); a damaged lent
// file leaves the device in its own pool.
func (n *Node) loadLend() {
	n.guests.guests, n.guests.fails = map[string]*Guest{}, map[string][]time.Time{}
	if p := n.guestsPath(); p != "" {
		b, err := os.ReadFile(p)
		switch {
		case err == nil:
			var f guestFile
			if err := json.Unmarshal(b, &f); err != nil {
				n.log.Warn("mesh: the guest table is damaged; admitting no guest", "err", err)
				break
			}
			for _, iv := range f.Invites {
				if iv != nil && len(iv.Hash) == 64 && core.ValidLendTrust(iv.Trust) {
					n.guests.invites = append(n.guests.invites, iv)
				}
			}
			for _, g := range f.Guests {
				if validGuest(g) {
					n.guests.guests[g.Node] = g
				}
			}
		case !os.IsNotExist(err):
			n.log.Warn("mesh: can't read the guest table; admitting no guest", "err", err)
		}
	}
	if p := n.lentPath(); p != "" {
		b, err := os.ReadFile(p)
		switch {
		case err == nil:
			var l Lent
			if err := json.Unmarshal(b, &l); err != nil || !validLent(&l) {
				n.log.Warn("mesh: the lent state is damaged; staying in this device's own pool", "err", err)
				break
			}
			n.lent = &l
		case !os.IsNotExist(err):
			n.log.Warn("mesh: can't read the lent state; staying in this device's own pool", "err", err)
		}
	}
}

func validGuest(g *Guest) bool {
	return g != nil && g.Node != "" && len(g.Node) <= 128 && g.UserID != 0 && core.ValidLendTrust(g.Trust) &&
		len(g.Repos) <= maxLendRepos && !g.Updated.IsZero()
}

func validLent(l *Lent) bool {
	_, err := netip.ParseAddrPort(l.HostAddr)
	return l.Pool != "" && len(l.Pool) <= maxPoolNameSize && l.HostNode != "" && l.OwnerUserID != 0 &&
		core.ValidLendTrust(l.Trust) && err == nil && len(l.Repos) <= maxLendRepos
}

// saveGuestsLocked writes the guest table. The caller holds n.guests.mu.
func (n *Node) saveGuestsLocked() error {
	p := n.guestsPath()
	if p == "" {
		return nil
	}
	f := guestFile{Version: 1, Invites: n.guests.invites}
	for _, id := range sortedGuestIDs(n.guests.guests) {
		f.Guests = append(f.Guests, n.guests.guests[id])
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteAtomic(p, append(b, '\n'), 0o600)
}

func sortedGuestIDs(m map[string]*Guest) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// pruneLocked drops expired invites and guests revoked over revokedKeep ago.
func (n *Node) pruneLocked(now time.Time) bool {
	changed := false
	keep := n.guests.invites[:0]
	for _, iv := range n.guests.invites {
		if now.Before(iv.Expires) {
			keep = append(keep, iv)
		} else {
			changed = true
		}
	}
	n.guests.invites = keep
	for id, g := range n.guests.guests {
		if !g.active() && now.Sub(g.Revoked) > revokedKeep {
			delete(n.guests.guests, id)
			changed = true
		}
	}
	return changed
}

// ---------------------------------------------------------------- relations

// pool is the pool this device is in: a joined pool while lent, else
// mesh.pool.
func (n *Node) pool() string {
	if l := n.lentInfo(); l != nil {
		return l.Pool
	}
	return n.o.Pool
}

// lentInfo is a copy of this device's lent state, or nil.
func (n *Node) lentInfo() *Lent {
	n.lentMu.Lock()
	defer n.lentMu.Unlock()
	if n.lent == nil {
		return nil
	}
	l := *n.lent
	l.Repos = append([]string(nil), n.lent.Repos...)
	return &l
}

// sameUser reports whether a node owned by (userID, login, tags) is the
// same tailnet user as self. Tagged devices are never anyone's.
func sameUser(self Peer, userID int64, login string, tags []string) bool {
	if isTagged(login, tags) || isTagged(self.LoginName, self.Tags) {
		return false
	}
	return userID != 0 && userID == self.UserID
}

// relOf says what the tailnet node (id, userID, login, tags) is to this
// device: the owner's (the same user or a member), a guest in the guest
// table (matched by stable node ID and user ID, never by name), or, on a
// lent device, one of the pool owner's devices. The zero relation is
// nobody.
func (n *Node) relOf(self Peer, id string, userID int64, login string, tags []string) relation {
	if sameUser(self, userID, login, tags) {
		return relation{kind: kindOwner, same: true}
	}
	if l := strings.ToLower(login); l != "" && n.members[l] {
		return relation{kind: kindOwner}
	}
	if l := n.lentInfo(); l != nil && !isTagged(login, tags) && userID != 0 && userID == l.OwnerUserID {
		return relation{kind: kindHost}
	}
	if id == "" {
		return relation{}
	}
	n.guests.mu.Lock()
	g := n.guests.guests[id]
	n.guests.mu.Unlock()
	if g != nil && g.UserID == userID && !isTagged(login, tags) {
		return relation{kind: kindGuest, lend: g.lend()}
	}
	return relation{}
}

// routeAllows says whether a caller with relation r may use path. Guests
// reach work and archives alone; a lent device's pool owner reads its
// hello and status alone; only the same user's devices wake devices or
// change the guest table.
func routeAllows(r relation, path string) bool {
	switch {
	case path == "/v1/hello":
		return r.kind != ""
	case path == "/v1/status":
		return r.kind == kindOwner || r.kind == kindHost
	case path == "/v1/work" || strings.HasPrefix(path, "/v1/archive/"):
		return r.kind == kindOwner || r.kind == kindGuest
	case path == "/v1/wake" || path == "/v1/guests":
		return r.kind == kindOwner && r.same
	case path == "/v1/leave":
		return r.kind == kindGuest
	}
	return r.kind == kindOwner
}

// ---------------------------------------------------------------- invite

// InviteCode is a new invite: the code, shown once, and what it grants.
type InviteCode struct {
	Code    string    `json:"code"`
	Trust   string    `json:"trust"`
	Repos   []string  `json:"repos"`
	Expires time.Time `json:"expires"`
}

// Invite makes a one-time join code for this device's pool. The code holds
// this device's tailnet address and 160 random bits; only their SHA-256 is
// kept, with the expiry, trust and repos.
func (n *Node) Invite(ctx context.Context, trust string, repos []string, ttl time.Duration) (InviteCode, error) {
	if !core.ValidLendTrust(trust) {
		return InviteCode{}, fmt.Errorf("trust must be %s or %s", core.TrustTrusted, core.TrustPublic)
	}
	if ttl == 0 {
		ttl = DefaultInviteTTL
	}
	if ttl < MinInviteTTL || ttl > MaxInviteTTL {
		return InviteCode{}, fmt.Errorf("the invite must last 1m to %s", MaxInviteTTL)
	}
	if len(repos) > maxLendRepos {
		return InviteCode{}, fmt.Errorf("at most %d repos", maxLendRepos)
	}
	repos = normRepos(repos)
	for _, r := range repos {
		if o, nm, ok := strings.Cut(r, "/"); !ok || o == "" || nm == "" || strings.Contains(nm, "/") {
			return InviteCode{}, fmt.Errorf("repo %q is not owner/name", r)
		}
	}
	if n.lentInfo() != nil {
		return InviteCode{}, errors.New("this device is lent to another pool; only the pool owner's devices invite")
	}
	sctx, cancel := context.WithTimeout(ctx, whoisTimeout)
	self, err := n.selfPeer(sctx, false)
	cancel()
	if err != nil {
		return InviteCode{}, fmt.Errorf("tailscale is not up: %w", err)
	}
	ip := pickIP(self.IPs)
	if !ip.IsValid() {
		return InviteCode{}, errors.New("this device has no tailnet address")
	}
	var secret [codeBytes]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return InviteCode{}, err
	}
	sec := encodeSecret(secret[:])
	now := n.now()
	iv := &invite{Hash: hashSecret(sec), Trust: trust, Repos: repos, Created: now, Expires: now.Add(ttl)}
	n.guests.mu.Lock()
	n.pruneLocked(now)
	if len(n.guests.invites) >= maxInvites {
		n.guests.mu.Unlock()
		return InviteCode{}, fmt.Errorf("%d invites are open; wait for some to be used or expire", maxInvites)
	}
	n.guests.invites = append(n.guests.invites, iv)
	if err := n.saveGuestsLocked(); err != nil {
		n.guests.invites = n.guests.invites[:len(n.guests.invites)-1]
		n.guests.mu.Unlock()
		return InviteCode{}, fmt.Errorf("can't save the invite: %w", err)
	}
	n.guests.mu.Unlock()
	n.log.Info("mesh: invite made", "trust", trust, "repos", strings.Join(repos, ","), "expires", iv.Expires.UTC().Format(time.RFC3339))
	code := codePrefix + sec + "@" + netip.AddrPortFrom(ip, uint16(n.o.Port)).String()
	return InviteCode{Code: code, Trust: trust, Repos: nonNilStrings(repos), Expires: iv.Expires}, nil
}

func normRepos(rs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rs {
		r = strings.TrimSpace(r)
		if r == "" || seen[strings.ToLower(r)] {
			continue
		}
		seen[strings.ToLower(r)] = true
		out = append(out, r)
	}
	return out
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

var codeEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

func encodeSecret(b []byte) string { return strings.ToLower(codeEnc.EncodeToString(b)) }

func hashSecret(sec string) string {
	h := sha256.Sum256([]byte(sec))
	return hex.EncodeToString(h[:])
}

// errBadCode is the one answer to a code that doesn't parse. It never
// carries the code.
var errBadCode = errors.New("that is not a Tentmaker join code; copy the whole line `tentmaker invite` printed")

// parseCode splits a join code into its secret and the inviting device's
// peer API address.
func parseCode(code string) (secret string, host netip.AddrPort, err error) {
	code = strings.TrimSpace(code)
	rest, ok := strings.CutPrefix(code, codePrefix)
	if !ok {
		return "", netip.AddrPort{}, errBadCode
	}
	sec, addr, ok := strings.Cut(rest, "@")
	if !ok || len(sec) != 32 {
		return "", netip.AddrPort{}, errBadCode
	}
	if b, err := codeEnc.DecodeString(strings.ToUpper(sec)); err != nil || len(b) != codeBytes {
		return "", netip.AddrPort{}, errBadCode
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || ap.Port() == 0 || !ap.Addr().IsValid() {
		return "", netip.AddrPort{}, errBadCode
	}
	return strings.ToLower(sec), netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
}

// ---------------------------------------------------------------- join (host side)

type joinRequest struct {
	Code   string `json:"code"`   // the secret part of the code alone
	Device string `json:"device"` // the name the guest announces; for people only
}

type joinResponse struct {
	Pool   string   `json:"pool"`
	Device string   `json:"device"`
	Trust  string   `json:"trust"`
	Repos  []string `json:"repos"`
}

const badCodeAnswer = "that join code is not valid; ask the pool owner for a new one"

// joinAllowed reports whether node may try a code now: at most
// joinFailsNode failures per node and joinFailsAll in all per joinWindow.
func (n *Node) joinAllowed(node string, now time.Time) bool {
	n.guests.mu.Lock()
	defer n.guests.mu.Unlock()
	cut := func(ts []time.Time) []time.Time {
		k := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < joinWindow {
				k = append(k, t)
			}
		}
		return k
	}
	n.guests.allFail = cut(n.guests.allFail)
	for k, ts := range n.guests.fails {
		if ts = cut(ts); len(ts) == 0 {
			delete(n.guests.fails, k)
		} else {
			n.guests.fails[k] = ts
		}
	}
	return len(n.guests.fails[node]) < joinFailsNode && len(n.guests.allFail) < joinFailsAll
}

func (n *Node) joinFailed(node string, now time.Time) {
	n.guests.mu.Lock()
	defer n.guests.mu.Unlock()
	if len(n.guests.fails) < 4096 {
		n.guests.fails[node] = append(n.guests.fails[node], now)
	}
	n.guests.allFail = append(n.guests.allFail, now)
}

// handleJoin redeems an invite. The guest's identity is WhoIs's answer for
// the connection (stable node ID, user ID, login), never anything in the
// body; its trust and repos are the invite's. A wrong, used or expired code
// gets one answer, and failures are rate limited.
func (n *Node) handleJoin(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(callerKey{}).(Identity)
	now := n.now()
	if !n.joinAllowed(id.ID, now) {
		n.refuse(r, id.LoginName+" on "+id.Node, "too many failed joins")
		writeErr(w, http.StatusTooManyRequests, "too many join attempts; wait 10 minutes")
		return
	}
	var req joinRequest
	if !readJSON(w, r, &req) {
		n.joinFailed(id.ID, now)
		return
	}
	self, err := n.selfPeer(r.Context(), false)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "this device can't read its tailnet identity")
		return
	}
	switch {
	case n.lentInfo() != nil:
		writeErr(w, http.StatusConflict, "this device is lent to another pool; only the pool owner's devices take guests")
		return
	case isTagged(id.LoginName, id.Tags) || id.UserID == 0:
		n.refuse(r, id.LoginName+" on "+id.Node, "a tagged device tried to join")
		writeErr(w, http.StatusForbidden, "a tagged device has no owner to lend it; only a person's device can join")
		return
	case n.relOf(self, id.ID, id.UserID, id.LoginName, id.Tags).kind == kindOwner:
		writeErr(w, http.StatusConflict, "this device is already the pool owner's; it joins by setting mesh.pool, not with a code")
		return
	}
	sec := strings.ToLower(strings.TrimSpace(req.Code))
	want := hashSecret(sec)
	n.guests.mu.Lock()
	n.pruneLocked(now)
	at := -1
	for i, iv := range n.guests.invites {
		if subtle.ConstantTimeCompare([]byte(iv.Hash), []byte(want)) == 1 && now.Before(iv.Expires) {
			at = i
		}
	}
	if at < 0 || len(sec) != 32 {
		n.guests.mu.Unlock()
		n.joinFailed(id.ID, now)
		n.refuse(r, id.LoginName+" on "+id.Node, "a wrong, used or expired join code")
		writeErr(w, http.StatusForbidden, badCodeAnswer)
		return
	}
	iv := n.guests.invites[at]
	oldInvites := append([]*invite(nil), n.guests.invites...)
	old := n.guests.guests[id.ID]
	if old == nil && len(n.guests.guests) >= maxGuests {
		n.guests.mu.Unlock()
		writeErr(w, http.StatusConflict, "this pool has too many guests; remove some first")
		return
	}
	n.guests.invites = append(n.guests.invites[:at:at], n.guests.invites[at+1:]...)
	g := &Guest{Node: id.ID, UserID: id.UserID, Login: id.LoginName, Device: clip(req.Device, 128), Trust: iv.Trust,
		Repos: append([]string(nil), iv.Repos...), InvitedBy: n.o.Device, Joined: now, Updated: now}
	n.guests.guests[id.ID] = g
	if err := n.saveGuestsLocked(); err != nil {
		// Not recorded: put the invite back, so the code still works.
		n.guests.invites = oldInvites
		if old != nil {
			n.guests.guests[id.ID] = old
		} else {
			delete(n.guests.guests, id.ID)
		}
		n.guests.mu.Unlock()
		n.log.Error("mesh: a guest's join could not be saved", "err", err)
		writeErr(w, http.StatusInternalServerError, "the join could not be saved; try again")
		return
	}
	n.guests.mu.Unlock()
	n.log.Info("mesh: a device joined the pool as a guest", "login", id.LoginName, "node", id.ID, "device", g.Device, "trust", g.Trust,
		"repos", strings.Join(g.Repos, ","))
	n.pushGuests(r.Context())
	n.markStale()
	writeJSON(w, http.StatusOK, joinResponse{Pool: n.pool(), Device: n.o.Device, Trust: g.Trust, Repos: nonNilStrings(g.Repos)})
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max]
	}
	return s
}

// handleLeave: a guest drops out of the pool (tentmaker leave on it).
func (n *Node) handleLeave(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(callerKey{}).(Identity)
	if _, err := n.revoke(func(g *Guest) bool { return g.Node == id.ID }, "left"); err != nil && !errors.Is(err, ErrNoGuest) {
		writeErr(w, http.StatusInternalServerError, "the leave could not be saved")
		return
	}
	n.pushGuests(r.Context())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- revoke

// ErrNoGuest is RevokeGuest's answer for a name no active guest has.
var ErrNoGuest = errors.New("no lent device has that name")

// RevokeGuest removes the active guests named name (the device name they
// announce, or their stable node ID) at once: from now on the pool hands
// them no new work, and the owner's other devices learn it at once (or on
// their next discovery if they can't be reached). Jobs already running on
// them finish or end like any device's under the drain rules.
func (n *Node) RevokeGuest(ctx context.Context, name string) ([]Guest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrNoGuest
	}
	gs, err := n.revoke(func(g *Guest) bool {
		return strings.EqualFold(g.Device, name) || g.Node == name
	}, "removed")
	if err != nil {
		return nil, err
	}
	n.pushGuests(ctx)
	return gs, nil
}

func (n *Node) revoke(match func(*Guest) bool, why string) ([]Guest, error) {
	now := n.now()
	n.guests.mu.Lock()
	defer n.guests.mu.Unlock()
	var hit []*Guest
	for _, g := range n.guests.guests {
		if g.active() && match(g) {
			hit = append(hit, g)
		}
	}
	if len(hit) == 0 {
		return nil, ErrNoGuest
	}
	var out []Guest
	for _, g := range hit {
		g.Revoked, g.Updated = now, now
		out = append(out, *g)
	}
	if err := n.saveGuestsLocked(); err != nil {
		for _, g := range hit {
			g.Revoked = time.Time{}
		}
		return nil, fmt.Errorf("can't save the guest table: %w", err)
	}
	for _, g := range out {
		n.log.Info("mesh: a guest was "+why, "login", g.Login, "node", g.Node, "device", g.Device)
	}
	return out, nil
}

// ---------------------------------------------------------------- sharing the guest table

type guestsMsg struct {
	Guests []Guest `json:"guests"`
}

// guestsRev names the guest table's content (not the device names, which
// each device learns on its own), so peers fetch it only when it changed.
func (n *Node) guestsRev() string {
	n.guests.mu.Lock()
	defer n.guests.mu.Unlock()
	if len(n.guests.guests) == 0 {
		return ""
	}
	h := sha256.New()
	for _, id := range sortedGuestIDs(n.guests.guests) {
		g := *n.guests.guests[id]
		g.Device = ""
		b, _ := json.Marshal(g)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (n *Node) guestList(all bool) []Guest {
	n.guests.mu.Lock()
	defer n.guests.mu.Unlock()
	var out []Guest
	for _, id := range sortedGuestIDs(n.guests.guests) {
		g := n.guests.guests[id]
		if all || g.active() {
			c := *g
			c.Repos = append([]string(nil), g.Repos...)
			out = append(out, c)
		}
	}
	return out
}

// mergeGuests takes the newer record of each guest (a revocation wins a
// tie) and saves the table when it changed.
func (n *Node) mergeGuests(in []Guest) {
	n.guests.mu.Lock()
	defer n.guests.mu.Unlock()
	changed := false
	for i := range in {
		g := in[i]
		if !validGuest(&g) {
			continue
		}
		cur := n.guests.guests[g.Node]
		newer := cur == nil || g.Updated.After(cur.Updated) ||
			(g.Updated.Equal(cur.Updated) && !g.active() && cur.active())
		if !newer {
			continue
		}
		if cur == nil && len(n.guests.guests) >= maxGuests {
			continue
		}
		if cur != nil && g.Device == "" {
			g.Device = cur.Device
		}
		g.Repos = normRepos(g.Repos)
		n.guests.guests[g.Node] = &g
		changed = true
	}
	if changed {
		if err := n.saveGuestsLocked(); err != nil {
			n.log.Warn("mesh: can't save the guest table", "err", err)
		}
	}
}

// handleGuests serves the guest table to the owner's own devices (GET) and
// takes theirs (POST).
func (n *Node) handleGuests(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, guestsMsg{Guests: n.guestList(true)})
	case http.MethodPost:
		var m guestsMsg
		if !readJSON(w, r, &m) {
			return
		}
		n.mergeGuests(m.Guests)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// pushGuests sends the guest table to the owner's other devices, so a
// join or a removal takes effect pool-wide at once. A device it can't reach
// fetches the table on its next discovery.
func (n *Node) pushGuests(ctx context.Context) {
	// Know the owner's devices first: a device that has not run discovery
	// yet would otherwise tell none of them.
	n.ensureFresh(ctx)
	msg := guestsMsg{Guests: n.guestList(true)}
	var wg sync.WaitGroup
	for _, p := range n.allPeers() {
		if !p.same || p.skipped {
			continue
		}
		wg.Add(1)
		go func(p peerRef) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), n.callTimeout)
			defer cancel()
			if err := n.call(cctx, http.MethodPost, p.base+"/v1/guests", msg, &struct{}{}); err != nil {
				n.log.Debug("mesh: guest table not sent", "device", p.hello.Device, "err", err)
			}
		}(p)
	}
	wg.Wait()
}

// pullGuests fetches the guest table of each of the owner's devices whose
// hello names a table other than this one's.
func (n *Node) pullGuests(ctx context.Context, peers []peerRef) {
	for _, p := range peers {
		if !p.same || p.hello.Guests == "" || p.hello.Guests == n.guestsRev() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, n.callTimeout)
		var m guestsMsg
		err := n.call(cctx, http.MethodGet, p.base+"/v1/guests", nil, &m)
		cancel()
		if err != nil {
			n.log.Debug("mesh: can't read a device's guest table", "device", p.hello.Device, "err", err)
			continue
		}
		n.mergeGuests(m.Guests)
	}
}

// noteGuestName records the name a guest announces, for people.
func (n *Node) noteGuestName(id, device string) {
	n.guests.mu.Lock()
	defer n.guests.mu.Unlock()
	g := n.guests.guests[id]
	if g == nil || g.Device == device {
		return
	}
	g.Device = clip(device, 128)
	if err := n.saveGuestsLocked(); err != nil {
		n.log.Debug("mesh: can't save the guest table", "err", err)
	}
}

// markStale makes the next Brokers, Claim or Pool call run discovery.
func (n *Node) markStale() {
	n.discMu.Lock()
	n.discAt = time.Time{}
	n.discMu.Unlock()
}

// ---------------------------------------------------------------- join (guest side)

// Join redeems a join code from another tailnet user's device: this device
// becomes a lent device in that device's pool, with the trust the invite
// names. The inviting device's identity (and so the pool owner's user) is
// this device's own WhoIs answer for the address in the code. The code is
// sent only in the body of one request to that address; no error names it.
func (n *Node) Join(ctx context.Context, code string) (Lent, error) {
	sec, host, err := parseCode(code)
	if err != nil {
		return Lent{}, err
	}
	if l := n.lentInfo(); l != nil {
		return Lent{}, fmt.Errorf("this device is lent to pool %q already; run tentmaker leave first", l.Pool)
	}
	sctx, cancel := context.WithTimeout(ctx, whoisTimeout)
	self, err := n.selfPeer(sctx, false)
	cancel()
	if err != nil {
		return Lent{}, fmt.Errorf("tailscale is not up: %w", err)
	}
	if fromSelf(self, host.Addr().String()) != "" {
		return Lent{}, errors.New("that code is from this device itself")
	}
	wctx, cancel := context.WithTimeout(ctx, whoisTimeout)
	who, err := n.o.Tailscale.WhoIs(wctx, host.Addr().String())
	cancel()
	switch {
	case err != nil:
		return Lent{}, fmt.Errorf("can't find the inviting device on the tailnet: %w", err)
	case who.ID == "" || who.UserID == 0 || isTagged(who.LoginName, who.Tags):
		return Lent{}, errors.New("the inviting device has no owner on the tailnet")
	case sameUser(self, who.UserID, who.LoginName, who.Tags):
		return Lent{}, errors.New("that code is from one of your own devices; set mesh.pool to the same name instead")
	}
	jctx, cancel := context.WithTimeout(ctx, ioTimeout)
	var resp joinResponse
	err = n.call(jctx, http.MethodPost, n.joinURL(host)+"/v1/join", joinRequest{Code: sec, Device: n.o.Device}, &resp)
	cancel()
	if err != nil {
		return Lent{}, fmt.Errorf("the inviting device refused: %w", err)
	}
	l := Lent{Pool: resp.Pool, Host: clip(resp.Device, 128), HostNode: who.ID, HostAddr: host.String(),
		OwnerUserID: who.UserID, OwnerLogin: who.LoginName, Trust: resp.Trust, Repos: normRepos(resp.Repos), Joined: n.now()}
	if !validLent(&l) {
		return Lent{}, errors.New("the inviting device sent an answer this version can't use")
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return Lent{}, err
	}
	if p := n.lentPath(); p != "" {
		if err := config.WriteAtomic(p, append(b, '\n'), 0o600); err != nil {
			return Lent{}, fmt.Errorf("joined, but can't save it here, so this device stays out of the pool: %w", err)
		}
	}
	n.lentMu.Lock()
	n.lent = &l
	n.lentMu.Unlock()
	n.peersMu.Lock()
	n.peers = map[string]*peerState{}
	n.peersMu.Unlock()
	n.markStale()
	n.log.Info("mesh: this device joined a pool as a lent device", "pool", l.Pool, "host", l.Host, "owner", l.OwnerLogin, "trust", l.Trust)
	return l, nil
}

// joinURL is the base URL of the peer API at host.
func (n *Node) joinURL(host netip.AddrPort) string {
	if n.hostURL != nil {
		return n.hostURL(host)
	}
	return "http://" + host.String()
}

// Leave takes this device out of the pool it joined: it tells the host
// (best effort, so a gone host doesn't keep it) and returns to its own
// pool.
func (n *Node) Leave(ctx context.Context) (Lent, error) {
	l := n.lentInfo()
	if l == nil {
		return Lent{}, errors.New("this device is not lent to any pool")
	}
	if ap, err := netip.ParseAddrPort(l.HostAddr); err == nil {
		cctx, cancel := context.WithTimeout(ctx, n.callTimeout)
		err := n.call(cctx, http.MethodPost, n.joinURL(ap)+"/v1/leave", struct{}{}, &struct{}{})
		cancel()
		if err != nil {
			n.log.Warn("mesh: the host did not hear that this device left; it stops getting work anyway", "host", l.Host, "err", err)
		}
	}
	if p := n.lentPath(); p != "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return Lent{}, fmt.Errorf("can't remove %s: %w", p, err)
		}
	}
	n.lentMu.Lock()
	n.lent = nil
	n.lentMu.Unlock()
	n.peersMu.Lock()
	n.peers = map[string]*peerState{}
	n.peersMu.Unlock()
	n.markStale()
	n.log.Info("mesh: this device left the pool it was lent to", "pool", l.Pool, "host", l.Host)
	return *l, nil
}

// ---------------------------------------------------------------- views

// PoolDevice is one device in `tentmaker devices`.
type PoolDevice struct {
	Device   string     `json:"device"`
	Node     string     `json:"node,omitempty"`
	Owner    string     `json:"owner"` // tailnet login
	Trust    string     `json:"trust"`
	Repos    []string   `json:"repos"`
	State    string     `json:"state"` // online or asleep
	Lent     bool       `json:"lent"`
	Self     bool       `json:"self,omitempty"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// LendStatus is the lending part of status --json.
type LendStatus struct {
	// Lent is set when this device is lent to another user's pool.
	Lent *Lent `json:"lent,omitempty"`
	// Guests are the lent devices in this device's pool.
	Guests []Guest `json:"guests,omitempty"`
}

// LendStatus reports this device's lent state and the pool's guests.
func (n *Node) LendStatus() LendStatus {
	return LendStatus{Lent: n.lentInfo(), Guests: n.guestList(false)}
}

// Pool name of this device now (the joined pool while lent).
func (n *Node) PoolName() string { return n.pool() }

// Devices lists every pool device this device knows: itself, the owner's
// devices, and lent devices, each online (answered in the last 75 s) or
// asleep.
func (n *Node) Devices(ctx context.Context) []PoolDevice {
	n.ensureFresh(ctx)
	now := n.now()
	sctx, cancel := context.WithTimeout(ctx, whoisTimeout)
	self, _ := n.selfPeer(sctx, false)
	cancel()
	lent := n.lentInfo()
	me := PoolDevice{Device: n.o.Device, Node: self.ID, Owner: self.LoginName, Trust: core.TrustOwner, Repos: []string{},
		State: "online", Self: true}
	if lent != nil {
		me.Trust, me.Repos, me.Lent = lent.Trust, nonNilStrings(lent.Repos), true
	}
	out := []PoolDevice{me}
	online := map[string]peerRef{}
	for _, p := range n.allPeers() {
		if !p.skipped {
			online[p.id] = p
		}
	}
	seen := map[string]bool{self.ID: true}
	add := func(d PoolDevice, id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		d.Node = id
		if d.Repos == nil {
			d.Repos = []string{}
		}
		if p, ok := online[id]; ok {
			d.State = "online"
			if d.Device == "" {
				d.Device = p.hello.Device
			}
		} else if k, ok := n.knownByID(id); ok {
			t := k.LastSeen
			d.LastSeen = &t
			d.State = "asleep"
			if awake(k, now) {
				d.State = "online"
			}
			if d.Device == "" {
				d.Device = k.Device
			}
		} else {
			d.State = "asleep"
		}
		out = append(out, d)
	}
	if lent != nil {
		for _, p := range n.allPeers() {
			if p.kind == kindHost {
				add(PoolDevice{Device: p.hello.Device, Owner: lent.OwnerLogin, Trust: core.TrustOwner}, p.id)
			}
		}
	} else {
		for _, g := range n.guestList(false) {
			add(PoolDevice{Device: g.Device, Owner: g.Login, Trust: g.Trust, Repos: g.Repos, Lent: true}, g.Node)
		}
		for _, p := range n.allPeers() {
			if p.kind == kindOwner {
				add(PoolDevice{Device: p.hello.Device, Owner: p.login, Trust: core.TrustOwner}, p.id)
			}
		}
		for _, k := range n.knownList() {
			if k.Kind == kindOwner {
				add(PoolDevice{Device: k.Device, Owner: k.Login, Trust: core.TrustOwner}, k.ID)
			}
		}
	}
	sort.SliceStable(out[1:], func(i, j int) bool {
		a, b := out[1+i], out[1+j]
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.Node < b.Node
	})
	return out
}

// knownByID is the known device with stable node ID id.
func (n *Node) knownByID(id string) (knownDevice, bool) {
	n.known.mu.Lock()
	defer n.known.mu.Unlock()
	d := n.known.devs[id]
	if d == nil {
		return knownDevice{}, false
	}
	return *d, true
}

// lendSource is a remote broker's view of this device's lend: the trust it
// accepted at join when the broker is the pool owner's device.
func (n *Node) lendFor(kind string) *core.Lend {
	if kind != kindHost {
		return nil
	}
	l := n.lentInfo()
	if l == nil {
		return &core.Lend{Trust: core.TrustRevoked}
	}
	lend := l.lend()
	return &lend
}
