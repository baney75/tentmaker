package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Copies move as tar streams. Copy in: the caller packs files here and tar
// unpacks them inside the sandbox, as the sandbox's user. Copy out: tar
// packs them inside the sandbox and Extract unpacks them here, refusing
// anything that could land outside the destination.

// PackPath writes src (a file or a folder, which goes whole) as a tar
// stream whose one top entry is src's base name.
func PackPath(w io.Writer, src string) error {
	src = filepath.Clean(src)
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	base := filepath.Base(src)
	if base == "." || base == string(filepath.Separator) {
		return fmt.Errorf("name a file or folder to copy, not %s", src)
	}
	tw := tar.NewWriter(w)
	if !st.IsDir() {
		if err := addFile(tw, src, base, st); err != nil {
			return err
		}
		return tw.Close()
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return addFile(tw, p, path.Join(base, filepath.ToSlash(rel)), info)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// GitLister lists a Git work tree's files: the output of
// `git -C dir ls-files -z --cached --others --exclude-standard`.
type GitLister func(ctx context.Context, dir string) ([]byte, error)

// inWorkTree reports whether dir or a folder above it holds .git: a
// subfolder of a work tree has none of its own, and walking it would send
// files Git ignores, such as .env (review first-F1).
func inWorkTree(dir string) bool {
	// Through a symlink, the folders above dir on disk aren't the ones in
	// its path (review sol-F2); a path that can't be resolved counts as in
	// one, so Git decides.
	real, err := filepath.EvalSymlinks(dir)
	if err == nil {
		real, err = filepath.Abs(real)
	}
	if err != nil {
		return true
	}
	for d := real; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// PackWorkTree writes dir's working tree as a tar stream with no top
// folder: in a Git work tree, every tracked file as it is on disk now
// (uncommitted changes included) and every untracked file Git doesn't
// ignore, and no .git; anywhere else, the whole folder. It returns how
// many files it packed.
func PackWorkTree(ctx context.Context, w io.Writer, dir string, git GitLister) (int, error) {
	dir = filepath.Clean(dir)
	tw := tar.NewWriter(w)
	n := 0
	out, gitErr := []byte(nil), errors.New("no git")
	if git != nil {
		if inWorkTree(dir) {
			out, gitErr = git(ctx, dir)
			if gitErr != nil {
				return 0, fmt.Errorf("list the files of the Git work tree %s: %v", dir, gitErr)
			}
		}
	}
	if gitErr == nil {
		for _, rel := range bytes.Split(out, []byte{0}) {
			name := string(rel)
			if name == "" || !localName(name) {
				continue
			}
			p := filepath.Join(dir, filepath.FromSlash(name))
			st, err := os.Lstat(p)
			if err != nil || st.IsDir() {
				// Deleted but not yet staged, or a submodule: skip.
				continue
			}
			if err := addFile(tw, p, name, st); err != nil {
				return n, err
			}
			n++
		}
		return n, tw.Close()
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := addFile(tw, p, filepath.ToSlash(rel), info); err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	return n, tw.Close()
}

// localName says whether a slash path stays below its root.
func localName(name string) bool {
	return filepath.IsLocal(filepath.FromSlash(name)) && !strings.ContainsRune(name, '\\')
}

// addFile writes one entry: a regular file, a folder or a symlink (as a
// link, never followed). Other kinds are skipped.
func addFile(tw *tar.Writer, p, name string, info fs.FileInfo) error {
	var link string
	switch {
	case info.Mode().IsRegular(), info.IsDir():
	case info.Mode()&fs.ModeSymlink != 0:
		l, err := os.Readlink(p)
		if err != nil {
			return err
		}
		link = filepath.ToSlash(l)
	default:
		return nil
	}
	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	h.Name = name
	if info.IsDir() {
		h.Name += "/"
	}
	// Names, not ids: the sandbox's user owns what lands there.
	h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
	h.Format = tar.FormatPAX
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.CopyN(tw, f, h.Size); err != nil {
		return fmt.Errorf("%s changed while it was copied: %v", p, err)
	}
	return nil
}

// ExtractResult is what Extract wrote.
type ExtractResult struct {
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Skipped int   `json:"skipped"` // links and special files, which are never written
}

// Extract unpacks a tar stream into the folder dst. Only regular files and
// folders are written; an entry whose name leaves dst, or whose path runs
// through a symlink already in dst, stops the copy. It writes at most
// MaxCopy bytes.
func Extract(r io.Reader, dst string) (ExtractResult, error) {
	return ExtractWith(r, dst, ExtractOptions{})
}

// ExtractOptions narrow what an extract may write.
type ExtractOptions struct {
	// Guarded is for a copy an agent asked for (the MCP server): no entry
	// may name a .git folder, whose config and hooks run code on the next
	// git command, and no existing file is replaced, so a copy only adds
	// files and can't rewrite a Makefile or uncommitted work (review r2
	// second-second-F1).
	Guarded bool
}

// ExtractWith is Extract with opts.
func ExtractWith(r io.Reader, dst string, opts ExtractOptions) (ExtractResult, error) {
	var res ExtractResult
	dst, err := filepath.Abs(dst)
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return res, err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return res, err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, fmt.Errorf("read the copy: %v", err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || name == "." {
			continue
		}
		if !localName(name) {
			return res, fmt.Errorf("the copy holds %q, which would land outside %s", h.Name, dst)
		}
		rel := filepath.FromSlash(name)
		if opts.Guarded && HasGitPart(name) {
			return res, fmt.Errorf("the copy holds %q, inside a .git folder, which an agent's copy never writes", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, 0o755); err != nil {
				return res, err
			}
		case tar.TypeReg:
			if res.Bytes+h.Size > MaxCopy {
				return res, fmt.Errorf("the copy is larger than %d bytes", int64(MaxCopy))
			}
			if d := filepath.Dir(rel); d != "." {
				if err := root.MkdirAll(d, 0o755); err != nil {
					return res, err
				}
			}
			// os.Root refuses a path through a symlink that leaves dst;
			// removing an existing entry first means a symlink at the
			// name itself is replaced, never written through, and a file
			// hardlinked to one outside dst keeps its contents (sol-F1).
			if _, err := root.Lstat(rel); err == nil {
				if opts.Guarded {
					return res, fmt.Errorf("%s already exists; an agent's copy only adds files, so copy into a new folder", filepath.Join(dst, rel))
				}
				if err := root.Remove(rel); err != nil {
					return res, fmt.Errorf("%s is in the way: %v", filepath.Join(dst, rel), err)
				}
			}
			mode := fs.FileMode(0o644)
			if h.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return res, err
			}
			n, err := io.CopyN(f, tr, h.Size)
			cerr := f.Close()
			res.Bytes += n
			if err != nil {
				return res, err
			}
			if cerr != nil {
				return res, cerr
			}
			res.Files++
		default:
			res.Skipped++
		}
	}
}

// HasGitPart reports whether a slash or OS separated path has a .git
// component, in any case (macOS and Windows folders ignore case).
func HasGitPart(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}
