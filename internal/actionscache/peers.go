package actionscache

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcPeers names the job behind a loopback connection from /proc, on
// Linux. A job reaches the host's loopback only through its own pasta
// process, which runs in the job's uplink unit, so the job is the one
// whose uplink cgroup holds the process that owns the connection's other
// end. Nothing the job sends is trusted for this.
type ProcPeers struct {
	// Proc is the proc mount ("/proc"). Its net/tcp must be the host
	// network namespace's: the daemon's own.
	Proc string
	// Cgroup returns the cgroup folder of a job's uplink unit (it holds
	// cgroup.procs), or "" when the job has none.
	Cgroup func(job string) string
}

// JobOf is Options.JobOf. remote is the peer's end of a connection the
// listener accepted, local the listener's.
func (p ProcPeers) JobOf(remote, local netip.AddrPort, jobs []string) (string, bool) {
	if !remote.Addr().IsLoopback() || !local.IsValid() || p.Cgroup == nil {
		return "", false
	}
	inode, ok := p.socketInode(remote, local)
	if !ok {
		return "", false
	}
	want := "socket:[" + inode + "]"
	for _, j := range jobs {
		dir := p.Cgroup(j)
		if dir == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(data)) {
			if _, err := strconv.Atoi(pid); err != nil {
				continue
			}
			if p.holds(pid, want) {
				return j, true
			}
		}
	}
	return "", false
}

// holds says whether process pid has an fd open on the socket.
func (p ProcPeers) holds(pid, want string) bool {
	fdDir := filepath.Join(p.Proc, pid, "fd")
	d, err := os.Open(fdDir)
	if err != nil {
		return false
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	for _, n := range names {
		if l, err := os.Readlink(filepath.Join(fdDir, n)); err == nil && l == want {
			return true
		}
	}
	return false
}

// socketInode finds the inode of the socket whose own end is remote and
// whose peer is local: the connecting side.
func (p ProcPeers) socketInode(remote, local netip.AddrPort) (string, bool) {
	files := []string{"tcp", "tcp6"}
	if remote.Addr().Is6() {
		files = []string{"tcp6"}
	}
	for _, f := range files {
		v6 := f == "tcp6"
		la, ra := procAddr(remote, v6), procAddr(local, v6)
		if la == "" || ra == "" {
			continue
		}
		if ino, ok := scanTCP(filepath.Join(p.Proc, "net", f), la, ra); ok {
			return ino, true
		}
	}
	return "", false
}

func scanTCP(path, local, remote string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	sc.Scan() // header
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 10 {
			continue
		}
		if strings.EqualFold(fs[1], local) && strings.EqualFold(fs[2], remote) && fs[9] != "0" {
			return fs[9], true
		}
	}
	return "", false
}

// procAddr writes ap the way /proc/net/tcp{,6} does: the address as 32-bit
// words in host order (little endian here), then the port in hex.
func procAddr(ap netip.AddrPort, v6 bool) string {
	a := ap.Addr()
	var raw []byte
	switch {
	case v6:
		b := a.As16() // an IPv4 address comes out v4-mapped, as in tcp6
		raw = b[:]
	case a.Is4():
		b := a.As4()
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
