package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzLookupSum: no panic, and a found sum is 64 lowercase hex characters
// from a line that names exactly the asset.
func FuzzLookupSum(f *testing.F) {
	good := strings.Repeat("ab", 32)
	f.Add([]byte(good+"  tentmaker_linux_amd64.tar.gz\n"), "tentmaker_linux_amd64.tar.gz")
	f.Add([]byte(good+" *tentmaker_windows_arm64.zip\r\n"+strings.Repeat("0", 64)+"  SHA256SUMS\n"), "tentmaker_windows_arm64.zip")
	f.Add([]byte("zz  x\n\n"+strings.ToUpper(good)+"  x"), "x")
	f.Add([]byte(good+"  a b\n"), "a b")
	f.Fuzz(func(t *testing.T, sums []byte, name string) {
		got, err := lookupSum(sums, name)
		if err != nil {
			return
		}
		if len(got) != 64 || strings.Trim(got, "0123456789abcdef") != "" {
			t.Fatalf("lookupSum = %q, not 64 lowercase hex", got)
		}
		found := false
		for _, l := range strings.Split(string(sums), "\n") {
			fs := strings.Fields(l)
			if len(fs) == 2 && strings.ToLower(fs[0]) == got && strings.TrimPrefix(fs[1], "*") == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("lookupSum(%q) = %q, but no line names it", name, got)
		}
	})
}

// fuzzPath builds an archive entry name from pieces meant to escape.
func fuzzPath(next func() byte) string {
	pieces := []string{"tentmaker", "a", "..", ".", "", "/", `x\y`, "C:", "tentmaker.exe", "b"}
	n := int(next()%4) + 1
	parts := make([]string, n)
	for i := range parts {
		parts[i] = pieces[int(next())%len(pieces)]
	}
	p := strings.Join(parts, "/")
	if next()&1 == 1 {
		p = "/" + p
	}
	return p
}

func reader(prog []byte) func() byte {
	return func() byte {
		if len(prog) == 0 {
			return 0
		}
		b := prog[0]
		prog = prog[1:]
		return b
	}
}

// hostileTarGz and hostileZip build release archives with escaping names,
// links, devices and duplicates from fuzz bytes.
func hostileTarGz(prog []byte) []byte {
	next := reader(prog)
	types := []byte{tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeFifo, tar.TypeReg}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i < 32 && len(prog) > 0; i++ {
		typ := types[int(next())%len(types)]
		h := &tar.Header{Typeflag: typ, Name: fuzzPath(next), Mode: int64(next()) | 0o4600, Format: tar.FormatPAX}
		var body []byte
		switch typ {
		case tar.TypeReg:
			body = bytes.Repeat([]byte{next()}, int(next()%16))
			h.Size = int64(len(body))
		case tar.TypeSymlink, tar.TypeLink:
			h.Linkname = fuzzPath(next)
		}
		prog = prog[min(len(prog), 4):]
		if tw.WriteHeader(h) != nil {
			continue
		}
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func hostileZip(prog []byte) []byte {
	next := reader(prog)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 32 && len(prog) > 0; i++ {
		h := &zip.FileHeader{Name: fuzzPath(next), Method: zip.Store}
		switch next() % 4 {
		case 0:
			h.SetMode(0o755)
		case 1:
			h.SetMode(fs.ModeDir | 0o755)
		case 2:
			h.SetMode(fs.ModeSymlink | 0o777)
		case 3:
			h.SetMode(fs.ModeDevice | 0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			continue
		}
		_, _ = w.Write(bytes.Repeat([]byte{next()}, int(next()%16)))
		prog = prog[min(len(prog), 4):]
	}
	_ = zw.Close()
	return buf.Bytes()
}

// checkExtracted: nothing was written beside out, and out holds only
// folders and regular files without setuid bits. The release extractor
// refuses links and devices outright.
func checkExtracted(t *testing.T, base, out string) {
	t.Helper()
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if e.Name() != "a" && e.Name() != "out" && e.Name() != "outside" {
			t.Fatalf("extraction wrote %s beside the target", e.Name())
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(base, "outside")); len(ents) != 0 {
		t.Fatalf("extraction wrote into a sibling dir")
	}
	_ = filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return nil
		}
		m := fi.Mode()
		if !m.IsDir() && !m.IsRegular() {
			t.Fatalf("%s is not a file or folder: %v", p, m)
		}
		if m&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Fatalf("%s kept a setuid or setgid bit: %v", p, m)
		}
		return nil
	})
}

func fuzzExtract(t *testing.T, archive []byte, zipped bool) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	out := filepath.Join(base, "out")
	if err := os.WriteFile(a, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if zipped {
		_ = extractZip(a, out)
	} else {
		_ = extractTarGz(a, out)
	}
	checkExtracted(t, base, out)
}

func FuzzExtractTarGz(f *testing.F) {
	for _, s := range []string{"\x00\x00\x00\x00\x07a", "\x02\x02\x00\x00\x00\x02\x01", "\x03\x00\x00\x00\x00\x00\x02", "\x04\x01\x00\x01", "\x00\x03\x02\x02\x00\x00"} {
		f.Add(hostileTarGz([]byte(s)), true)
		f.Add([]byte(s), false)
	}
	f.Fuzz(func(t *testing.T, data []byte, build bool) {
		if build {
			data = hostileTarGz(data)
		}
		fuzzExtract(t, data, false)
	})
}

func FuzzExtractZip(f *testing.F) {
	for _, s := range []string{"\x00\x00\x00\x00\x07a", "\x02\x02\x02\x00\x00\x00\x01", "\x03\x00\x00\x03\x00\x00\x02", "\x00\x02\x02\x01\x00"} {
		f.Add(hostileZip([]byte(s)), true)
		f.Add([]byte(s), false)
	}
	f.Fuzz(func(t *testing.T, data []byte, build bool) {
		if build {
			data = hostileZip(data)
		}
		fuzzExtract(t, data, true)
	})
}

// FuzzParseVersion: no panic, and a parsed version prints back to itself.
func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{"1.2.3", "v0.1.0", "1.2.3-dev.202611210400.abc1234", "1.2.3+build.5", "01.2.3", "1.2.3-", "1.2.3-a..b",
		"99999999999999999999.0.0", "1.2.3-0.00.1", " v1.2.3 "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseVersion(s)
		if err != nil {
			return
		}
		if v.Major < 0 || v.Minor < 0 || v.Patch < 0 {
			t.Fatalf("ParseVersion(%q) = %+v has a negative part", s, v)
		}
		back, err := ParseVersion(v.String())
		if err != nil || back.Compare(v) != 0 || back.String() != v.String() {
			t.Fatalf("ParseVersion(%q).String() = %q parses to %+v, %v", s, v.String(), back, err)
		}
		if v.Compare(v) != 0 {
			t.Fatalf("%q does not equal itself", s)
		}
	})
}

// genVersion makes a version from fuzz bytes out of small parts, so
// triples often share a prefix and the interesting comparisons happen.
func genVersion(next func() byte) Version {
	ids := []string{"0", "1", "2", "10", "01", "dev", "alpha", "beta", "a", "A", "z9", "202611210400", "202611210401", "abc1234", "-", "9999999999999999999999"}
	v := Version{Major: int64(next() % 3), Minor: int64(next() % 3), Patch: int64(next() % 3)}
	for n := int(next() % 4); n > 0; n-- {
		v.Pre = append(v.Pre, ids[int(next())%len(ids)])
	}
	return v
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// FuzzVersionOrder: Compare is a total order on generated triples:
// reflexive, antisymmetric and transitive, and Newer agrees with it.
func FuzzVersionOrder(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 5, 0, 0, 0, 2, 5, 11, 0, 0, 0, 2, 5, 12})
	f.Add([]byte{1, 2, 0, 0, 1, 2, 0, 1, 3, 4, 0, 1, 2, 0, 2, 3, 3})
	f.Add([]byte{0, 0, 0, 2, 3, 1, 0, 0, 0, 2, 4, 1, 0, 0, 0, 1, 15})
	f.Fuzz(func(t *testing.T, data []byte) {
		next := reader(data)
		a, b, c := genVersion(next), genVersion(next), genVersion(next)
		for _, p := range [][2]Version{{a, b}, {b, c}, {a, c}} {
			x, y := p[0], p[1]
			if sign(x.Compare(y)) != -sign(y.Compare(x)) {
				t.Fatalf("not antisymmetric: %s vs %s: %d and %d", x, y, x.Compare(y), y.Compare(x))
			}
			if n, err := Newer(x.String(), y.String()); err != nil || n != (x.Compare(y) > 0) {
				t.Fatalf("Newer(%s, %s) = %v, %v; Compare = %d", x, y, n, err, x.Compare(y))
			}
		}
		if a.Compare(a) != 0 {
			t.Fatalf("%s is not equal to itself", a)
		}
		if a.Compare(b) <= 0 && b.Compare(c) <= 0 && a.Compare(c) > 0 {
			t.Fatalf("not transitive: %s <= %s <= %s but %s > %s", a, b, c, a, c)
		}
		if a.Compare(b) >= 0 && b.Compare(c) >= 0 && a.Compare(c) < 0 {
			t.Fatalf("not transitive: %s >= %s >= %s but %s < %s", a, b, c, a, c)
		}
	})
}
