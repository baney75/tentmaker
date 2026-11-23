package linux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a command whose Cmd.Timeout is zero.
const DefaultTimeout = 2 * time.Minute

// Cmd is one command the executor runs on the host. Secrets travel only in
// Stdin, never in Path, Args or Env.
type Cmd struct {
	Path    string   // a bare name is looked up in a fixed system PATH, never the daemon's
	Args    []string // without the program name
	Env     []string // nil: a fixed minimal environment
	Stdin   []byte
	Timeout time.Duration // 0: DefaultTimeout
}

func (c Cmd) String() string { return strings.Join(append([]string{c.Path}, c.Args...), " ") }

// Result is a finished command. Code is the exit code (-1 if it never ran).
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Host is every privileged thing the executor does to the machine besides
// plain file writes under the paths in Options. Tests swap in a fake that
// records each call.
type Host interface {
	// Run runs c and waits. A non-zero exit is an error, with Result filled.
	Run(ctx context.Context, c Cmd) (Result, error)
	// LookupUser returns a local user's uid and primary gid.
	LookupUser(name string) (uid, gid int, err error)
	// Lchown changes a path's owner without following a symlink.
	Lchown(path string, uid, gid int) error
	// Fetch downloads url to dst (a new file), within the context's deadline.
	Fetch(ctx context.Context, url, dst string) error
}

// SystemPath is where bare command names are found. The daemon's own PATH is
// never used, so nothing in its environment can swap a tool.
var SystemPath = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

var baseEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}

// maxOutput caps what Run keeps of a command's stdout and stderr.
const maxOutput = 1 << 20

// OSHost is the real Host.
type OSHost struct{}

func lookPath(name string) (string, error) {
	if strings.ContainsRune(name, '/') {
		return name, nil
	}
	for _, d := range SystemPath {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", name, strings.Join(SystemPath, ":"))
}

// Run implements Host.
func (OSHost) Run(ctx context.Context, c Cmd) (Result, error) {
	res := Result{Code: -1}
	path, err := lookPath(c.Path)
	if err != nil {
		return res, err
	}
	t := c.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = baseEnv
	}
	cmd.Dir = "/"
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var out, errb capWriter
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	res.Stdout, res.Stderr = out.String(), errb.String()
	if cmd.ProcessState != nil {
		res.Code = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%s: timed out after %s", filepath.Base(path), t)
	}
	if err != nil {
		return res, fmt.Errorf("%s: %w: %s", filepath.Base(path), err, tail(res.Stderr, 300))
	}
	return res, nil
}

// Stream implements Streamer.
func (OSHost) Stream(ctx context.Context, c Cmd, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	path, err := lookPath(c.Path)
	if err != nil {
		return -1, err
	}
	t := c.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = baseEnv
	}
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if stdin != nil {
		// Copy stdin ourselves: exec's own copy would keep Wait waiting on a
		// stdin (a debug shell's connection) that never ends after the
		// process exits. Wait closes w, which ends this copy's next write.
		w, err := cmd.StdinPipe()
		if err != nil {
			return -1, err
		}
		go func() { io.Copy(w, stdin); w.Close() }()
	}
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return code, fmt.Errorf("%s: timed out after %s", filepath.Base(path), t)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return code, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return code, nil
}

var _ Streamer = OSHost{}

// LookupUser implements Host.
func (OSHost) LookupUser(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("user %s has a non-numeric uid or gid", name)
	}
	return uid, gid, nil
}

// Lchown implements Host.
func (OSHost) Lchown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }

// Fetch implements Host.
func (OSHost) Fetch(ctx context.Context, url, dst string) error { return fetch(ctx, url, dst) }

func fetch(ctx context.Context, url, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(part)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dst)
}

// capWriter keeps the first maxOutput bytes written to it.
type capWriter struct{ b bytes.Buffer }

func (w *capWriter) Write(p []byte) (int, error) {
	if room := maxOutput - w.b.Len(); room > 0 {
		if len(p) > room {
			w.b.Write(p[:room])
		} else {
			w.b.Write(p)
		}
	}
	return len(p), nil
}
func (w *capWriter) String() string { return w.b.String() }

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// fileSHA256 returns the hex sha256 of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchVerified downloads url to dst unless dst already exists, then checks
// its sha256. A mismatch deletes the file.
func fetchVerified(ctx context.Context, h Host, url, dst, want string) error {
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := h.Fetch(ctx, url, dst); err != nil {
			return err
		}
	}
	got, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if got != want {
		os.Remove(dst)
		return fmt.Errorf("%s: sha256 %s, want %s; deleted it", filepath.Base(dst), got, want)
	}
	return nil
}
