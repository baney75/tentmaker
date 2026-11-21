// Package mesh is Tentmaker's tailnet peer API. Devices on one tailnet that
// share a pool name find each other, hand out work from brokers to workers,
// and coordinate job claims. Tailnet identity is the only authentication:
// every request's source address goes through Tailscale's WhoIs, and only
// the same tailnet user (or a configured member) is served.
package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TaggedLogin is the login name Tailscale reports for devices owned by ACL
// tags instead of a person. Tagged devices never count as "the same user";
// they are served only when Members lists this name.
const TaggedLogin = "tagged-devices"

const (
	cliTimeout  = 5 * time.Second
	whoisTTL    = 60 * time.Second
	whoisMaxLen = 1024
)

// Peer is one device on the tailnet, as `tailscale status` reports it.
type Peer struct {
	ID        string       `json:"id"`        // stable node ID
	HostName  string       `json:"host_name"` // the OS hostname
	DNSName   string       `json:"dns_name"`  // MagicDNS FQDN without the trailing dot
	IPs       []netip.Addr `json:"ips"`
	Online    bool         `json:"online"`
	OS        string       `json:"os"` // "linux", "macOS", "windows", ...
	UserID    int64        `json:"user_id"`
	LoginName string       `json:"login_name"`
	Tags      []string     `json:"tags,omitempty"`
}

// Identity is who is behind a tailnet address, as `tailscale whois` reports it.
type Identity struct {
	ID        string       `json:"id"`   // stable node ID, the same as Peer.ID
	Node      string       `json:"node"` // MagicDNS FQDN without the trailing dot
	HostName  string       `json:"host_name"`
	IPs       []netip.Addr `json:"ips"`
	UserID    int64        `json:"user_id"`
	LoginName string       `json:"login_name"`
	Tags      []string     `json:"tags,omitempty"`
}

// Tailscale is what the mesh needs from the local tailscaled.
type Tailscale interface {
	// Self is this device. It fails when tailscale is not running.
	Self(ctx context.Context) (Peer, error)
	// Peers lists the other devices this one can see.
	Peers(ctx context.Context) ([]Peer, error)
	// WhoIs identifies the device and user behind remoteAddr ("ip:port" or "ip").
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
}

// Runner runs a command and returns its standard output. Errors carry a
// short tail of standard error. Tests swap it for a fake.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs a real process.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &tailBuffer{max: 4096, buf: &stderr}
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			err = fmt.Errorf("%s: %w", filepath.Base(name), ctx.Err())
		}
		if msg != "" {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf *bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

// NewTailscaleCLI returns a Tailscale that shells out to the tailscale CLI.
func NewTailscaleCLI() Tailscale {
	return newCLI(ExecRunner, func() (string, error) {
		return findTailscale(runtime.GOOS, exec.LookPath, fileExists, os.Getenv)
	}, time.Now)
}

type tsCLI struct {
	run  Runner
	find func() (string, error)
	now  func() time.Time

	mu    sync.Mutex
	whois map[netip.Addr]whoisEntry
}

type whoisEntry struct {
	id  Identity
	exp time.Time
}

func newCLI(run Runner, find func() (string, error), now func() time.Time) *tsCLI {
	return &tsCLI{run: run, find: find, now: now, whois: map[netip.Addr]whoisEntry{}}
}

func (c *tsCLI) exec(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := c.find()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	out, err := c.run(ctx, bin, args...)
	if err != nil {
		return nil, fmt.Errorf("tailscale %s: %w", args[0], err)
	}
	return out, nil
}

func (c *tsCLI) status(ctx context.Context) (Peer, []Peer, error) {
	out, err := c.exec(ctx, "status", "--json")
	if err != nil {
		return Peer{}, nil, err
	}
	return ParseStatus(out)
}

func (c *tsCLI) Self(ctx context.Context) (Peer, error) {
	self, _, err := c.status(ctx)
	return self, err
}

func (c *tsCLI) Peers(ctx context.Context) ([]Peer, error) {
	_, peers, err := c.status(ctx)
	return peers, err
}

func (c *tsCLI) WhoIs(ctx context.Context, remoteAddr string) (Identity, error) {
	ap, err := parseRemote(remoteAddr)
	if err != nil {
		return Identity{}, err
	}
	now := c.now()
	c.mu.Lock()
	if e, ok := c.whois[ap.Addr()]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.id, nil
	}
	c.mu.Unlock()

	target := ap.Addr().String()
	if ap.Port() != 0 {
		target = ap.String()
	}
	out, err := c.exec(ctx, "whois", "--json", target)
	if err != nil {
		return Identity{}, err
	}
	id, err := ParseWhoIs(out)
	if err != nil {
		return Identity{}, err
	}
	c.mu.Lock()
	if len(c.whois) >= whoisMaxLen {
		for k, e := range c.whois {
			if !now.Before(e.exp) {
				delete(c.whois, k)
			}
		}
		if len(c.whois) >= whoisMaxLen {
			c.whois = map[netip.Addr]whoisEntry{}
		}
	}
	c.whois[ap.Addr()] = whoisEntry{id: id, exp: now.Add(whoisTTL)}
	c.mu.Unlock()
	return id, nil
}

// parseRemote reads "ip:port", "[v6]:port" or a bare IP. Anything else is
// refused, so nothing but an address ever reaches argv.
func parseRemote(s string) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return netip.AddrPortFrom(a.Unmap(), 0), nil
	}
	return netip.AddrPort{}, fmt.Errorf("whois: %q is not an IP address", s)
}

// statusJSON is the part of ipnstate.Status that the mesh reads.
type statusJSON struct {
	BackendState string
	Self         *peerJSON
	Peer         map[string]*peerJSON
	User         map[string]userJSON
}

type peerJSON struct {
	ID           string
	HostName     string
	DNSName      string
	OS           string
	UserID       int64
	TailscaleIPs []string
	Online       bool
	Expired      bool
	Tags         []string
}

type userJSON struct {
	ID          int64
	LoginName   string
	DisplayName string
}

// ParseStatus reads `tailscale status --json`. It fails unless the backend
// is running, since a stopped or logged-out tailscaled has no identity.
func ParseStatus(b []byte) (self Peer, peers []Peer, err error) {
	var st statusJSON
	if err := json.Unmarshal(b, &st); err != nil {
		return Peer{}, nil, fmt.Errorf("tailscale status: %w", err)
	}
	if st.BackendState != "Running" {
		state := st.BackendState
		if state == "" {
			state = "unknown"
		}
		return Peer{}, nil, fmt.Errorf("tailscale is not running (state %s)", state)
	}
	if st.Self == nil {
		return Peer{}, nil, errors.New("tailscale status has no Self")
	}
	self = st.Self.peer(st.User)
	self.Online = true
	if len(self.IPs) == 0 {
		return Peer{}, nil, errors.New("tailscale status: this device has no tailnet IP")
	}
	for _, p := range st.Peer {
		if p == nil {
			continue
		}
		peers = append(peers, p.peer(st.User))
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].HostName != peers[j].HostName {
			return peers[i].HostName < peers[j].HostName
		}
		return peers[i].ID < peers[j].ID
	})
	return self, peers, nil
}

func (p *peerJSON) peer(users map[string]userJSON) Peer {
	out := Peer{
		ID:       p.ID,
		HostName: p.HostName,
		DNSName:  strings.TrimSuffix(p.DNSName, "."),
		Online:   p.Online && !p.Expired,
		OS:       p.OS,
		UserID:   p.UserID,
		Tags:     p.Tags,
		IPs:      parseAddrs(p.TailscaleIPs),
	}
	if u, ok := users[strconv.FormatInt(p.UserID, 10)]; ok {
		out.LoginName = u.LoginName
	}
	return out
}

// whoisJSON is the part of apitype.WhoIsResponse that the mesh reads.
type whoisJSON struct {
	Node *struct {
		ID           int64
		StableID     string
		Name         string
		ComputedName string
		Addresses    []string
		User         int64
		Tags         []string
		Hostinfo     struct {
			Hostname string
			OS       string
		}
	}
	UserProfile *userJSON
}

// ParseWhoIs reads `tailscale whois --json`.
func ParseWhoIs(b []byte) (Identity, error) {
	var w whoisJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return Identity{}, fmt.Errorf("tailscale whois: %w", err)
	}
	if w.Node == nil || w.UserProfile == nil {
		return Identity{}, errors.New("tailscale whois: no node or user in the answer")
	}
	id := Identity{
		ID:        w.Node.StableID,
		Node:      strings.TrimSuffix(w.Node.Name, "."),
		HostName:  w.Node.Hostinfo.Hostname,
		IPs:       parseAddrs(w.Node.Addresses),
		UserID:    w.UserProfile.ID,
		LoginName: w.UserProfile.LoginName,
		Tags:      w.Node.Tags,
	}
	if id.UserID == 0 {
		id.UserID = w.Node.User
	}
	if id.UserID == 0 && id.LoginName == "" {
		return Identity{}, errors.New("tailscale whois: no user in the answer")
	}
	return id, nil
}

// parseAddrs reads IPs or prefixes ("100.64.0.1/32") and drops junk.
func parseAddrs(ss []string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Addr())
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// findTailscale finds the tailscale CLI: PATH first, then where each OS's
// installer puts it (a service's PATH is often minimal).
func findTailscale(goos string, lookPath func(string) (string, error), exists func(string) bool, getenv func(string) string) (string, error) {
	if p, err := lookPath("tailscale"); err == nil {
		return p, nil
	}
	var candidates []string
	switch goos {
	case "darwin":
		candidates = []string{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
			"/usr/local/bin/tailscale",
			"/opt/homebrew/bin/tailscale",
		}
	case "windows":
		if pf := getenv("ProgramFiles"); pf != "" {
			candidates = append(candidates, pf+`\Tailscale\tailscale.exe`)
		}
		candidates = append(candidates, `C:\Program Files\Tailscale\tailscale.exe`)
	default:
		candidates = []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale", "/usr/sbin/tailscale"}
	}
	for _, c := range candidates {
		if exists(c) {
			return c, nil
		}
	}
	return "", errors.New("tailscale CLI not found: install Tailscale from https://tailscale.com/download")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// splitHostIP returns the IP of an "ip:port" address, or "" if it has none.
func splitHostIP(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}
