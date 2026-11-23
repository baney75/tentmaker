package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// The answer to an exec (and to a copy out) is a stream of frames: one
// kind byte, a 4 byte big endian length, then the payload. 'o' is stdout,
// 'e' is stderr, and 'x' ends the stream with an Exit as JSON. A stream
// that ends without an 'x' frame broke.
const (
	FrameStdout = 'o'
	FrameStderr = 'e'
	FrameExit   = 'x'
	// maxFrame bounds one frame's payload.
	maxFrame = 1 << 20
)

// ContentType names the frame stream.
const ContentType = "application/vnd.tentmaker.frames"

// ExecRequest is the first line of an exec's body; the rest is stdin.
type ExecRequest struct {
	Argv     []string `json:"argv"`
	Dir      string   `json:"dir,omitempty"`
	TimeoutS int64    `json:"timeout_s,omitempty"` // 0: until the sandbox expires
}

// Core turns r into the executor's request.
func (r ExecRequest) Core() core.SandboxExec {
	return core.SandboxExec{Argv: r.Argv, Dir: r.Dir, Timeout: time.Duration(r.TimeoutS) * time.Second}
}

// Exit ends a frame stream.
type Exit struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"` // the command could not run, or broke off
	Fix      string `json:"fix,omitempty"`
}

// FrameWriter writes frames to w, one at a time, flushing after each.
type FrameWriter struct {
	mu    sync.Mutex
	w     io.Writer
	flush func()
	err   error
}

// NewFrameWriter returns a FrameWriter; flush may be nil.
func NewFrameWriter(w io.Writer, flush func()) *FrameWriter {
	return &FrameWriter{w: w, flush: flush}
}

func (f *FrameWriter) frame(kind byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	for first := true; first || len(p) > 0; first = false {
		n := min(len(p), maxFrame)
		var h [5]byte
		h[0] = kind
		binary.BigEndian.PutUint32(h[1:], uint32(n))
		if _, err := f.w.Write(h[:]); err != nil {
			f.err = err
			return err
		}
		if _, err := f.w.Write(p[:n]); err != nil {
			f.err = err
			return err
		}
		p = p[n:]
	}
	if f.flush != nil {
		f.flush()
	}
	return nil
}

// Stream returns a writer whose bytes go out as frames of kind.
func (f *FrameWriter) Stream(kind byte) io.Writer { return streamWriter{f, kind} }

// Exit writes the closing frame.
func (f *FrameWriter) Exit(e Exit) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return f.frame(FrameExit, b)
}

type streamWriter struct {
	f    *FrameWriter
	kind byte
}

func (s streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.f.frame(s.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// ErrBroken means a frame stream ended before its exit frame.
var ErrBroken = errors.New("the connection to the daemon broke before the command finished")

// ReadFrames copies a frame stream's stdout and stderr to the writers and
// returns its exit frame.
func ReadFrames(r io.Reader, stdout, stderr io.Writer) (Exit, error) {
	var h [5]byte
	buf := make([]byte, 32<<10)
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return Exit{}, ErrBroken
		}
		n := int64(binary.BigEndian.Uint32(h[1:]))
		if n > maxFrame {
			return Exit{}, fmt.Errorf("a frame of %d bytes is too long", n)
		}
		switch h[0] {
		case FrameStdout, FrameStderr:
			w := stdout
			if h[0] == FrameStderr {
				w = stderr
			}
			if _, err := io.CopyBuffer(w, &exactReader{r: r, n: n}, buf); err != nil {
				return Exit{}, err
			}
		case FrameExit:
			b := make([]byte, n)
			if _, err := io.ReadFull(r, b); err != nil {
				return Exit{}, ErrBroken
			}
			var e Exit
			if err := json.Unmarshal(b, &e); err != nil {
				return Exit{}, fmt.Errorf("a malformed exit frame: %v", err)
			}
			return e, nil
		default:
			return Exit{}, fmt.Errorf("an unknown frame kind %q", h[0])
		}
	}
}

// exactReader reads exactly n bytes of r, and calls a short read a break.
type exactReader struct {
	r io.Reader
	n int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.n {
		p = p[:e.n]
	}
	n, err := e.r.Read(p)
	e.n -= int64(n)
	if err == io.EOF {
		if e.n > 0 {
			return n, ErrBroken
		}
		err = nil
	}
	return n, err
}
