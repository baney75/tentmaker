package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Caller is who is on the other end of a control API connection. The
// server names it from the connection itself (the socket's peer uid on
// Linux and macOS, the owning process's account on Windows), never from
// anything the caller sends.
type Caller struct {
	Runner bool   // a CI job: the runner user, or on Linux one of its subordinate uids
	Admin  bool   // root, SYSTEM or an elevated Administrator
	Who    string // for logs and errors: "uid 1000", "S-1-5-21-..."
	ID     string // the account: a uid on Unix, a SID on Windows
}

// Identify names the caller on the other end of c. An error refuses every
// request on that connection.
type Identify func(c net.Conn) (Caller, error)

type callerKey struct{}

type callerResult struct {
	c   Caller
	err error
}

// ConnContext names the caller of a new connection and keeps it in the
// connection's context. Serve installs it; a test that serves the API with
// httptest sets it as the test server's Config.ConnContext.
func (s *Server) ConnContext(ctx context.Context, c net.Conn) context.Context {
	if s.ident == nil {
		return ctx
	}
	cl, err := s.ident(c)
	return context.WithValue(ctx, callerKey{}, callerResult{cl, err})
}

// callerOf returns the caller ConnContext stored, or an error when there is
// none (fail closed).
func callerOf(ctx context.Context) (Caller, error) {
	r, ok := ctx.Value(callerKey{}).(callerResult)
	if !ok {
		return Caller{}, errors.New("the control API can't tell who is calling")
	}
	return r.c, r.err
}

// IDRange is a run of uids, First to First+Count-1.
type IDRange struct{ First, Count uint32 }

func (r IDRange) has(id uint32) bool { return id >= r.First && id-r.First < r.Count }

// classifyUID names a Unix caller: root is an admin; the runner user's uid
// and subordinate uids (rootless containers run as those) are CI jobs.
// When the runner's ids can't be read, only root is served.
func classifyUID(uid uint32, runner []IDRange, runnerErr error) (Caller, error) {
	c := Caller{Who: "uid " + strconv.FormatUint(uint64(uid), 10), ID: strconv.FormatUint(uint64(uid), 10)}
	if uid == 0 {
		c.Admin = true
		return c, nil
	}
	if runnerErr != nil {
		return c, fmt.Errorf("can't read the runner user's ids, so only root is served: %w", runnerErr)
	}
	for _, r := range runner {
		if r.has(uid) {
			c.Runner = true
		}
	}
	return c, nil
}

// RunnerIDs finds user's uid in passwd (/etc/passwd) and its subordinate
// uid ranges in subuid (/etc/subuid, which may be empty). No such user
// means no ids: there is no runner to refuse.
func RunnerIDs(user string, passwd, subuid []byte) ([]IDRange, error) {
	if user == "" {
		return nil, nil
	}
	uid, found := "", false
	sc := bufio.NewScanner(bytes.NewReader(passwd))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) >= 3 && f[0] == user {
			uid, found = f[2], true
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read the user list: %w", err)
	}
	if !found {
		return nil, nil
	}
	n, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %s has a malformed uid %q", user, uid)
	}
	out := []IDRange{{First: uint32(n), Count: 1}}
	sc = bufio.NewScanner(bytes.NewReader(subuid))
	for sc.Scan() {
		f := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(f) != 3 || (f[0] != user && f[0] != uid) {
			continue
		}
		first, err1 := strconv.ParseUint(f[1], 10, 32)
		count, err2 := strconv.ParseUint(f[2], 10, 32)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("malformed subordinate uid line for %s: %q", user, sc.Text())
		}
		out = append(out, IDRange{First: uint32(first), Count: uint32(count)})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read the subordinate uids: %w", err)
	}
	return out, nil
}

// classifySID names a Windows caller from its process token: SYSTEM or an
// elevated token is an admin; the runner user's SID is a CI job. When the
// runner's SID can't be looked up, only admins are served.
func classifySID(sid string, elevated bool, runnerSID string, runnerErr error) (Caller, error) {
	c := Caller{Who: sid, ID: sid}
	if sid == "S-1-5-18" || elevated {
		c.Admin = true
		return c, nil
	}
	if runnerErr != nil {
		return c, fmt.Errorf("can't look up the runner user, so only Administrators are served: %w", runnerErr)
	}
	if sid == "" {
		return c, errors.New("the caller's process has no user")
	}
	c.Runner = runnerSID != "" && strings.EqualFold(sid, runnerSID)
	return c, nil
}

// tcpRow is one row of Windows' MIB_TCPTABLE_OWNER_PID.
type tcpRow struct {
	local, remote netip.AddrPort
	pid           uint32
}

// parseTCPTable reads a MIB_TCPTABLE_OWNER_PID: a DWORD count (little
// endian, as on every Windows Tentmaker supports), then rows of six DWORDs:
// state, local address, local port, remote address, remote port, owning
// PID. Addresses are in network byte order, and each port is the first two
// bytes of its DWORD, big endian.
func parseTCPTable(b []byte) ([]tcpRow, error) {
	if len(b) < 4 {
		return nil, errors.New("TCP table is truncated")
	}
	n := binary.LittleEndian.Uint32(b)
	const row = 24
	if uint64(n)*row > uint64(len(b)-4) {
		return nil, fmt.Errorf("TCP table says %d rows but holds %d bytes", n, len(b))
	}
	out := make([]tcpRow, 0, n)
	for i := uint32(0); i < n; i++ {
		r := b[4+i*row:]
		addr := func(off int) netip.AddrPort {
			ip := netip.AddrFrom4([4]byte{r[off], r[off+1], r[off+2], r[off+3]})
			return netip.AddrPortFrom(ip, uint16(r[off+4])<<8|uint16(r[off+5]))
		}
		out = append(out, tcpRow{local: addr(4), remote: addr(12), pid: binary.LittleEndian.Uint32(r[20:])})
	}
	return out, nil
}

// ownerPID finds the process that owns the client end of a connection the
// server accepted: the row whose local end is the client and whose remote
// end is the server.
func ownerPID(rows []tcpRow, client, server netip.AddrPort) (uint32, error) {
	client = netip.AddrPortFrom(client.Addr().Unmap(), client.Port())
	server = netip.AddrPortFrom(server.Addr().Unmap(), server.Port())
	for _, r := range rows {
		if r.local == client && r.remote == server {
			return r.pid, nil
		}
	}
	return 0, fmt.Errorf("no process owns the connection from %s", client)
}

// addrPorts returns a TCP connection's client (remote) and server (local)
// ends.
func addrPorts(c net.Conn) (client, server netip.AddrPort, err error) {
	ra, ok1 := c.RemoteAddr().(*net.TCPAddr)
	la, ok2 := c.LocalAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return client, server, errors.New("not a TCP connection")
	}
	return ra.AddrPort(), la.AddrPort(), nil
}

// procTCPIdentify names a loopback TCP caller from proc's net/tcp and
// net/tcp6 (the daemon's own network namespace): the uid that owns the
// client end of the connection, classified like a Unix socket caller. A
// connection it can't find is refused.
func procTCPIdentify(proc string, runner func() ([]IDRange, error)) Identify {
	return func(c net.Conn) (Caller, error) {
		client, server, err := addrPorts(c)
		if err != nil {
			return Caller{}, err
		}
		if !client.Addr().Unmap().IsLoopback() {
			return Caller{}, errors.New("not a loopback connection")
		}
		for _, f := range []string{"tcp", "tcp6"} {
			table, err := os.ReadFile(filepath.Join(proc, "net", f))
			if err != nil {
				continue
			}
			if uid, ok := tcpOwnerUID(table, client, server, f == "tcp6"); ok {
				ids, err := runner()
				return classifyUID(uid, ids, err)
			}
		}
		return Caller{}, fmt.Errorf("no socket owns the connection from %s", client)
	}
}

// tcpOwnerUID finds, in a /proc/net/tcp (or tcp6 when v6) table, the uid
// that owns the socket whose own end is client and whose peer is server.
func tcpOwnerUID(table []byte, client, server netip.AddrPort, v6 bool) (uint32, bool) {
	la, ra := procAddr(client, v6), procAddr(server, v6)
	if la == "" || ra == "" {
		return 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(table))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// sl local rem st tx:rx tr:when retrnsmt uid timeout inode
		if len(f) < 10 || !strings.EqualFold(f[1], la) || !strings.EqualFold(f[2], ra) {
			continue
		}
		uid, err := strconv.ParseUint(f[7], 10, 32)
		if err != nil {
			return 0, false
		}
		return uint32(uid), true
	}
	return 0, false
}

// procAddr writes ap the way /proc/net/tcp{,6} does: the address as 32-bit
// words in host order (little endian on every Linux Tentmaker supports),
// then the port in hex. An IPv4 address in tcp6 is v4-mapped.
func procAddr(ap netip.AddrPort, v6 bool) string {
	a := ap.Addr()
	var raw []byte
	switch {
	case v6:
		b := a.As16()
		raw = b[:]
	case a.Unmap().Is4():
		b := a.Unmap().As4()
		raw = b[:]
	default:
		return ""
	}
	out := make([]byte, len(raw))
	for i := 0; i+4 <= len(raw); i += 4 {
		binary.LittleEndian.PutUint32(out[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	return strings.ToUpper(hex.EncodeToString(out)) + fmt.Sprintf(":%04X", ap.Port())
}
