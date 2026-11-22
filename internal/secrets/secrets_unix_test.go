//go:build !windows

package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const value = "tm-test-secret-value-123"

func newStore(t *testing.T) (*fileStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "secrets")
	return Dir(dir).(*fileStore), dir
}

func noLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), value) {
		t.Fatalf("error leaks the secret: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	s, dir := newStore(t)
	if _, err := s.Get("github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing store: want ErrNotFound, got %v", err)
	}
	if err := s.Set("github", []byte(value)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o, want 700", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(dir, "github"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %o, want 600", fi.Mode().Perm())
	}
	got, err := s.Get("github")
	if err != nil || string(got) != value {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Set("github", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("github"); string(got) != "second" {
		t.Fatalf("overwrite: got %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	if err := s.Delete("github"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("github"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.Get("github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestBadNames(t *testing.T) {
	s, _ := newStore(t)
	for _, n := range []string{"../escape", "UP", ""} {
		if err := s.Set(n, []byte("x")); err == nil {
			t.Errorf("Set(%q) accepted", n)
		}
		if _, err := s.Get(n); err == nil {
			t.Errorf("Get(%q) accepted", n)
		}
		if err := s.Delete(n); err == nil {
			t.Errorf("Delete(%q) accepted", n)
		}
	}
}

func TestRefusesGroupOrWorldReadableFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o666} {
		s, dir := newStore(t)
		if err := s.Set("tok", []byte(value)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, "tok"), mode); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get("tok")
		if err == nil || !strings.Contains(err.Error(), "others can access") {
			t.Fatalf("mode %o: want refusal, got %v", mode, err)
		}
		noLeak(t, err)
	}
}

func TestRefusesOpenDirectory(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Set("tok", []byte(value)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("tok"); err == nil {
		t.Fatal("read from a world-readable store")
	}
	if err := s.Set("tok2", []byte(value)); err == nil {
		t.Fatal("wrote into a world-readable store")
	}
}

func TestRefusesOtherOwner(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Set("tok", []byte(value)); err != nil {
		t.Fatal(err)
	}
	s.euid = func() int { return os.Geteuid() + 1 }
	_, err := s.Get("tok")
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("want owner refusal, got %v", err)
	}
	noLeak(t, err)
}

func TestRefusesSymlink(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Set("real", []byte(value)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get("link")
	if err == nil {
		t.Fatal("followed a symlink")
	}
	noLeak(t, err)
}
