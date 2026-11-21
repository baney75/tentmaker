package checks

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	maxArchiveBytes   int64 = 2 << 30 // the download
	maxExtractedBytes int64 = 8 << 30 // what it unpacks to
	readIdleTimeout         = 60 * time.Second
)

var errReadIdle = errors.New("no data for 60 s")

// newDownloadClient has no overall timeout: a big archive may take a while.
// download bounds each read instead.
func newDownloadClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 30 * time.Second
	t.ResponseHeaderTimeout = readIdleTimeout
	return &http.Client{Transport: t}
}

// download writes the archive at rawURL to dst. Errors never include the
// URL or the header, since either may carry a token.
// localArchive reports whether rawURL is a file:// URL and, if so, its
// absolute path. The darwin executor copies the source into the VM and
// points the bundle at it this way.
func localArchive(rawURL string) (string, bool, error) {
	if !strings.HasPrefix(strings.ToLower(rawURL), "file:") {
		return "", false, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || u.Opaque != "" {
		return "", true, errors.New("the archive URL is not a file URL with an absolute path")
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // file:///C:/x
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		return "", true, errors.New("the archive URL is not a file URL with an absolute path")
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", true, errors.New("the source archive file is missing")
	}
	return p, true, nil
}

func download(ctx context.Context, client *http.Client, rawURL, header, dst string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("the archive URL is not an http(s) URL")
	}
	var hName, hValue string
	if header != "" {
		n, v, ok := strings.Cut(header, ":")
		n = strings.TrimSpace(n)
		if !ok || n == "" || strings.ContainsAny(n, " \t\r\n") || strings.ContainsAny(v, "\r\n") {
			return errors.New("the bundle's download header is malformed")
		}
		hName, hValue = n, strings.TrimSpace(v)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(readIdleTimeout, func() { cancel(errReadIdle) })
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("can't build the download request")
	}
	if hName != "" {
		req.Header.Set(hName, hValue)
	}
	c := *client
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		// Keep the auth header on the archive's own host only.
		if hName != "" && r.URL.Host != u.Host {
			r.Header.Del(hName)
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return cleanNetErr(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the forge answered HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxArchiveBytes {
		return errors.New("the archive is larger than 2 GiB")
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	body := &idleReader{r: resp.Body, t: idle}
	n, err := io.Copy(f, io.LimitReader(body, maxArchiveBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return cleanNetErr(ctx, err)
	}
	if n > maxArchiveBytes {
		return errors.New("the archive is larger than 2 GiB")
	}
	return nil
}

type idleReader struct {
	r io.Reader
	t *time.Timer
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.t.Reset(readIdleTimeout)
	}
	return n, err
}

// cleanNetErr drops the URL from net/http errors.
func cleanNetErr(ctx context.Context, err error) error {
	if c := context.Cause(ctx); c != nil && !errors.Is(c, context.Canceled) {
		return c
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err
}

// extract unpacks a tar.gz into dst, dropping the single top-level folder
// forges put around a repo. It refuses absolute paths, .. components,
// symlinks that leave dst, entries reached through a symlink, hard links
// outside dst, and device files.
func extract(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("the archive is not gzip: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	top := ""
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the archive is damaged: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue
		}
		rel, first, err := entryPath(h.Name)
		if err != nil {
			return err
		}
		if top == "" {
			top = first
		} else if first != top {
			return fmt.Errorf("the archive has more than one top-level folder (%s and %s)", top, first)
		}
		if rel == "" {
			if h.Typeflag != tar.TypeDir {
				return fmt.Errorf("the archive has no top-level folder (%s)", h.Name)
			}
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := noSymlinkParents(dst, rel); err != nil {
			return err
		}
		if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive entry %s would overwrite a symlink", rel)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxExtractedBytes {
				return errors.New("the archive unpacks to more than 8 GiB")
			}
			if err := writeFile(target, tr, h.Size, h.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := makeSymlink(dst, rel, target, h.Linkname); err != nil {
				return err
			}
		case tar.TypeLink:
			lrel, lfirst, err := entryPath(h.Linkname)
			if err != nil || lfirst != top || lrel == "" {
				return fmt.Errorf("archive entry %s links outside the archive", rel)
			}
			if err := noSymlinkParents(dst, lrel); err != nil {
				return err
			}
			srcPath := filepath.Join(dst, filepath.FromSlash(lrel))
			fi, err := os.Lstat(srcPath)
			if err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("archive entry %s links to %s, which is not a file", rel, lrel)
			}
			total += fi.Size()
			if total > maxExtractedBytes {
				return errors.New("the archive unpacks to more than 8 GiB")
			}
			in, err := os.Open(srcPath)
			if err != nil {
				return err
			}
			err = writeFile(target, in, fi.Size(), fi.Mode())
			in.Close()
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %s is a device or special file, which checks mode refuses", rel)
		}
	}
	if top == "" {
		return errors.New("the archive is empty")
	}
	return nil
}

// entryPath checks an archive name and splits off its top-level folder.
func entryPath(name string) (rel, first string, err error) {
	bad := func(why string) (string, string, error) {
		return "", "", fmt.Errorf("archive entry %q %s; refusing the archive", name, why)
	}
	if name == "" {
		return bad("has no name")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return bad("is an absolute path")
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(name, `\:`) {
		return bad("has a backslash or colon")
	}
	name = strings.TrimPrefix(name, "./")
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == ".." {
			return bad("climbs out with ..")
		}
	}
	first = parts[0]
	if first == "" || first == "." {
		return bad("has no top-level folder")
	}
	rest := path.Clean(strings.Join(parts[1:], "/"))
	if rest == "." || rest == "/" {
		rest = ""
	}
	return strings.TrimPrefix(rest, "/"), first, nil
}

// noSymlinkParents refuses an entry whose parent folders include a symlink,
// so nothing is written through a link.
func noSymlinkParents(dst, rel string) error {
	parts := strings.Split(rel, "/")
	cur := dst
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive entry %s goes through a symlink; refusing the archive", rel)
		}
	}
	return nil
}

func makeSymlink(dst, rel, target, link string) error {
	if link == "" || strings.HasPrefix(link, "/") || filepath.IsAbs(link) || filepath.VolumeName(link) != "" {
		return fmt.Errorf("archive symlink %s points to an absolute path; refusing the archive", rel)
	}
	resolved := path.Clean(path.Join(path.Dir(rel), link))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("archive symlink %s points outside the archive; refusing the archive", rel)
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(link, `\:`) {
		return fmt.Errorf("archive symlink %s has a backslash or colon; refusing the archive", rel)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("archive entry %s appears twice", rel)
	}
	if err := os.Symlink(filepath.FromSlash(link), target); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		// Windows without symlink rights: keep the link text, as git does
		// with core.symlinks=false.
		return os.WriteFile(target, []byte(link), 0o644)
	}
	return nil
}

func writeFile(target string, r io.Reader, size int64, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("archive entry %s is truncated", filepath.Base(target))
	}
	return err
}
