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
