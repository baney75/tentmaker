package control

import (
	"errors"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DefaultAddr is where the daemon serves the control API. Windows
// Firewall doesn't filter loopback, so the server names every caller by
// the account of the process that owns the connection.
const DefaultAddr = TCPAddr

var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// tcpTableOwnerPIDConnections is TCP_TABLE_OWNER_PID_CONNECTIONS.
const tcpTableOwnerPIDConnections = 4

// HostIdentify names a TCP loopback caller by the account of the process
// that owns the client end (GetExtendedTcpTable, then the process token)
// and marks runnerUser's SID as a CI job.
func HostIdentify(runnerUser string) Identify {
	return func(c net.Conn) (Caller, error) {
		client, server, err := addrPorts(c)
		if err != nil {
			return Caller{}, err
		}
		rows, err := tcpTable()
		if err != nil {
			return Caller{}, err
		}
		pid, err := ownerPID(rows, client, server)
		if err != nil {
			return Caller{}, err
		}
		sid, elevated, err := processUser(pid)
		if err != nil {
			return Caller{}, fmt.Errorf("can't read the account of process %d: %w", pid, err)
		}
		rsid, rerr := lookupSID(runnerUser)
		return classifySID(sid, elevated, rsid, rerr)
	}
}

func tcpTable() ([]tcpRow, error) {
	size := uint32(16 << 10)
	for range 4 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
			windows.AF_INET, tcpTableOwnerPIDConnections, 0)
		switch e := windows.Errno(r); e {
		case 0:
			return parseTCPTable(buf)
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 4 << 10 // connections may open between the two calls
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable: %w", e)
		}
	}
	return nil, errors.New("GetExtendedTcpTable: the table kept growing")
}

func processUser(pid uint32) (sid string, elevated bool, err error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", false, err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return "", false, err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", false, err
	}
	return u.User.Sid.String(), tok.IsElevated(), nil
}

// lookupSID returns user's SID, or "" when there is no such user.
func lookupSID(user string) (string, error) {
	if user == "" {
		return "", nil
	}
	sid, _, _, err := windows.LookupSID("", user)
	if errors.Is(err, windows.ERROR_NONE_MAPPED) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

func listenUnix(string) (net.Listener, error) {
	return nil, errors.New("on Windows the control API listens on TCP loopback, not a Unix socket")
}
