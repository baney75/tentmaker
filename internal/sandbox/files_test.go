package sandbox

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
