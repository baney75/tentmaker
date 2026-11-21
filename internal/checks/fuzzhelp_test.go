package checks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostileTar turns fuzz bytes into a tar.gz built to escape: names and
// link targets come from pieces like "..", "/", "link" and "C:", entry
// types include symlinks, hard links, devices and FIFOs, and the same name
// can repeat. HostileTar never fails; entries tar refuses are dropped.
func hostileTar(prog []byte) []byte {
	pieces := []string{"top", "top", "a", "b", "..", ".", "", "link", "up", "esc", "/", "top2", `x\y`, "C:", "f.txt", "etc"}
	types := []byte{tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo,
		tar.TypeReg, tar.TypeSymlink, tar.TypeDir, tar.TypeGNUSparse, tar.TypeCont}
	next := func() byte {
		if len(prog) == 0 {
			return 0
		}
		b := prog[0]
		prog = prog[1:]
		return b
	}
	path := func(abs bool) string {
		n := int(next()%5) + 1
		parts := make([]string, n)
		for i := range parts {
			parts[i] = pieces[int(next())%len(pieces)]
		}
		p := strings.Join(parts, "/")
		if abs {
			p = "/" + p
		}
		return p
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; len(prog) > 0 && i < 64; i++ {
		op := next()
		typ := types[int(op)%len(types)]
		h := &tar.Header{Typeflag: typ, Name: path(op&0x40 != 0), Mode: int64(next()) | 0o600, Format: tar.FormatPAX}
		if op&0x80 != 0 {
			h.Mode |= 0o4000 // setuid: must not survive
		}
		var body []byte
		switch typ {
		case tar.TypeReg:
			body = bytes.Repeat([]byte{next()}, int(next()%32))
			h.Size = int64(len(body))
		case tar.TypeSymlink, tar.TypeLink:
			h.Linkname = path(next()&1 == 1)
		case tar.TypeChar, tar.TypeBlock:
			h.Devmajor, h.Devminor = 1, 3
		}
		if err := tw.WriteHeader(h); err != nil {
			continue
		}
		if len(body) > 0 {
			_, _ = tw.Write(body)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// extractSandbox lays out base/{a.tar.gz, dst, outside/sentinel} and
// returns the archive path, dst, and a check that nothing changed outside
// dst and nothing inside dst is a device, FIFO or a symlink that resolves
// out of dst.
func extractSandbox(t *testing.T, archive []byte) (string, string, func()) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(base, "a.tar.gz")
	dst := filepath.Join(base, "dst")
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(a, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		ents, _ := os.ReadDir(base)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if got := strings.Join(names, ","); got != "a.tar.gz,dst,outside" && got != "a.tar.gz,outside" {
			t.Fatalf("extraction wrote beside the target: %s", got)
		}
		ents, _ = os.ReadDir(outside)
		if len(ents) != 1 || ents[0].Name() != "sentinel" {
			t.Fatalf("extraction wrote into a sibling dir: %v", ents)
		}
		if b, _ := os.ReadFile(filepath.Join(outside, "sentinel")); string(b) != "keep" {
			t.Fatalf("extraction changed a file outside the target: %q", b)
		}
		_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			fi, err := os.Lstat(p)
			if err != nil {
				return nil
			}
			switch m := fi.Mode(); {
			case m.IsDir(), m.IsRegular():
				if m&(os.ModeSetuid|os.ModeSetgid) != 0 {
					t.Fatalf("%s kept a setuid or setgid bit: %v", p, m)
				}
			case m&os.ModeSymlink != 0:
				if escapes(dst, p) {
					target, _ := os.Readlink(p)
					t.Fatalf("symlink %s -> %s resolves outside %s", p, target, dst)
				}
			default:
				t.Fatalf("%s is a special file: %v", p, m)
			}
			return nil
		})
	}
	return a, dst, check
}

// escapes resolves link (a symlink under root) one component at a time,
// following symlinks the way the OS does, and reports whether any step
// lands outside root. A loop or a dangling tail is not an escape.
func escapes(root, link string) bool {
	inside := func(p string) bool {
		rel, err := filepath.Rel(root, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	cur := filepath.Dir(link)
	pending := strings.Split(filepath.ToSlash(target), "/")
	if filepath.IsAbs(target) {
		cur = filepath.VolumeName(target) + string(filepath.Separator)
	}
	for hops := 0; len(pending) > 0; {
		c := pending[0]
		pending = pending[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
		default:
			next := filepath.Join(cur, c)
			fi, err := os.Lstat(next)
			if err != nil || fi.Mode()&os.ModeSymlink == 0 {
				cur = next
				break
			}
			if hops++; hops > 40 {
				return false
			}
			t, err := os.Readlink(next)
			if err != nil {
				return false
			}
			if filepath.IsAbs(t) {
				cur = filepath.VolumeName(t) + string(filepath.Separator)
			}
			pending = append(strings.Split(filepath.ToSlash(t), "/"), pending...)
		}
		if !inside(cur) {
			return true
		}
	}
	return false
}
