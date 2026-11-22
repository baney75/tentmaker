// Package testresults finds the tests a job ran: JUnit XML reports the job
// left in its workspace, and `go test -json` events in its log.
//
// The job folder belongs to the job, and untrusted CI code writes it. So
// Collect reads everything through an os.Root (nothing leaves the folder),
// never follows a symlink, reads only regular files opened with O_NONBLOCK
// (a planted FIFO can't hang the daemon), and bounds how many entries it
// walks, how deep it goes, how many reports it reads and how many bytes.
package testresults

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/baney75/tentmaker/internal/executor/joblog"
	"github.com/baney75/tentmaker/internal/runs"
)

// Limits bound one job's scan. Zero fields take the defaults below.
type Limits struct {
	MaxFiles     int   // reports read, default 200
	MaxFileBytes int64 // one report, default 16 MiB; a larger one is skipped with a Problem
	MaxTotal     int64 // all reports together, default 64 MiB
	MaxEntries   int   // directory entries walked, default 50000
	MaxDepth     int   // folder depth below each root, default 16
}

// Defaults for Limits.
const (
	DefaultMaxFiles     = 200
	DefaultMaxFileBytes = 16 << 20
	DefaultMaxTotal     = 64 << 20
	DefaultMaxEntries   = 50000
	DefaultMaxDepth     = 16
)

func (l Limits) withDefaults() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxTotal <= 0 {
		l.MaxTotal = DefaultMaxTotal
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxDepth
	}
	return l
}

// sniffSize is how much of a candidate report is read to decide whether it
// is JUnit XML.
const sniffSize = 4 << 10

// listBatch is how many entries one directory listing call reads.
const listBatch = 256

// skipDirs are folders that hold tools, caches and dependencies, never a
// job's own test reports.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"_actions":     true,
	"_tool":        true,
	".cache":       true,
	"vendor":       true,
	"toolcache":    true,
	"home":         true,
}

// reportDirs are folders where any .xml file is a candidate report.
var reportDirs = map[string]bool{
	"test-results":     true,
	"test-reports":     true,
	"surefire-reports": true,
	"failsafe-reports": true,
	"junit":            true,
	"reports":          true,
}

// Collect walks the given sub-folders of dir (a job folder that belongs to
// the job: "runner/_work" for a runner job, "work" for a check job; a missing
// one is skipped) and parses every JUnit XML report it finds.
func Collect(dir string, subdirs []string, lim Limits) runs.TestResults {
	lim = lim.withDefaults()
	var res runs.TestResults
	root, err := os.OpenRoot(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			res.Problems = append(res.Problems, "could not open the job folder to look for test reports")
		}
		return res.Capped()
	}
	defer root.Close()

	w := &walker{root: root, lim: lim}
	for _, sub := range subdirs {
		sub = path.Clean(filepath.ToSlash(sub))
		if !filepath.IsLocal(filepath.FromSlash(sub)) {
			w.problem(fmt.Sprintf("skipped folder %s: it is not inside the job folder", sub))
			continue
		}
		st, err := root.Lstat(filepath.FromSlash(sub))
		if err != nil || !st.IsDir() {
			continue
		}
		w.walk(sub, 0, false)
		if w.stopped {
			break
		}
	}
	if w.deep {
		w.problem(fmt.Sprintf("skipped folders more than %d levels deep", lim.MaxDepth))
	}

	var total int64
	var tooMany int
	for _, rel := range w.candidates {
		cases, ok := w.read(rel, &total, len(res.Files), &tooMany)
		if !ok {
			continue
		}
		for i := range cases {
			cases[i].File = rel
			cases[i].Source = runs.SourceJUnit
		}
		res.Cases = append(res.Cases, cases...)
		res.Files = append(res.Files, rel)
	}
	if tooMany > 0 {
		w.problem(fmt.Sprintf("skipped %d test reports past the %d file limit", tooMany, lim.MaxFiles))
	}
	res.Problems = append(res.Problems, w.problems...)
	return res.Capped()
}

// walker holds one scan's state.
type walker struct {
	root       *os.Root
	lim        Limits
	entries    int
	stopped    bool // MaxEntries was hit
	deep       bool // a folder past MaxDepth was skipped
	candidates []string
	problems   []string
}

func (w *walker) problem(s string) { w.problems = append(w.problems, s) }

// walk lists rel (a folder, slash separated, relative to the root) and
// descends into its plain sub-folders. inReports says rel is under one of
// reportDirs.
func (w *walker) walk(rel string, depth int, inReports bool) {
	if w.stopped {
		return
	}
	f, err := w.root.Open(filepath.FromSlash(rel))
	if err != nil {
		return
	}
	st, err := f.Stat()
	if err != nil || !st.IsDir() {
		f.Close()
		return
	}
	var ents []fs.DirEntry
	for {
		batch, err := f.ReadDir(listBatch)
		if w.entries+len(batch) > w.lim.MaxEntries {
			batch = batch[:w.lim.MaxEntries-w.entries]
			w.stopped = true
		}
		w.entries += len(batch)
		ents = append(ents, batch...)
		if err != nil || len(batch) == 0 || w.stopped {
			break
		}
	}
	f.Close()
	if w.stopped {
		w.problem(fmt.Sprintf("stopped looking for test reports after %d folder entries", w.lim.MaxEntries))
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })

	for _, e := range ents {
		name := e.Name()
		child := path.Join(rel, name)
		// The entry's type comes from the listing, which does not follow
		// symlinks: a symlink, FIFO, socket or device is neither of these.
		switch t := e.Type(); {
		case t.IsDir():
			if skipDirs[name] || (name == "_runner_file_commands" && path.Base(rel) == "_temp") {
				continue
			}
			if depth+1 > w.lim.MaxDepth {
				w.deep = true
				continue
			}
			w.walk(child, depth+1, inReports || reportDirs[name])
			if w.stopped {
				// Keep the reports found so far; the walk is over.
				return
			}
		case t.IsRegular():
			if isCandidate(name, inReports) {
				w.candidates = append(w.candidates, child)
			}
		}
	}
}

// read opens one candidate report, checks it is JUnit XML and within the
// limits, and parses it. ok is false when the file is skipped.
func (w *walker) read(rel string, total *int64, read int, tooMany *int) (cases []runs.TestCase, ok bool) {
	f, st, err := joblog.OpenRegular(w.root, filepath.FromSlash(rel))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	head := make([]byte, sniffSize)
	k, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, false
	}
	head = head[:k]
	if !looksLikeJUnit(head) {
		return nil, false
	}
	if read >= w.lim.MaxFiles {
		*tooMany++
		return nil, false
	}
	if st.Size() > w.lim.MaxFileBytes {
		w.problem(fmt.Sprintf("skipped %s: %s is over the %s limit", rel, sizeText(st.Size()), sizeText(w.lim.MaxFileBytes)))
		return nil, false
	}
	if *total+st.Size() > w.lim.MaxTotal {
		w.problem(fmt.Sprintf("skipped %s: the reports together are over the %s limit", rel, sizeText(w.lim.MaxTotal)))
		return nil, false
	}
	// The file may still grow after the Stat, so the read is capped too.
	rest, err := io.ReadAll(io.LimitReader(f, w.lim.MaxFileBytes-int64(k)+1))
	if err != nil {
		return nil, false
	}
	data := append(head, rest...)
	n := int64(len(data))
	if n > w.lim.MaxFileBytes {
		w.problem(fmt.Sprintf("skipped %s: it is over the %s limit", rel, sizeText(w.lim.MaxFileBytes)))
		return nil, false
	}
	if *total+n > w.lim.MaxTotal {
		w.problem(fmt.Sprintf("skipped %s: the reports together are over the %s limit", rel, sizeText(w.lim.MaxTotal)))
		return nil, false
	}
	*total += n
	cases, err = ParseJUnit(bytes.NewReader(data), rel)
	if err != nil {
		w.problem(fmt.Sprintf("%s does not parse as JUnit XML", rel))
		return nil, false
	}
	return cases, true
}

// isCandidate reports whether a regular file named name may be a JUnit
// report, by its name or because it sits under a reports folder.
func isCandidate(name string, inReports bool) bool {
	low := strings.ToLower(name)
	if !strings.HasSuffix(low, ".xml") {
		return false
	}
	if inReports {
		return true
	}
	stem := strings.TrimSuffix(low, ".xml")
	switch {
	case strings.HasPrefix(low, "test-"),
		strings.Contains(stem, "junit"),
		strings.HasSuffix(stem, "tests"),
		low == "report.xml",
		low == "results.xml":
		return true
	}
	if i := strings.Index(stem, "test"); i >= 0 && strings.Contains(stem[i+len("test"):], "result") {
		return true
	}
	return false
}

// looksLikeJUnit reports whether head (the start of a file) opens with a
// <testsuites> or <testsuite> element, after an optional byte order mark,
// XML declaration, processing instructions, comments and white space.
func looksLikeJUnit(head []byte) bool {
	b := bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	for {
		b = bytes.TrimLeft(b, " \t\r\n")
		switch {
		case bytes.HasPrefix(b, []byte("<?")):
			i := bytes.Index(b, []byte("?>"))
			if i < 0 {
				return false
			}
			b = b[i+2:]
		case bytes.HasPrefix(b, []byte("<!--")):
			i := bytes.Index(b[4:], []byte("-->"))
			if i < 0 {
				return false
			}
			b = b[4+i+3:]
		default:
			if !bytes.HasPrefix(b, []byte("<testsuite")) {
				return false
			}
			b = b[len("<testsuite"):]
			b = bytes.TrimPrefix(b, []byte("s"))
			if len(b) == 0 {
				return false
			}
			switch b[0] {
			case ' ', '\t', '\r', '\n', '>', '/':
				return true
			}
			return false
		}
	}
}

// sizeText says n bytes in MiB, in whole KiB, or else in bytes.
func sizeText(n int64) string {
	const kib, mib = 1 << 10, 1 << 20
	switch {
	case n >= mib && n%mib == 0:
		return fmt.Sprintf("%d MiB", n/mib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib && n%kib == 0:
		return fmt.Sprintf("%d KiB", n/kib)
	}
	return fmt.Sprintf("%d bytes", n)
}
