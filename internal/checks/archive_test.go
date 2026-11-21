package checks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func tarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.typ == tar.TypeXGlobalHeader {
			// GitHub's tarballs start with one carrying the commit.
			h := &tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "abc123"}}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			continue
		}
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ, Linkname: e.link}
		if e.mode != 0 {
			h.Mode = e.mode
		}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Typeflag == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeArchive(t *testing.T, entries []entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, tarGz(t, entries), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractStripsTopFolder(t *testing.T) {
	a := writeArchive(t, []entry{
		{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		{name: "repo-abc/", typ: tar.TypeDir},
		{name: "repo-abc/go.mod", body: "module x\n"},
		{name: "repo-abc/scripts/run.sh", body: "echo hi\n", mode: 0o755},
		{name: "repo-abc/link", typ: tar.TypeSymlink, link: "scripts/run.sh"},
		{name: "repo-abc/hard", typ: tar.TypeLink, link: "repo-abc/go.mod"},
	})
	dst := filepath.Join(t.TempDir(), "src")
	if err := extract(a, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "go.mod")); string(b) != "module x\n" {
		t.Fatalf("go.mod = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "hard")); string(b) != "module x\n" {
		t.Fatalf("hard link = %q", b)
	}
	fi, err := os.Stat(filepath.Join(dst, "scripts", "run.sh"))
	if err != nil || (fi.Mode()&0o100 == 0 && os.PathSeparator == '/') {
		t.Fatalf("run.sh: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "link")); string(b) != "echo hi\n" {
		t.Fatalf("symlink = %q", b)
	}
}

func TestExtractRefusesUnsafeArchives(t *testing.T) {
	for _, c := range []struct {
		name    string
		entries []entry
	}{
		{"dotdot", []entry{{name: "repo/../../evil", body: "x"}}},
		{"absolute", []entry{{name: "/etc/evil", body: "x"}}},
		{"symlink out", []entry{{name: "repo/l", typ: tar.TypeSymlink, link: "../../etc"}}},
		{"symlink absolute", []entry{{name: "repo/l", typ: tar.TypeSymlink, link: "/etc"}}},
		{"through symlink", []entry{
			{name: "repo/l", typ: tar.TypeSymlink, link: "."},
			{name: "repo/l/x", body: "x"},
		}},
		{"hard link out", []entry{{name: "repo/h", typ: tar.TypeLink, link: "../etc/passwd"}}},
		{"device", []entry{{name: "repo/dev", typ: tar.TypeChar}}},
		{"fifo", []entry{{name: "repo/fifo", typ: tar.TypeFifo}}},
		{"two tops", []entry{{name: "a/x", body: "1"}, {name: "b/y", body: "2"}}},
		{"no top", []entry{{name: "file", body: "1"}}},
		{"empty", nil},
	} {
		root := t.TempDir()
		dst := filepath.Join(root, "out", "src")
		err := extract(writeArchive(t, c.entries), dst)
		if err == nil {
			t.Errorf("%s: extracted", c.name)
		}
		if _, err := os.Stat(filepath.Join(root, "evil")); err == nil {
			t.Errorf("%s: wrote outside", c.name)
		}
	}
}

func TestDownloadHonorsHeaderAndHidesSecrets(t *testing.T) {
	data := []byte("archive bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "tok-123" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "a")
	ctx := context.Background()
	if err := download(ctx, srv.Client(), srv.URL+"/a?token=urlsecret", "PRIVATE-TOKEN: tok-123", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, data) {
		t.Fatalf("got %q", b)
	}
	err := download(ctx, srv.Client(), srv.URL+"/a?token=urlsecret", "PRIVATE-TOKEN: wrong", dst)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	srv.Close()
	err = download(ctx, srv.Client(), srv.URL+"/a?token=urlsecret", "PRIVATE-TOKEN: tok-123", dst)
	if err == nil || strings.Contains(err.Error(), "urlsecret") || strings.Contains(err.Error(), "tok-123") {
		t.Fatalf("err leaks a secret: %v", err)
	}
	if err := download(ctx, srv.Client(), "file:///etc/passwd", "", dst); err == nil {
		t.Fatal("file URL accepted")
	}
}
