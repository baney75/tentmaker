package darwin

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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds a command whose Cmd.Timeout is zero.
const DefaultTimeout = 2 * time.Minute

// MaxOutput caps what Run keeps of stdout when Cmd.MaxOutput is zero.
const MaxOutput = 1 << 20

// Cmd is one command. Secrets and the guest binary travel only in Stdin.
type Cmd struct {
	Path      string
	Args      []string
	Env       []string
	Stdin     []byte
	Timeout   time.Duration // 0: DefaultTimeout; for Spawn, the process's whole lifetime
	MaxOutput int
}

func (c Cmd) String() string { return strings.Join(append([]string{c.Path}, c.Args...), " ") }

// Result is a finished command.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Proc is a long-running process, such as `tart run` or the runner's `tart exec`.
type Proc interface {
	Done() <-chan struct{}
	ExitCode() int // valid once Done is closed
	Kill()
}

// Tools is everything the executor does to the host. Tests swap in a fake.
type Tools interface {
	Run(ctx context.Context, c Cmd) (Result, error)
	// Spawn starts c with stdout and stderr appended to logPath (0600). Its
	// stdin is written and closed. c.Timeout kills it.
	Spawn(c Cmd, logPath string) (Proc, error)
	Fetch(ctx context.Context, url, dst string) error
	Lchown(path string, uid, gid int) error
}

// OSTools is the real Tools.
type OSTools struct{}

// Run implements Tools.
func (OSTools) Run(ctx context.Context, c Cmd) (Result, error) {
	res := Result{Code: -1}
	t := c.Timeout
	if t <= 0 {
		t = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = "/"
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	limit := c.MaxOutput
	if limit <= 0 {
		limit = MaxOutput
	}
	out, errb := &capWriter{n: limit}, &capWriter{n: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, errb
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	res.Stdout, res.Stderr = out.b.String(), errb.b.String()
	if cmd.ProcessState != nil {
		res.Code = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%s: timed out after %s", filepath.Base(c.Path), t)
	}
	if err != nil {
		return res, fmt.Errorf("%s %s: %w: %s", filepath.Base(c.Path), first(c.Args), err, tail(res.Stderr, 300))
	}
	return res, nil
}

type osProc struct {
	cmd    *exec.Cmd
	done   chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	code   int
}

func (p *osProc) Done() <-chan struct{} { return p.done }
func (p *osProc) ExitCode() int         { p.mu.Lock(); defer p.mu.Unlock(); return p.code }
func (p *osProc) Kill()                 { p.cancel() }

// Spawn implements Tools.
func (OSTools) Spawn(c Cmd, logPath string) (Proc, error) {
	if c.Timeout <= 0 {
		return nil, errors.New("spawn needs a lifetime timeout")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close() // the child keeps its own copy
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.WaitDelay = 10 * time.Second
	var stdin io.WriteCloser
	if c.Stdin != nil {
		if stdin, err = cmd.StdinPipe(); err != nil {
			cancel()
			return nil, err
		}
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	p := &osProc{cmd: cmd, done: make(chan struct{}), cancel: cancel, code: -1}
	if stdin != nil {
		go func() {
			stdin.Write(c.Stdin)
			stdin.Close()
		}()
	}
	go func() {
		cmd.Wait()
		p.mu.Lock()
		if cmd.ProcessState != nil {
			p.code = cmd.ProcessState.ExitCode()
		}
		p.mu.Unlock()
		cancel()
		close(p.done)
	}()
	return p, nil
}

// Fetch implements Tools.
func (OSTools) Fetch(ctx context.Context, url, dst string) error {
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

// Lchown implements Tools.
func (OSTools) Lchown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }

type capWriter struct {
	b bytes.Buffer
	n int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.n - w.b.Len(); room > 0 {
		if len(p) > room {
			w.b.Write(p[:room])
		} else {
			w.b.Write(p)
		}
	}
	return len(p), nil
}

func first(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

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

func fetchVerified(ctx context.Context, t Tools, url, dst, want string) error {
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := t.Fetch(ctx, url, dst); err != nil {
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
