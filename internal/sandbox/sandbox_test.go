package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
)

func TestFramesRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf, nil)
	big := bytes.Repeat([]byte("x"), maxFrame+10)
	io.WriteString(fw.Stream(FrameStdout), "out ")
	io.WriteString(fw.Stream(FrameStderr), "err")
	fw.Stream(FrameStdout).Write(big)
	fw.Exit(Exit{ExitCode: 4})
	var out, errs bytes.Buffer
	e, err := ReadFrames(&buf, &out, &errs)
	if err != nil || e.ExitCode != 4 {
		t.Fatalf("exit %+v err %v", e, err)
	}
	if out.Len() != 4+len(big) || errs.String() != "err" {
		t.Fatalf("out %d bytes, err %q", out.Len(), errs.String())
	}
}

func TestFramesBroken(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf, nil)
	io.WriteString(fw.Stream(FrameStdout), "partial output")
	full := buf.Bytes()
	// No exit frame, and a frame cut short: both are a broken stream,
	// never a success.
	for _, b := range [][]byte{full, full[:len(full)-3], nil} {
		if _, err := ReadFrames(bytes.NewReader(b), io.Discard, io.Discard); !errors.Is(err, ErrBroken) {
			t.Fatalf("%d bytes: err %v, want ErrBroken", len(b), err)
		}
	}
	bad := []byte{'z', 0, 0, 0, 0}
	if _, err := ReadFrames(bytes.NewReader(bad), io.Discard, io.Discard); err == nil {
		t.Fatal("an unknown frame kind was accepted")
	}
	huge := []byte{'o', 0xff, 0xff, 0xff, 0xff}
	if _, err := ReadFrames(bytes.NewReader(huge), io.Discard, io.Discard); err == nil {
		t.Fatal("a 4 GiB frame was accepted")
	}
}

// fakeWorker is a Worker with sandboxes in a map.
type fakeWorker struct {
	specs []dispatch.SandboxSpec
	sbx   map[string]dispatch.SandboxInfo
	err   error
	execs []string // the sandbox of each ExecSandbox call
	exec  func(ctx context.Context, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

func (f *fakeWorker) StartSandbox(ctx context.Context, s dispatch.SandboxSpec) (dispatch.SandboxInfo, error) {
	f.specs = append(f.specs, s)
	if f.err != nil {
		return dispatch.SandboxInfo{}, f.err
	}
	p := s.Platform
	p.Arch = "x64"
	i := dispatch.SandboxInfo{Name: s.Name, Platform: p, Limits: s.Limits, Owner: s.Owner, State: "running",
		Expires: time.Now().Add(s.TTL)}
	f.sbx[s.Name] = i
	return i, nil
}
func (f *fakeWorker) ExecSandbox(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.execs = append(f.execs, name)
	if f.exec != nil {
		return f.exec(ctx, req, stdin, stdout, stderr)
	}
	if stdin != nil {
		io.Copy(io.Discard, stdin)
	}
	return 0, nil
}
func (f *fakeWorker) StopSandbox(ctx context.Context, name, why string) error {
	delete(f.sbx, name)
	return nil
}
func (f *fakeWorker) Sandbox(name string) (dispatch.SandboxInfo, error) {
	i, ok := f.sbx[name]
	if !ok {
		return i, dispatch.ErrNoSandbox
	}
	return i, nil
}
func (f *fakeWorker) Sandboxes() []dispatch.SandboxInfo {
	var out []dispatch.SandboxInfo
	for _, i := range f.sbx {
		out = append(out, i)
	}
	return out
}

func status(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestManagerCreate(t *testing.T) {
	fw := &fakeWorker{sbx: map[string]dispatch.SandboxInfo{}}
	m := New(Options{Worker: func() Worker { return fw }, Device: func() string { return "box" },
		MaxTTL: func() time.Duration { return time.Hour }})
	admin := Caller{Admin: true, ID: "0"}
	bg := context.Background()

	info, err := m.Create(bg, admin, CreateRequest{})
	if err != nil || !ValidID(info.ID) || fw.specs[0].TTL != DefaultTTL || fw.specs[0].Platform.OS != "linux" {
		t.Fatalf("info %+v err %v spec %+v", info, err, fw.specs)
	}
	for _, c := range []struct {
		req  CreateRequest
		code int
		want string
	}{
		{CreateRequest{TTLSeconds: 2 * 3600}, http.StatusBadRequest, "longer than this device allows"},
		{CreateRequest{TTLSeconds: 10}, http.StatusBadRequest, "too short"},
		{CreateRequest{OS: "plan9"}, http.StatusBadRequest, "unknown OS"},
		{CreateRequest{Device: "other"}, http.StatusBadRequest, "another device"},
		{CreateRequest{CPUs: -1}, http.StatusBadRequest, "negative"},
	} {
		_, err := m.Create(bg, admin, c.req)
		if status(err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v (status %d)", c.req, err, status(err))
		}
	}
	if len(fw.specs) != 1 {
		t.Fatalf("bad requests reached the worker: %+v", fw.specs)
	}
	fw.err = &dispatch.NoRoomError{Reason: "the budget admits no new work: on battery"}
	_, err = m.Create(bg, admin, CreateRequest{})
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict || !strings.Contains(e.Msg, "on battery") || e.Fix == "" {
		t.Fatalf("no room: %#v", err)
	}
	if _, err := m.Create(bg, Caller{ID: "1000"}, CreateRequest{}); status(err) != http.StatusForbidden {
		t.Fatalf("an unlisted account: %v", err)
	}
	m2 := New(Options{Worker: func() Worker { return nil }})
	if _, err := m2.Create(bg, admin, CreateRequest{}); status(err) != http.StatusServiceUnavailable {
		t.Fatalf("no worker: %v", err)
	}
}

func TestManagerCopyArgv(t *testing.T) {
	in, _ := copyInArgv("linux", "dst dir")
	if strings.Join(in, "|") != `sh|-c|mkdir -p -- "$1" && exec tar -x -f - -C "$1"|sh|dst dir` {
		t.Fatalf("copy in argv %q", in)
	}
	out, _ := copyOutArgv("linux", "/work/a b")
	if strings.Join(out, "|") != "tar|-c|-f|-|-C|/work|a b" {
		t.Fatalf("copy out argv %q", out)
	}
	if _, err := copyInArgv("windows", "x"); status(err) != http.StatusNotImplemented {
		t.Fatalf("windows copy: %v", err)
	}
}

func TestValidIDIsExact(t *testing.T) {
	id, err := NewID()
	if err != nil || !ValidID(id) {
		t.Fatalf("NewID made %q (%v), which ValidID refuses", id, err)
	}
	for _, s := range []string{"sbx-../../v1/pause", "sbx-0123456789AB", "sbx-0123456789abc", "sbx-0123456789a",
		"sbx-", "sbx-0123456789ab/x", "sbx-aaaa-bbbbbbb", "tm-0123456789ab", ""} {
		if ValidID(s) {
			t.Errorf("ValidID(%q) = true", s)
		}
	}
}

// A copy-out path is tar's operand; a name like --help or
// --to-command=sh must never be read as an option.
func TestCopyOutArgvNeverPassesAnOption(t *testing.T) {
	for src, want := range map[string]string{
		"data/--help":                  "./--help",
		"--to-command=sh":              "./--to-command=sh",
		"/work/-rf":                    "./-rf",
		"-dir/-x":                      "./-x",
		"/work/a b":                    "a b",
		"out":                          "out",
		"./--checkpoint-action=exec=x": "./--checkpoint-action=exec=x",
	} {
		clean, err := cleanSandboxPath(src)
		if err != nil {
			t.Fatal(err)
		}
		argv, err := copyOutArgv("linux", clean)
		if err != nil {
			t.Fatal(err)
		}
		if len(argv) != 7 || argv[4] != "-C" || argv[6] != want {
			t.Errorf("%q: argv %q, want the operand %q", src, argv, want)
		}
		if strings.HasPrefix(argv[6], "-") {
			t.Errorf("%q: the operand %q is an option", src, argv[6])
		}
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// tar -c has no size limit; a copy out past MaxCopy is an error, never a
// success, even when tar exits 0.
func TestCopyOutStopsAtMaxCopy(t *testing.T) {
	fw := &fakeWorker{sbx: map[string]dispatch.SandboxInfo{}}
	m := New(Options{Worker: func() Worker { return fw }})
	admin := Caller{Admin: true, ID: "0"}
	bg := context.Background()
	info, err := m.Create(bg, admin, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	var stopped bool
	fw.exec = func(ctx context.Context, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		// Like the executor: copy until a write fails, then report tar's
		// own exit, which is 0 here.
		for i := 0; i < MaxCopy/len(chunk)+8; i++ {
			if _, err := stdout.Write(chunk); err != nil {
				stopped = ctx.Err() != nil
				return 0, nil
			}
		}
		return 0, nil
	}
	var out countWriter
	code, err := m.CopyOut(bg, admin, info.ID, "big", &out, io.Discard)
	if err == nil || status(err) != http.StatusRequestEntityTooLarge || code != -1 {
		t.Fatalf("a copy past MaxCopy: code %d err %v", code, err)
	}
	if out.n > MaxCopy {
		t.Fatalf("%d bytes went out, more than MaxCopy", out.n)
	}
	if !stopped {
		t.Fatal("the command was not told to stop")
	}
	// Exactly MaxCopy is fine.
	fw.exec = func(ctx context.Context, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		for i := 0; i < MaxCopy/len(chunk); i++ {
			if _, err := stdout.Write(chunk); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	out = countWriter{}
	if code, err := m.CopyOut(bg, admin, info.ID, "big", &out, io.Discard); err != nil || code != 0 || out.n != MaxCopy {
		t.Fatalf("a copy of exactly MaxCopy: code %d err %v, %d bytes", code, err, out.n)
	}
}

// The owner checks are the security claim of sandboxes: an account may
// list, read, exec in, copy to or from and remove only its own; an admin
// may act on any.
func TestManagerOwnerChecks(t *testing.T) {
	fw := &fakeWorker{sbx: map[string]dispatch.SandboxInfo{}}
	m := New(Options{Worker: func() Worker { return fw }, Device: func() string { return "box" },
		Users: func() []string { return []string{"1000", "1001"} }})
	alice, bob, admin := Caller{ID: "1000"}, Caller{ID: "1001"}, Caller{ID: "0", Admin: true}
	bg := context.Background()
	info, err := m.Create(bg, alice, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	id := info.ID
	if fw.specs[0].Owner != "1000" {
		t.Fatalf("owner %q, want the creator", fw.specs[0].Owner)
	}
	ops := []struct {
		name string
		do   func(c Caller) error
	}{
		{"get", func(c Caller) error { _, err := m.Get(bg, c, id); return err }},
		{"exec", func(c Caller) error {
			_, err := m.Exec(bg, c, id, core.SandboxExec{Argv: []string{"true"}}, nil, io.Discard, io.Discard)
			return err
		}},
		{"copy in", func(c Caller) error { _, err := m.CopyIn(bg, c, id, "in", strings.NewReader("")); return err }},
		{"copy out", func(c Caller) error { _, err := m.CopyOut(bg, c, id, "out", io.Discard, io.Discard); return err }},
		{"remove", func(c Caller) error { return m.Remove(bg, c, id) }},
	}
	listed := func(c Caller) bool {
		l, err := m.List(bg, c)
		if err != nil {
			t.Fatalf("list as %+v: %v", c, err)
		}
		for _, i := range l {
			if i.ID == id {
				return true
			}
		}
		return false
	}

	// Another listed account: refused everywhere, answered like a missing
	// sandbox, and nothing reaches the worker.
	if listed(bob) {
		t.Error("another account lists the sandbox")
	}
	for _, op := range ops {
		err := op.do(bob)
		if status(err) != http.StatusNotFound || !strings.Contains(err.Error(), "no sandbox "+id) {
			t.Errorf("%s by another account: %v (status %d), want 404", op.name, err, status(err))
		}
	}
	if len(fw.execs) != 0 || len(fw.sbx) != 1 {
		t.Fatalf("another account reached the sandbox: execs %v, sandboxes %d", fw.execs, len(fw.sbx))
	}
	// An account not in users is refused before any lookup.
	for _, op := range ops {
		if err := op.do(Caller{ID: "1002"}); status(err) != http.StatusForbidden {
			t.Errorf("%s by an unlisted account: %v", op.name, err)
		}
	}

	// The owner may do all of it; so may an admin, on a sandbox it doesn't own.
	for _, c := range []Caller{alice, admin} {
		if !listed(c) {
			t.Errorf("%+v doesn't list the sandbox", c)
		}
		for _, op := range ops[:len(ops)-1] {
			if err := op.do(c); err != nil {
				t.Errorf("%s by %+v: %v", op.name, c, err)
			}
		}
	}
	if len(fw.execs) != 6 {
		t.Fatalf("execs %v, want 3 each for the owner and the admin", fw.execs)
	}
	if err := ops[len(ops)-1].do(admin); err != nil || len(fw.sbx) != 0 {
		t.Fatalf("an admin's remove: %v, %d left", err, len(fw.sbx))
	}
	// And the owner's own remove.
	info, _ = m.Create(bg, alice, CreateRequest{})
	id = info.ID
	if err := ops[len(ops)-1].do(alice); err != nil || len(fw.sbx) != 0 {
		t.Fatalf("the owner's remove: %v, %d left", err, len(fw.sbx))
	}
}
