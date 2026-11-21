package secrets

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestNamePattern(t *testing.T) {
	good := []string{"github", "forgejo.codeberg", "a", "gh_token-2", strings.Repeat("a", 64)}
	bad := []string{"", "GitHub", ".hidden", "-x", "a/b", `a\b`, "..", "a b", strings.Repeat("a", 65), "tok\n"}
	for _, n := range good {
		if err := checkName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := checkName(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestErrNotFoundIsNotExist(t *testing.T) {
	if !errors.Is(ErrNotFound, fs.ErrNotExist) {
		t.Fatal("ErrNotFound should match fs.ErrNotExist")
	}
}

func TestDefaultDir(t *testing.T) {
	if DefaultDir() == "" || Default() == nil {
		t.Fatal("no default store")
	}
}
