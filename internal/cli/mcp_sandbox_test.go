package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
)

// The controller refuses a timeout the tool should have refused, so a
// fraction can never truncate to 0 (no limit) on the way to the daemon.
func TestMCPSandboxExecRefusesFractionalTimeout(t *testing.T) {
	for _, bad := range []float64{0.5, -1, 1e300} {
		_, err := controller{}.SandboxExec(context.Background(), "sbx-0123456789ab", []string{"true"}, "", bad, "")
		if err == nil || !strings.Contains(err.Error(), "whole number") {
			t.Errorf("timeout %g: %v", bad, err)
		}
	}
}

// sandbox_exec returned up to 1 MiB of each stream, more than fits a
// model's context. It keeps the last 64 KiB and says how much it dropped.
func TestMCPSandboxExecKeepsTheTail(t *testing.T) {
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	call := mcpSession(t, f)
	res, ok := call("sandbox_create", map[string]any{})
	if !ok {
		t.Fatalf("create: %s", mustJSON(res))
	}
	id := res["structuredContent"].(map[string]any)["id"].(string)
	const total = 300 << 10
	f.ex.SetExec(func(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		// Many small writes, as a chatty build makes, then the error last.
		line := strings.Repeat("x", 1023) + "\n"
		for i := 0; i < total/len(line)-1; i++ {
			io.WriteString(stdout, line)
		}
		io.WriteString(stdout, strings.Repeat("y", 1023)+"E")
		io.WriteString(stderr, "short")
		return 1, nil
	})
	res, ok = call("sandbox_exec", map[string]any{"id": id, "command": []any{"make"}})
	if !ok {
		t.Fatalf("exec: %s", mustJSON(res))
	}
	v := res["structuredContent"].(map[string]any)
	out, _ := v["stdout"].(string)
	if len(out) != mcpMaxOutput || !strings.HasSuffix(out, "E") {
		t.Fatalf("stdout is %d bytes ending %q, want the last %d", len(out), out[max(0, len(out)-8):], mcpMaxOutput)
	}
	if v["stdout_cut_bytes"] != float64(total-mcpMaxOutput) || v["stderr"] != "short" || v["stderr_cut_bytes"] != nil {
		t.Fatalf("cut %v, stderr %v %v", v["stdout_cut_bytes"], v["stderr"], v["stderr_cut_bytes"])
	}
	if v["exit_code"] != 1.0 || !strings.Contains(v["note"].(string), "64 KiB") {
		t.Fatalf("exit %v note %v", v["exit_code"], v["note"])
	}
	if b, _ := json.Marshal(res); len(b) > 2*mcpMaxOutput+16<<10 {
		t.Fatalf("the whole answer is %d bytes", len(b))
	}
}

func TestTailBufferKeepsWholeRunes(t *testing.T) {
	tb := &tailBuffer{max: 4}
	io.WriteString(tb, "aé") // é is 2 bytes; the last 4 of "aébcd" start in it
	io.WriteString(tb, "bcd")
	if tb.String() != "bcd" || tb.cut() != 3 {
		t.Fatalf("tail %q cut %d", tb.String(), tb.cut())
	}
}

// The client stops a copy out at MaxCopy too, whatever the daemon sends.
func TestCopyLimitFailsPastMaxCopy(t *testing.T) {
	var got strings.Builder
	cl := &copyLimit{w: &got, left: 5}
	if n, err := cl.Write([]byte("abcd")); n != 4 || err != nil {
		t.Fatalf("under the limit: %d %v", n, err)
	}
	if _, err := cl.Write([]byte("ef")); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("past the limit: %v", err)
	}
	if got.String() != "abcd" {
		t.Fatalf("passed %q", got.String())
	}
}

// sandbox_cp out of a sandbox writes only under the server's --root, with
// symlinks resolved, and never replaces a file outside it: an agent
// steered by text in a sandbox must not overwrite host files, least of all
// as root.
func TestMCPSandboxCopyOutStaysUnderRoot(t *testing.T) {
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(victim, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// root/link points outside; root/trap/inbox/notes.txt is a symlink to
	// the victim.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "trap", "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "trap", "inbox", "notes.txt")); err != nil {
		t.Fatal(err)
	}
	call := mcpSession(t, f, "--root", root)
	res, ok := call("sandbox_create", map[string]any{})
	if !ok {
		t.Fatalf("create: %s", mustJSON(res))
	}
	id := res["structuredContent"].(map[string]any)["id"].(string)
	local := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(local, []byte("from the sandbox\n"), 0o644)
	if res, ok := call("sandbox_cp", map[string]any{"from": local, "to": id + ":inbox"}); !ok {
		t.Fatalf("cp in: %s", mustJSON(res))
	}

	for _, to := range []string{
		outside,
		filepath.Join(outside, "new"),
		"../" + filepath.Base(outside),
		"link",
		"link/deeper/still",
		filepath.Join(root, "link"),
		"/",
	} {
		res, ok := call("sandbox_cp", map[string]any{"from": id + ":inbox", "to": to})
		if ok || !strings.Contains(mustJSON(res), "writes only under") {
			t.Errorf("cp out to %q: ok %v %s", to, ok, mustJSON(res))
		}
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep me\n" {
		t.Fatalf("a file outside the root was replaced: %q", b)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("something was written outside the root: %v", ents)
	}

	// Under the root, relative or absolute, it lands.
	for to, landed := range map[string]string{
		"out":                      filepath.Join(root, "out", "inbox", "notes.txt"),
		filepath.Join(root, "abs"): filepath.Join(root, "abs", "inbox", "notes.txt"),
		".":                        filepath.Join(root, "inbox", "notes.txt"),
		"a/../b":                   filepath.Join(root, "b", "inbox", "notes.txt"),
	} {
		if res, ok := call("sandbox_cp", map[string]any{"from": id + ":inbox", "to": to}); !ok {
			t.Fatalf("cp out to %q: %s", to, mustJSON(res))
		}
		if b, err := os.ReadFile(landed); err != nil || string(b) != "from the sandbox\n" {
			t.Fatalf("cp out to %q: %s holds %q %v", to, landed, b, err)
		}
	}
	// A symlink at a name the copy writes is kept (r2 second-second-F1).
	if res, ok := call("sandbox_cp", map[string]any{"from": id + ":inbox", "to": "trap"}); ok || !strings.Contains(mustJSON(res), "already exists") {
		t.Fatalf("cp out over trap: ok %v %s", ok, mustJSON(res))
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep me\n" {
		t.Fatalf("the copy wrote through a symlink to %s: %q", victim, b)
	}
	if st, err := os.Lstat(filepath.Join(root, "trap", "inbox", "notes.txt")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("trap/inbox/notes.txt was replaced: %v %v", st, err)
	}
}

// An agent's copy out never writes .git and never replaces a file (r2 second-second-F1).
func TestMCPSandboxCopyOutOnlyAddsFilesOutsideGit(t *testing.T) {
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644)
	os.WriteFile(filepath.Join(root, "Makefile"), []byte("all:\n"), 0o644)
	call := mcpSession(t, f, "--root", root)
	res, _ := call("sandbox_create", map[string]any{})
	id := res["structuredContent"].(map[string]any)["id"].(string)

	evil := t.TempDir()
	os.MkdirAll(filepath.Join(evil, "x", ".git"), 0o755)
	os.WriteFile(filepath.Join(evil, "x", ".git", "config"), []byte("[core]\n\tfsmonitor = evil\n"), 0o644)
	os.WriteFile(filepath.Join(evil, "x", "Makefile"), []byte("all:\n\tevil\n"), 0o644)
	if res, ok := call("sandbox_cp", map[string]any{"from": filepath.Join(evil, "x"), "to": id + ":"}); !ok {
		t.Fatalf("cp in: %s", mustJSON(res))
	}
	for _, c := range []struct{ from, to, want string }{
		{id + ":x/.git", ".", ".git"}, // entries named .git/config
		{id + ":x/Makefile", ".", "already exists"},
		{id + ":x/Makefile", ".git", ".git"},
		{id + ":x/Makefile", "sub/.GIT/hooks", ".git"},
	} {
		res, ok := call("sandbox_cp", map[string]any{"from": c.from, "to": c.to})
		if ok || !strings.Contains(mustJSON(res), c.want) {
			t.Errorf("cp %s to %s: ok %v %s", c.from, c.to, ok, mustJSON(res))
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".git", "config")); string(b) != "[core]\n" {
		t.Fatalf(".git/config rewritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "Makefile")); string(b) != "all:\n" {
		t.Fatalf("Makefile rewritten: %q", b)
	}
	// A new file still lands.
	if res, ok := call("sandbox_cp", map[string]any{"from": id + ":x/Makefile", "to": "new"}); !ok {
		t.Fatalf("cp to a new folder: %s", mustJSON(res))
	}
}

// With no --root, the root is the folder the server started in.
// With no --root, a current folder that confines nothing (a filesystem
// root, the home folder, or one that can't be read) gives no root: copy
// out is refused with the reason, and the server still serves (review
// first-F1, second-F3).
func TestMCPSandboxCopyOutRefusedWithoutAConfiningRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, c := range []struct {
		name  string
		getwd func() (string, error)
		want  string
	}{
		{"filesystem root", func() (string, error) { return string(filepath.Separator), nil }, "filesystem root"},
		{"home folder", func() (string, error) { return home, nil }, "home folder"},
		{"unreadable", func() (string, error) { return "", errors.New("gone") }, "can't be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := geteuid
			geteuid = func() int { return 1000 }
			t.Cleanup(func() { geteuid = old })
			f := newFixture(t)
			serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
			f.env.Getwd = c.getwd
			call := mcpSession(t, f)
			res, ok := call("sandbox_create", map[string]any{})
			if !ok {
				t.Fatalf("create: %s", mustJSON(res))
			}
			id := res["structuredContent"].(map[string]any)["id"].(string)
			res, ok = call("sandbox_cp", map[string]any{"from": id + ":in", "to": "passwd"})
			if ok || !strings.Contains(mustJSON(res), c.want) || !strings.Contains(mustJSON(res), "--root") {
				t.Fatalf("copy out with no confining root: ok=%v %s", ok, mustJSON(res))
			}
		})
	}
}

// As root, or with no home folder known, there is no default root even in
// an ordinary folder: sudo can leave HOME at root's while the folder is a
// user's home (review r2 sol-F1, first-F1).
func TestMCPDefaultRootNeedsAKnownHomeAndNoRoot(t *testing.T) {
	wd := t.TempDir()
	getwd := func() (string, error) { return wd, nil }
	old := geteuid
	t.Cleanup(func() { geteuid = old })

	geteuid = func() int { return 1000 }
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	if r, why := defaultRoot(getwd); r == "" {
		t.Fatalf("an ordinary folder: no root (%s)", why)
	}
	geteuid = func() int { return 0 }
	if r, why := defaultRoot(getwd); r != "" || !strings.Contains(why, "runs as root") {
		t.Fatalf("as root: %q %q", r, why)
	}
	geteuid = func() int { return 1000 }
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if r, why := defaultRoot(getwd); r != "" || !strings.Contains(why, "home folder") {
		t.Fatalf("no HOME: %q %q", r, why)
	}
}

func TestMCPSandboxCopyRootDefaultsToTheCurrentFolder(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = old })
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	wd := t.TempDir()
	f.env.Getwd = func() (string, error) { return wd, nil }
	call := mcpSession(t, f)
	res, _ := call("sandbox_create", map[string]any{})
	id := res["structuredContent"].(map[string]any)["id"].(string)
	local := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(local, []byte("a"), 0o644)
	call("sandbox_cp", map[string]any{"from": local, "to": id + ":in"})
	if res, ok := call("sandbox_cp", map[string]any{"from": id + ":in", "to": "got"}); !ok {
		t.Fatalf("cp out: %s", mustJSON(res))
	}
	if b, err := os.ReadFile(filepath.Join(wd, "got", "in", "a.txt")); err != nil || string(b) != "a" {
		t.Fatalf("landed %q %v", b, err)
	}
	if res, ok := call("sandbox_cp", map[string]any{"from": id + ":in", "to": t.TempDir()}); ok {
		t.Fatalf("cp out outside the current folder: %s", mustJSON(res))
	}
}
