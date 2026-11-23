package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type tarEntry struct {
	name, body, link string
	typ              byte
}

func tarOf(t *testing.T, entries ...tarEntry) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	return &buf
}

func TestExtractStaysInside(t *testing.T) {
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	for _, name := range []string{"../escape.txt", "/etc/escape.txt", "a/../../escape.txt"} {
		_, err := Extract(tarOf(t, tarEntry{name: name, body: "x", typ: tar.TypeReg}), dst)
		if err == nil {
			t.Errorf("%s was extracted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Fatal("a file landed outside the destination")
	}

	// Links in the copy are never made; regular files and folders are.
	res, err := Extract(tarOf(t,
		tarEntry{name: "out/", typ: tar.TypeDir},
		tarEntry{name: "out/ok.txt", body: "fine", typ: tar.TypeReg},
		tarEntry{name: "out/link", link: "/etc/passwd", typ: tar.TypeSymlink},
		tarEntry{name: "out/hard", link: "/etc/passwd", typ: tar.TypeLink},
	), dst)
	if err != nil || res.Files != 1 || res.Skipped != 2 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "out", "ok.txt")); string(b) != "fine" {
		t.Fatalf("ok.txt = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dst, "out", "link")); err == nil {
		t.Fatal("a symlink was made")
	}
}

func TestExtractDoesNotWriteThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	os.MkdirAll(outside, 0o755)
	dst := filepath.Join(parent, "dst")
	os.MkdirAll(dst, 0o755)
	// A link already in the destination, pointing out of it.
	if err := os.Symlink(outside, filepath.Join(dst, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "victim"), filepath.Join(dst, "file")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(outside, "victim"), []byte("keep"), 0o644)
	Extract(tarOf(t, tarEntry{name: "sub/evil.txt", body: "x", typ: tar.TypeReg}), dst)
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("wrote through a directory symlink")
	}
	if _, err := Extract(tarOf(t, tarEntry{name: "file", body: "overwritten", typ: tar.TypeReg}), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim")); string(b) != "keep" {
		t.Fatal("wrote through a file symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "file")); string(b) != "overwritten" {
		t.Fatalf("the link was not replaced by the file: %q", b)
	}
}

func TestPackPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("A"), 0o644)
	var buf bytes.Buffer
	if err := PackPath(&buf, src); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if _, err := Extract(&buf, out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "proj", "sub", "a.txt")); string(b) != "A" {
		t.Fatalf("got %q", b)
	}
}

// A file hardlinked to one outside the destination is replaced, not
// written through (review sol-F1).
func TestExtractDoesNotWriteThroughAHardlink(t *testing.T) {
	parent := t.TempDir()
	victim := filepath.Join(parent, "victim")
	os.WriteFile(victim, []byte("keep"), 0o644)
	dst := filepath.Join(parent, "dst")
	os.MkdirAll(dst, 0o755)
	if err := os.Link(victim, filepath.Join(dst, "file")); err != nil {
		t.Skip("no hardlinks here:", err)
	}
	if _, err := Extract(tarOf(t, tarEntry{name: "file", body: "overwritten", typ: tar.TypeReg}), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("wrote through a hardlink: the outside file is %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "file")); string(b) != "overwritten" {
		t.Fatalf("the file was not replaced: %q", b)
	}
}

// A subfolder of a Git work tree has no .git of its own; it is still
// packed from Git's list, so files Git ignores stay out (review first-F1).
func TestPackWorkTreeSubfolderUsesGit(t *testing.T) {
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	sub := filepath.Join(repo, "services", "api")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(sub, ".env"), []byte("SECRET=1"), 0o644)
	var asked string
	git := func(_ context.Context, dir string) ([]byte, error) {
		asked = dir
		return []byte("main.go\x00"), nil
	}
	var buf bytes.Buffer
	n, err := PackWorkTree(context.Background(), &buf, sub, git)
	if err != nil || n != 1 || asked != sub {
		t.Fatalf("packed %d, err %v, git asked about %q", n, err, asked)
	}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == ".env" {
			t.Fatal("a file Git ignores was packed")
		}
	}
}

// Reached through a symlink, the subfolder is still found to be in the
// work tree (review r2 sol-F2).
func TestPackWorkTreeThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	sub := filepath.Join(repo, "services", "api")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".env"), []byte("SECRET=1"), 0o644)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(repo, "services"), alias); err != nil {
		t.Fatal(err)
	}
	asked := false
	git := func(context.Context, string) ([]byte, error) { asked = true; return nil, nil }
	n, err := PackWorkTree(context.Background(), io.Discard, filepath.Join(alias, "api"), git)
	if err != nil || n != 0 || !asked {
		t.Fatalf("packed %d, err %v, asked git %v", n, err, asked)
	}
}

// A guarded extract (an agent's copy) writes no .git entry and replaces no
// existing file; an unguarded one still replaces (review r2
// second-second-F1).
func TestExtractGuarded(t *testing.T) {
	dst := t.TempDir()
	os.WriteFile(filepath.Join(dst, "Makefile"), []byte("keep"), 0o644)
	for _, name := range []string{".git/config", "a/.Git/hooks/pre-commit"} {
		if _, err := ExtractWith(tarOf(t, tarEntry{name: name, body: "evil", typ: tar.TypeReg}), dst, ExtractOptions{Guarded: true}); err == nil || !strings.Contains(err.Error(), ".git") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		t.Error("a .git folder was written")
	}
	if _, err := ExtractWith(tarOf(t, tarEntry{name: "Makefile", body: "evil", typ: tar.TypeReg}), dst, ExtractOptions{Guarded: true}); err == nil {
		t.Error("a guarded extract replaced Makefile")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "Makefile")); string(b) != "keep" {
		t.Fatalf("Makefile: %q", b)
	}
	if _, err := ExtractWith(tarOf(t, tarEntry{name: "new.txt", body: "ok", typ: tar.TypeReg}), dst, ExtractOptions{Guarded: true}); err != nil {
		t.Fatalf("a new file: %v", err)
	}
	if _, err := Extract(tarOf(t, tarEntry{name: "Makefile", body: "new", typ: tar.TypeReg}), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "Makefile")); string(b) != "new" {
		t.Fatalf("an unguarded extract did not replace: %q", b)
	}
}
