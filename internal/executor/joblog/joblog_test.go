package joblog

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadJoinsAndTails(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "runner/_diag/Worker_20261101.log"), "step a\nStep result: Failed\n")
	write(t, filepath.Join(dir, "runner/_diag/Runner_20261101.log"), "runner up\n")
	write(t, filepath.Join(dir, "runner/_diag/other.txt"), "ignored\n")
	l, err := Read(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(l.Data) != "runner up\nstep a\nStep result: Failed\n" || l.Size != int64(len(l.Data)) {
		t.Fatalf("got %q size %d", l.Data, l.Size)
	}
	l, _ = Read(dir, 10)
	if !strings.HasSuffix(string(l.Data), "Failed\n") || len(l.Data) > 10 {
		t.Fatalf("tail %q", l.Data)
	}
}

func TestReadCheckLogAndMissing(t *testing.T) {
	dir := t.TempDir()
	if l, err := Read(dir, 100); err != nil || len(l.Data) != 0 {
		t.Fatalf("empty folder: %q %v", l.Data, err)
	}
	write(t, filepath.Join(dir, "out/log.txt"), "check output\n")
	if l, _ := Read(dir, 100); string(l.Data) != "check output\n" {
		t.Fatalf("check log %q", l.Data)
	}
	if _, err := Read(filepath.Join(dir, "nope"), 100); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestReadRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "shadow")
	write(t, secret, "root:secret\n")
	os.MkdirAll(filepath.Join(dir, "runner/_diag"), 0o755)
	os.Symlink(secret, filepath.Join(dir, "runner/_diag/Runner_1.log"))
	os.Symlink(filepath.Dir(secret), filepath.Join(dir, "out"))
	l, err := Read(dir, 100)
	if err != nil || strings.Contains(string(l.Data), "secret") {
		t.Fatalf("followed a symlink: %q %v", l.Data, err)
	}
}
