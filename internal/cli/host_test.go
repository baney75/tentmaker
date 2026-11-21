package cli

import (
	"path/filepath"
	"testing"
)

func TestWindowsBaseDir(t *testing.T) {
	for _, c := range []struct{ config, want string }{
		{filepath.Join("ProgramData", "Tentmaker", "config.toml"), filepath.Join("ProgramData", "Tentmaker")},
		{filepath.Join("data", "TENTMAKER", "config.toml"), filepath.Join("data", "TENTMAKER")},
		// A custom config path: setup leaves that folder's ACL alone.
		{filepath.Join("Users", "me", "tm.toml"), ""},
		{"config.toml", ""},
	} {
		if got := windowsBaseDir(c.config); got != c.want {
			t.Errorf("windowsBaseDir(%q) = %q, want %q", c.config, got, c.want)
		}
	}
}
