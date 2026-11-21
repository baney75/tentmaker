package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxExtract caps the total bytes extracted from one archive.
const maxExtract = 512 << 20

// Download fetches rel's archive for this OS and arch into dir, checks its
// sha256 against the release's SHA256SUMS, extracts it into a fresh
// directory under dir and returns the path of the tentmaker binary inside.
// Nothing is extracted unless the checksum matches.
func (u *Updater) Download(ctx context.Context, rel Release, dir string) (string, error) {
	name := ArchiveName(u.o.GOOS, u.o.GOARCH)
	archiveURL, ok := rel.Assets[name]
	if !ok {
		return "", fmt.Errorf("%w %s for %s/%s in %s", errNoAsset, name, u.o.GOOS, u.o.GOARCH, rel.Tag)
	}
	sumsURL, ok := rel.Assets[SumsName]
	if !ok {
		return "", fmt.Errorf("%w %s in %s; refusing to install an unverified build", errNoAsset, SumsName, rel.Tag)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	var sums bytes.Buffer
	if _, err := u.fetch(ctx, sumsURL, &sums, 1<<20); err != nil {
		return "", fmt.Errorf("downloading %s: %w", SumsName, err)
	}
	want, err := lookupSum(sums.Bytes(), name)
	if err != nil {
		return "", err
	}

	f, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	archivePath := f.Name()
	defer os.Remove(archivePath)
	h := sha256.New()
	_, err = u.fetch(ctx, archiveURL, io.MultiWriter(f, h), u.o.MaxArchive)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", name, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return "", fmt.Errorf("%s failed verification: sha256 is %s, SHA256SUMS says %s; refusing to install (retry, and report it if it repeats)", name, got, want)
	}

	out, err := os.MkdirTemp(dir, "tentmaker-"+safeName(rel.Version)+"-")
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(name, ".zip") {
		err = extractZip(archivePath, out)
	} else {
		err = extractTarGz(archivePath, out)
	}
	if err != nil {
		os.RemoveAll(out)
		return "", fmt.Errorf("%s: %w", name, err)
	}
	bin := filepath.Join(out, binName(u.o.GOOS))
	st, err := os.Lstat(bin)
	if err != nil || !st.Mode().IsRegular() {
		os.RemoveAll(out)
		return "", fmt.Errorf("%s has no %s at its top level", name, binName(u.o.GOOS))
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		os.RemoveAll(out)
		return "", err
	}
	return bin, nil
}

func binName(goos string) string {
	if goos == "windows" {
		return "tentmaker.exe"
	}
	return "tentmaker"
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			return r
		}
		return '_'
	}, s)
}

// fetch GETs rawURL into w, failing past limit bytes.
func (u *Updater) fetch(ctx context.Context, rawURL string, w io.Writer, limit int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, u.o.DownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", u.o.UserAgent)
	resp, err := u.o.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("can't reach the release server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("%s: HTTP %d", redact(req.URL), resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("%s is larger than %d bytes", redact(req.URL), limit)
	}
	return n, nil
}

// lookupSum finds name in a sha256sum-format file ("<hex>  <name>" or
// "<hex> *<name>").
func lookupSum(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			return "", fmt.Errorf("%s has a malformed entry for %s", SumsName, name)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", fmt.Errorf("%s has a malformed entry for %s", SumsName, name)
		}
		return sum, nil
	}
	return "", fmt.Errorf("%s has no entry for %s; refusing to install an unverified build", SumsName, name)
}

// localPath turns an archive entry name into a path under root, refusing
// absolute paths, ".." and anything else that would leave root.
func localPath(root, name string) (string, error) {
	clean := strings.TrimSuffix(name, "/")
	if clean == "" || clean == "." || strings.Contains(clean, "\\") {
		return "", fmt.Errorf("unsafe archive entry %q", name)
	}
	rel := filepath.FromSlash(clean)
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("unsafe archive entry %q (leaves the target directory)", name)
	}
	return filepath.Join(root, rel), nil
}

func extractTarGz(path, root string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bad tar archive: %w", err)
		}
		dst, err := localPath(root, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxExtract {
				return fmt.Errorf("archive expands past %d bytes", maxExtract)
			}
			if err := writeFile(dst, tr, hdr.Size, fs.FileMode(hdr.Mode)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q is not a regular file or directory (type %q); refusing links and devices", hdr.Name, hdr.Typeflag)
		}
	}
}

func extractZip(path, root string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("bad zip archive: %w", err)
	}
	defer zr.Close()
	var total int64
	for _, zf := range zr.File {
		dst, err := localPath(root, zf.Name)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case mode.IsRegular():
			size := int64(zf.UncompressedSize64)
			if size < 0 || zf.UncompressedSize64 > maxExtract {
				return fmt.Errorf("archive entry %q is too large", zf.Name)
			}
			total += size
			if total > maxExtract {
				return fmt.Errorf("archive expands past %d bytes", maxExtract)
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			err = writeFile(dst, rc, size, mode)
			rc.Close()
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q is not a regular file or directory (%v); refusing links and devices", zf.Name, mode.Type())
		}
	}
	return nil
}

func writeFile(dst string, r io.Reader, size int64, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	perm := mode.Perm() & 0o755
	if perm == 0 {
		perm = 0o644
	}
	// O_EXCL: a duplicate entry can't overwrite an earlier file.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("archive entry %s has the wrong size", filepath.Base(dst))
	}
	return nil
}
