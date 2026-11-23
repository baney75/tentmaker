package core

import "testing"

func TestSandboxWorkDir(t *testing.T) {
	for dir, want := range map[string]string{"": ".", ".": ".", "src": "src", "src/../lib": "lib", `a\b`: "a/b", "./x/": "x"} {
		if got, err := (SandboxExec{Dir: dir}).WorkDir(); err != nil || got != want {
			t.Errorf("WorkDir(%q) = %q, %v; want %q", dir, got, err, want)
		}
	}
	for _, dir := range []string{"/etc", "/", "..", "../x", "../../..", "a/../../b", `C:\Windows`, `\etc`} {
		if got, err := (SandboxExec{Dir: dir}).WorkDir(); err == nil {
			t.Errorf("WorkDir(%q) = %q, want an error", dir, got)
		}
	}
}
