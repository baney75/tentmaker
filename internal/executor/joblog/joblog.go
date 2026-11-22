// Package joblog reads a job's log from its job folder for run history and
// `tentmaker logs`: the actions/runner diag logs of a runner job, or
// check-run's log.txt of a check job.
//
// The folder belongs to the job, so everything is read through an os.Root
// (no symlink leaves the folder), only regular files are read, and they are
// opened with O_NONBLOCK so a FIFO the job planted can't hang the daemon.
package joblog

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/baney75/tentmaker/internal/core"
)

// Files is where a job folder's logs live, in the order they are joined:
// the runner's process log, then its job worker's log (step names and
// results), then check-run's output.
var Files = []struct {
	Dir, Prefix, Suffix string
}{
	{"runner/_diag", "Runner_", ".log"},
	{"runner/_diag", "Worker_", ".log"},
	{"out", "log.txt", ""},
}

// maxFiles caps how many diag files are read.
const maxFiles = 16

// Read returns the last max bytes of the logs under dir (a job folder) and
// their total size. A folder with no logs yet is an empty log, not an
// error; a missing folder is core.ErrNotFound.
func Read(dir string, max int64) (core.JobLog, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return core.JobLog{}, core.ErrNotFound
	}
	if err != nil {
		return core.JobLog{}, err
	}
	defer root.Close()
	var names []string
	for _, f := range Files {
		ents, err := readDir(root, f.Dir)
		if err != nil {
			continue
		}
		var match []string
		for _, e := range ents {
			n := e.Name()
			if strings.HasPrefix(n, f.Prefix) && strings.HasSuffix(n, f.Suffix) && e.Type().IsRegular() {
				match = append(match, path.Join(f.Dir, n))
			}
		}
		sort.Strings(match)
		names = append(names, match...)
	}
	if len(names) > maxFiles {
		names = names[len(names)-maxFiles:]
	}
	type part struct {
		f    *os.File
		size int64
	}
	var parts []part
	defer func() {
		for _, p := range parts {
			p.f.Close()
		}
	}()
	var total int64
	for _, n := range names {
		f, st, err := openRegular(root, n)
		if err != nil {
			continue
		}
		parts = append(parts, part{f, st.Size()})
		total += st.Size()
	}
	// Read the tail across the files: skip what lies before total-max.
	skip := total - max
	if skip < 0 {
		skip = 0
	}
	var out []byte
	for _, p := range parts {
		if skip >= p.size {
			skip -= p.size
			continue
		}
		b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(p.f, skip, p.size-skip), p.size-skip))
		skip = 0
		if err != nil {
			continue
		}
		out = append(out, b...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
	}
	if int64(len(out)) > max {
		out = out[int64(len(out))-max:]
	}
	return core.JobLog{Data: out, Size: total}, nil
}

func readDir(root *os.Root, dir string) ([]fs.DirEntry, error) {
	st, err := root.Lstat(dir)
	if err != nil || !st.IsDir() {
		return nil, errors.New("not a folder")
	}
	f, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(256)
}

func openRegular(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	st, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, errors.New(rel + " is not a regular file")
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|nonblock, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, errors.New(rel + " is not a regular file")
	}
	return f, st, nil
}
