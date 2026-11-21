//go:build windows

package secrets

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestDPAPIRoundTrip(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("the protected DACL grants only SYSTEM and Administrators; run elevated")
	}
	s := Dir(filepath.Join(t.TempDir(), "secrets"))
	if err := s.Set("github", []byte("tm-test-value")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("github")
	if err != nil || string(got) != "tm-test-value" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete("github"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
