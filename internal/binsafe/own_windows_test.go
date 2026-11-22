//go:build windows

package binsafe

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A folder and file an elevated setup creates for the service must be owned
// by Administrators: owned by the elevated user, Windows judges them a
// problem (the scenario workflow's quickstart failed on exactly this).
func TestOwnNewFilesMakesAdministratorsTheOwner(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated process")
	}
	if err := OwnNewFiles(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "svc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "tentmaker.exe")
	if err := os.WriteFile(file, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, dir} {
		e, err := hostLookup(p)
		if err != nil {
			t.Fatal(err)
		}
		if e.Owner != SIDAdministrators {
			t.Errorf("%s is owned by %s, want Administrators", p, e.Owner)
		}
	}
}
