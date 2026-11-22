//go:build !windows

package control

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultAddr is where the daemon serves the control API: a Unix socket,
// so the server reads each caller's uid from the kernel.
const DefaultAddr = "unix:/var/run/tentmaker/control.sock"

// HostIdentify names a Unix socket caller by its peer uid and marks
// runnerUser's uid and its subordinate uids (from /etc/passwd and
// /etc/subuid) as CI jobs. It reads both files on every connection, since
// setup may create the user after the daemon starts.
func HostIdentify(runnerUser string) Identify {
	return unixIdentify(func() ([]IDRange, error) {
		passwd, err := os.ReadFile("/etc/passwd")
		if err != nil {
			return nil, err
		}
		sub, err := os.ReadFile("/etc/subuid")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return RunnerIDs(runnerUser, passwd, sub)
	})
}

func unixIdentify(runner func() ([]IDRange, error)) Identify {
	return func(c net.Conn) (Caller, error) {
		uid, err := peerUID(c)
		if err != nil {
			return Caller{}, fmt.Errorf("can't read the caller's uid: %w", err)
		}
		ids, err := runner()
		return classifyUID(uid, ids, err)
	}
}

// listenUnix serves on a Unix socket anyone may connect to; the server
// names each caller by its uid. A socket a dead daemon left behind is
// removed; one something still answers on is not.
func listenUnix(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("control socket %q must be an absolute path", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another process serves %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
