// Package guestbin keeps a sandbox's copy of the tentmaker binary in step
// with the daemon's. Setup copies it once; after an update the copy is
// stale, so executors check it before each check job.
package guestbin

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Place writes data-equal copy of src at dst atomically, with the owner and
// mode the executor wants.
type Place func(src, dst string) error

type stamp struct {
	size  int64
	mtime time.Time
}

func stampOf(fi os.FileInfo) stamp { return stamp{fi.Size(), fi.ModTime()} }

// Syncer remembers which (src, dst) pairs were equal, by size and mtime, so
// most checks hash nothing.
type Syncer struct {
	mu    sync.Mutex
	equal map[string][2]stamp // dst -> {src, dst} stamps when last found equal
}

// Sync makes dst equal to src (size and sha256), calling place when they
// differ. It reports whether it copied.
func (s *Syncer) Sync(src, dst string, place Place) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sfi, err := os.Stat(src)
	if err != nil {
		return false, fmt.Errorf("the tentmaker binary for the sandbox: %w", err)
	}
	if !sfi.Mode().IsRegular() {
		return false, fmt.Errorf("the tentmaker binary for the sandbox (%s) is not a file", src)
	}
	if dfi, err := os.Lstat(dst); err == nil && dfi.Mode().IsRegular() {
		key := [2]stamp{stampOf(sfi), stampOf(dfi)}
		if s.equal[dst+"\x00"+src] == key {
			return false, nil
		}
		if sfi.Size() == dfi.Size() {
			same, err := sameContent(src, dst)
			if err != nil {
				return false, err
			}
			if same {
				s.remember(src, dst, key)
				return false, nil
			}
		}
	}
	if err := place(src, dst); err != nil {
		return false, fmt.Errorf("refresh the sandbox's tentmaker binary: %w", err)
	}
	sfi2, err1 := os.Stat(src)
	dfi2, err2 := os.Lstat(dst)
	if err1 == nil && err2 == nil {
		s.remember(src, dst, [2]stamp{stampOf(sfi2), stampOf(dfi2)})
	}
	return true, nil
}

func (s *Syncer) remember(src, dst string, key [2]stamp) {
	if s.equal == nil {
		s.equal = map[string][2]stamp{}
	}
	s.equal[dst+"\x00"+src] = key
}

func sameContent(a, b string) (bool, error) {
	ha, err := hashFile(a)
	if err != nil {
		return false, err
	}
	hb, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ha, hb), nil
}

func hashFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
