package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Log rotation from docs/CLI.md: 10 MiB per file, 5 kept.
const (
	LogMaxBytes = 10 << 20
	LogKeep     = 5
)

// RotatingWriter appends to a file and rotates it at MaxBytes, keeping Keep
// old files (path.1 is the newest).
type RotatingWriter struct {
	path string
	max  int64
	keep int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenRotating opens path for appending, creating its directory.
func OpenRotating(path string, max int64, keep int) (*RotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &RotatingWriter{path: path, max: max, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

// Write implements io.Writer.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) rotate() error {
	w.f.Close()
	w.f = nil
	os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep))
	for i := w.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if w.keep > 0 {
		if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		os.Remove(w.path)
	}
	return w.open()
}

// Close closes the file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// NewLogger is the daemon's logger: stderr (journald) when logPath is empty,
// as on Linux, otherwise a rotating file.
func NewLogger(logPath string, stderr io.Writer) (*slog.Logger, io.Closer, error) {
	if logPath == "" {
		return slog.New(slog.NewTextHandler(stderr, nil)), io.NopCloser(nil), nil
	}
	w, err := OpenRotating(logPath, LogMaxBytes, LogKeep)
	if err != nil {
		return slog.New(slog.NewTextHandler(stderr, nil)), io.NopCloser(nil), err
	}
	return slog.New(slog.NewTextHandler(w, nil)), w, nil
}
