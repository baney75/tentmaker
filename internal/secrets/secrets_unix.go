//go:build !windows

package secrets

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/baney75/tentmaker/internal/config"
)

// DefaultDir is where Default keeps secrets on this OS.
func DefaultDir() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/Tentmaker/secrets"
	}
	return "/etc/tentmaker/secrets"
}

// Dir returns a store that keeps one file per secret in dir. The directory
// is created 0700 on first Set and must be owned by the current effective
// user (root, for the daemon).
func Dir(dir string) Store { return &fileStore{dir: dir, euid: os.Geteuid} }

type fileStore struct {
	dir  string
	euid func() int // a seam for tests
}

// maxSecret caps a read, so a wrong file can't exhaust memory.
const maxSecret = 1 << 20

func (s *fileStore) path(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, name), nil
}

// checkPrivate refuses anything a user other than the owner could read or
// change: group or world permission bits, or another owner.
func (s *fileStore) checkPrivate(what string, fi fs.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to use it", what)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o, which others can access; set it to %04o (chmod)", what, perm, perm&0o700)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: can't read its owner", what)
	}
	if want := s.euid(); int(st.Uid) != want {
		return fmt.Errorf("%s is owned by uid %d, not uid %d; refusing to use it", what, st.Uid, want)
	}
	return nil
}

func (s *fileStore) checkDir() error {
	fi, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("secret store %s: %w", s.dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("secret store %s is not a directory", s.dir)
	}
	return s.checkPrivate("secret store "+s.dir, fi)
}

func (s *fileStore) Get(name string) ([]byte, error) {
	p, err := s.path(name)
	if err != nil {
		return nil, err
	}
	if err := s.checkDir(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("secret %q: can't open %s (a symlink is refused): %w", name, p, unwrapPath(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", name, unwrapPath(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("secret %q: %s is not a regular file", name, p)
	}
	if err := s.checkPrivate("secret file "+p, fi); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecret+1))
	if err != nil {
		return nil, fmt.Errorf("secret %q: read: %w", name, unwrapPath(err))
	}
	if len(b) > maxSecret {
		return nil, fmt.Errorf("secret %q is larger than %d bytes", name, maxSecret)
	}
	return b, nil
}

func (s *fileStore) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("secret store %s: %w", s.dir, unwrapPath(err))
	}
	return s.checkDir()
}

func (s *fileStore) Set(name string, v []byte) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if len(v) > maxSecret {
		return fmt.Errorf("secret %q is larger than %d bytes", name, maxSecret)
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	if err := config.WriteAtomic(p, v, 0o600); err != nil {
		return fmt.Errorf("secret %q: write: %w", name, unwrapPath(err))
	}
	return nil
}

func (s *fileStore) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("secret %q: delete: %w", name, unwrapPath(err))
	}
	return nil
}

// unwrapPath drops the *PathError wrapper (whose text repeats the path) and
// keeps the cause.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
