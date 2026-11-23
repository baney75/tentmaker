// Package sandbox gives agents throwaway sandboxes on Tentmaker devices.
//
// A sandbox is a job with no forge: the same executor isolation (on Linux
// a transient unit as the runner user with its own network namespace and
// the job fence), counted against the device budget like a job, with a
// time to live that always ends it. The Manager is the daemon's side: it
// checks who may create and use sandboxes, places them, and runs commands
// and copies in them. The wire format (stream.go) and the client helpers
// for copying files (files.go) live here too, so the CLI and the daemon
// agree on them.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/user"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
)

// Defaults.
const (
	DefaultTTL = 30 * time.Minute
	MinTTL     = time.Minute
	// DefaultMaxTTL is the longest TTL when the config sets none.
	DefaultMaxTTL = 4 * time.Hour
	// Prefix starts every sandbox id.
	Prefix = "sbx-"
)

// CreateRequest asks for one sandbox.
type CreateRequest struct {
	Device      string  `json:"device,omitempty"` // "" or this device's name
	OS          string  `json:"os,omitempty"`     // linux (default), macos, windows
	Arch        string  `json:"arch,omitempty"`   // "" = any
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
	CPUs        float64 `json:"cpus,omitempty"`
	TTLSeconds  int64   `json:"ttl_s,omitempty"` // 0 = DefaultTTL
}

// Info is one sandbox as callers see it.
type Info struct {
	ID          string    `json:"id"`
	Device      string    `json:"device"`
	Platform    string    `json:"platform"` // "linux/x64"
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	MemoryBytes int64     `json:"memory_bytes"`
	CPUs        float64   `json:"cpus"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
	State       string    `json:"state"` // starting, running, stopping
	Executor    string    `json:"executor"`
	Owner       string    `json:"owner,omitempty"`
}

// CopyResult is what a copy into a sandbox moved.
type CopyResult struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// Caller is who asks, as the control API named them from the connection.
type Caller struct {
	ID    string // uid on Unix, SID on Windows
	Admin bool   // root, SYSTEM, an elevated Administrator, or the control token's holder
}

// Error is a refusal with an HTTP status and a fix.
type Error struct {
	Status int
	Msg    string
	Fix    string
}

func (e *Error) Error() string { return e.Msg }

func refuse(status int, fix, format string, a ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, a...), Fix: fix}
}

// Worker is what the Manager needs from dispatch.Worker.
type Worker interface {
	StartSandbox(ctx context.Context, spec dispatch.SandboxSpec) (dispatch.SandboxInfo, error)
	ExecSandbox(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	StopSandbox(ctx context.Context, name, why string) error
	Sandbox(name string) (dispatch.SandboxInfo, error)
	Sandboxes() []dispatch.SandboxInfo
}

// Options configures a Manager. Every func is read on each call, so a
// config reload takes effect at once.
type Options struct {
	Worker func() Worker // nil answer: the worker is not running
	Device func() string
	// Users are local accounts (names, uids or SIDs) that may use
	// sandboxes besides admins: config [sandbox] users.
	Users  func() []string
	MaxTTL func() time.Duration
	// LookupID maps a user name to its uid (Unix) or SID (Windows).
	LookupID func(name string) (string, error)
	Log      *slog.Logger
}

// Manager serves sandboxes on this device.
type Manager struct{ o Options }

// New returns a Manager.
func New(o Options) *Manager {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.LookupID == nil {
		o.LookupID = func(name string) (string, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			return u.Uid, nil
		}
	}
	if o.Users == nil {
		o.Users = func() []string { return nil }
	}
	if o.MaxTTL == nil {
		o.MaxTTL = func() time.Duration { return DefaultMaxTTL }
	}
	if o.Device == nil {
		o.Device = func() string { return "" }
	}
	return &Manager{o: o}
}

const fixAccess = "run it with sudo (an Administrator shell on Windows), or have an admin add your account to users in the [sandbox] section of the config and restart the service"

// allowed says whether c may use sandboxes at all.
func (m *Manager) allowed(c Caller) error {
	if c.Admin {
		return nil
	}
	if c.ID != "" {
		for _, u := range m.o.Users() {
			if u == c.ID {
				return nil
			}
			if id, err := m.o.LookupID(u); err == nil && id == c.ID {
				return nil
			}
		}
	}
	return refuse(http.StatusForbidden, fixAccess, "your account may not use sandboxes on this device")
}

func (m *Manager) worker() (Worker, error) {
	var w Worker
	if m.o.Worker != nil {
		w = m.o.Worker()
	}
	if w == nil {
		return nil, refuse(http.StatusServiceUnavailable, "run tentmaker doctor", "the worker is not running on this device")
	}
	return w, nil
}

// NewID makes a sandbox id: Prefix and 12 random hex digits.
func NewID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return Prefix + hex.EncodeToString(b[:]), nil
}

// idPattern is a sandbox id exactly: Prefix and 12 lowercase hex digits,
// the shape NewID makes and the Linux executor accepts (isSandboxName).
var idPattern = regexp.MustCompile(`^` + Prefix + `[0-9a-f]{12}$`)

// ValidID reports whether s is a sandbox id. Nothing else may go into a
// sandbox URL, a unit name or a log line as one.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// Create places one sandbox on this device and waits until it runs.
func (m *Manager) Create(ctx context.Context, c Caller, req CreateRequest) (Info, error) {
	if err := m.allowed(c); err != nil {
		return Info{}, err
	}
	dev := m.o.Device()
	if req.Device != "" && !strings.EqualFold(req.Device, dev) {
		return Info{}, refuse(http.StatusBadRequest, "run tentmaker sandbox create on "+req.Device+", or leave out --device",
			"placing a sandbox on another device (%s) is not in this version; this device is %s", req.Device, dev)
	}
	os := strings.ToLower(req.OS)
	switch os {
	case "":
		os = "linux"
	case "mac", "darwin":
		os = "macos"
	case "linux", "macos", "windows":
	default:
		return Info{}, refuse(http.StatusBadRequest, "use --os linux, macos or windows", "unknown OS %q", req.OS)
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl == 0 {
		ttl = DefaultTTL
	}
	maxTTL := m.o.MaxTTL()
	if maxTTL <= 0 {
		maxTTL = DefaultMaxTTL
	}
	switch {
	case ttl < MinTTL:
		return Info{}, refuse(http.StatusBadRequest, "use --ttl 1m or more", "a time to live of %s is too short", ttl)
	case ttl > maxTTL:
		return Info{}, refuse(http.StatusBadRequest, fmt.Sprintf("use --ttl %s or less, or raise max_ttl in the [sandbox] section of the config", maxTTL),
			"a time to live of %s is longer than this device allows (%s)", ttl, maxTTL)
	}
	if req.MemoryBytes < 0 || req.CPUs < 0 || req.CPUs != req.CPUs {
		return Info{}, refuse(http.StatusBadRequest, "use --memory like 4GiB and --cpus like 2", "memory and CPUs can't be negative")
	}
	w, err := m.worker()
	if err != nil {
		return Info{}, err
	}
	id, err := NewID()
	if err != nil {
		return Info{}, err
	}
	si, err := w.StartSandbox(ctx, dispatch.SandboxSpec{Name: id, Platform: core.Platform{OS: os, Arch: req.Arch},
		Limits: core.Resources{MemoryBytes: req.MemoryBytes, CPUs: req.CPUs}, TTL: ttl, Owner: c.ID})
	var nr *dispatch.NoRoomError
	if errors.As(err, &nr) {
		return Info{}, refuse(http.StatusConflict,
			"remove a sandbox you no longer need (tentmaker sandbox ls, tentmaker sandbox rm ID), ask for less with --memory and --cpus, or wait for a job to finish",
			"%s has no room for a %s sandbox: %s", dev, os, nr.Reason)
	}
	if err != nil {
		return Info{}, refuse(http.StatusInternalServerError, "run tentmaker doctor; the executor's last error is in tentmaker status", "%v", err)
	}
	m.o.Log.Info("sandbox created", "id", id, "owner", c.ID, "platform", si.Platform.String(), "ttl", ttl.String())
	return infoOf(si), nil
}

func infoOf(s dispatch.SandboxInfo) Info {
	return Info{ID: s.Name, Device: s.Device, Platform: s.Platform.String(), OS: s.Platform.OS, Arch: s.Platform.Arch,
		MemoryBytes: s.Limits.MemoryBytes, CPUs: s.Limits.CPUs, Created: s.Started, Expires: s.Expires, State: s.State,
		Executor: s.Executor, Owner: s.Owner}
}

// get finds a sandbox c may use. Another owner's sandbox is not found, so
// its id says nothing.
func (m *Manager) get(c Caller, id string) (Worker, dispatch.SandboxInfo, error) {
	if err := m.allowed(c); err != nil {
		return nil, dispatch.SandboxInfo{}, err
	}
	notFound := refuse(http.StatusNotFound, "list your sandboxes with tentmaker sandbox ls", "no sandbox %s on %s", id, m.o.Device())
	if !ValidID(id) {
		return nil, dispatch.SandboxInfo{}, notFound
	}
	w, err := m.worker()
	if err != nil {
		return nil, dispatch.SandboxInfo{}, err
	}
	si, err := w.Sandbox(id)
	if err != nil || (!c.Admin && si.Owner != c.ID) {
		return nil, dispatch.SandboxInfo{}, notFound
	}
	return w, si, nil
}

// List returns the sandboxes c may use: all of them for an admin.
func (m *Manager) List(ctx context.Context, c Caller) ([]Info, error) {
	if err := m.allowed(c); err != nil {
		return nil, err
	}
	w, err := m.worker()
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, s := range w.Sandboxes() {
		if c.Admin || s.Owner == c.ID {
			out = append(out, infoOf(s))
		}
	}
	return out, nil
}

// Get returns one sandbox.
func (m *Manager) Get(ctx context.Context, c Caller, id string) (Info, error) {
	_, si, err := m.get(c, id)
	return infoOf(si), err
}

// Remove stops and deletes a sandbox.
func (m *Manager) Remove(ctx context.Context, c Caller, id string) error {
	w, _, err := m.get(c, id)
	if err != nil {
		return err
	}
	if err := w.StopSandbox(ctx, id, "removed"); err != nil {
		return refuse(http.StatusInternalServerError, "Tentmaker retries the stop; check tentmaker status", "%v", err)
	}
	m.o.Log.Info("sandbox removed", "id", id, "by", c.ID)
	return nil
}

// Exec runs one command in a sandbox and returns its exit code.
func (m *Manager) Exec(ctx context.Context, c Caller, id string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	w, si, err := m.get(c, id)
	if err != nil {
		return -1, err
	}
	if si.State != "running" {
		return -1, refuse(http.StatusConflict, "wait a moment and retry, or create a new sandbox", "sandbox %s is %s", id, si.State)
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return -1, refuse(http.StatusBadRequest, "put the command after --, like tentmaker sandbox exec ID -- go test ./...", "no command to run")
	}
	return w.ExecSandbox(ctx, id, req, stdin, stdout, stderr)
}

// copyArgv is the command that writes a tar stream into dst (copy in), or
// writes src as a tar stream (copy out), on a sandbox's OS.
func copyInArgv(os, dst string) ([]string, error) {
	switch os {
	case "linux", "macos":
		return []string{"sh", "-c", `mkdir -p -- "$1" && exec tar -x -f - -C "$1"`, "sh", dst}, nil
	}
	return nil, refuse(http.StatusNotImplemented, "copy with tentmaker sandbox exec and tar", "copying files into a %s sandbox is not in this version", os)
}

func copyOutArgv(os, src string) ([]string, error) {
	switch os {
	case "linux", "macos":
		// The name is tar's one operand; one that starts with a dash
		// (data/--help, -rf) would be read as an option, so it goes as
		// ./NAME, which names the same file. The folder is -C's argument,
		// never an option, whatever it starts with.
		name := path.Base(src)
		if strings.HasPrefix(name, "-") {
			name = "./" + name
		}
		return []string{"tar", "-c", "-f", "-", "-C", path.Dir(src), name}, nil
	}
	return nil, refuse(http.StatusNotImplemented, "copy with tentmaker sandbox exec and tar", "copying files out of a %s sandbox is not in this version", os)
}

// cleanSandboxPath checks a path inside a sandbox: relative to its work
// folder, or absolute; "" is the work folder.
func cleanSandboxPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", refuse(http.StatusBadRequest, "name a path inside the sandbox", "the path holds a NUL byte")
	}
	if p == "" {
		return ".", nil
	}
	return path.Clean(strings.ReplaceAll(p, `\`, "/")), nil
}

// CopyIn unpacks a tar stream into dst, a folder in the sandbox (made if
// missing). tar runs inside the sandbox as its user, so nothing outside the
// sandbox's reach is written.
func (m *Manager) CopyIn(ctx context.Context, c Caller, id, dst string, in io.Reader) (CopyResult, error) {
	w, si, err := m.get(c, id)
	if err != nil {
		return CopyResult{}, err
	}
	if si.State != "running" {
		return CopyResult{}, refuse(http.StatusConflict, "wait a moment and retry", "sandbox %s is %s", id, si.State)
	}
	dst, err = cleanSandboxPath(dst)
	if err != nil {
		return CopyResult{}, err
	}
	argv, err := copyInArgv(si.Platform.OS, dst)
	if err != nil {
		return CopyResult{}, err
	}
	cr := &countReader{r: io.LimitReader(in, MaxCopy+1)}
	var stderr capBuffer
	code, err := w.ExecSandbox(ctx, id, core.SandboxExec{Argv: argv}, cr, io.Discard, &stderr)
	if cr.n > MaxCopy {
		return CopyResult{}, refuse(http.StatusRequestEntityTooLarge, "copy less at a time", "a copy is limited to %s", config.Size(MaxCopy))
	}
	if err != nil {
		return CopyResult{}, refuse(http.StatusInternalServerError, "retry; if it keeps failing, create a new sandbox", "copy into %s: %v", id, err)
	}
	if code != 0 {
		return CopyResult{}, refuse(http.StatusUnprocessableEntity, "check the path; it is relative to the sandbox's work folder",
			"tar exited %d in the sandbox: %s", code, strings.TrimSpace(stderr.String()))
	}
	return CopyResult{ID: id, Path: dst, Bytes: cr.n}, nil
}

// CopyOut writes src from the sandbox as a tar stream to out, and its
// errors to stderr, and returns tar's exit code.
func (m *Manager) CopyOut(ctx context.Context, c Caller, id, src string, out, stderr io.Writer) (int, error) {
	w, si, err := m.get(c, id)
	if err != nil {
		return -1, err
	}
	if si.State != "running" {
		return -1, refuse(http.StatusConflict, "wait a moment and retry", "sandbox %s is %s", id, si.State)
	}
	src, err = cleanSandboxPath(src)
	if err != nil {
		return -1, err
	}
	argv, err := copyOutArgv(si.Platform.OS, src)
	if err != nil {
		return -1, err
	}
	return w.ExecSandbox(ctx, id, core.SandboxExec{Argv: argv}, nil, out, stderr)
}

// MaxCopy bounds one copy either way.
const MaxCopy = 2 << 30

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// capBuffer keeps the first 64 KiB written to it.
type capBuffer struct{ b bytes.Buffer }

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := 64<<10 - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

func (c *capBuffer) String() string { return c.b.String() }
