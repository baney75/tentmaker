package fake

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/baney75/tentmaker/internal/core"
)

var _ core.Sandboxer = (*Executor)(nil)

// CopyInScript and the copy-out argv are what internal/sandbox runs to copy
// files on Linux and macOS guests; the fake plays them against an
// in-memory file tree per sandbox.
const CopyInScript = `mkdir -p -- "$1" && exec tar -x -f - -C "$1"`

// ExecFunc stands in for a command a test wants to control.
type ExecFunc func(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// SetExec makes Exec call f for every command (nil: the built-in shell).
func (e *Executor) SetExec(f ExecFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.execFn = f
}

// Files returns a copy of a sandbox's file tree: path -> content.
func (e *Executor) Files(name string) map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for k, v := range e.files[name] {
		out[k] = v
	}
	return out
}

// Execs lists the argv of every command run in a sandbox, in order.
func (e *Executor) Execs(name string) [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.execs[name]...)
}

// Exec implements core.Sandboxer. Without SetExec it knows echo, true,
// false, cat FILE, ls, "sh -c 'exit N'" and the copy commands; anything
// else exits 127.
func (e *Executor) Exec(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	e.mu.Lock()
	e.record("Exec", name)
	r := e.runs[name]
	if r == nil || r.State != core.ExecRunning {
		e.mu.Unlock()
		return -1, fmt.Errorf("sandbox %s is not running", name)
	}
	if r.Spec.Kind != core.KindSandbox {
		e.violations = append(e.violations, "exec in a runner that is not a sandbox: "+name)
		e.mu.Unlock()
		return -1, errors.New("not a sandbox")
	}
	if e.execs == nil {
		e.execs = map[string][][]string{}
	}
	e.execs[name] = append(e.execs[name], append([]string(nil), req.Argv...))
	f := e.execFn
	e.mu.Unlock()
	if f != nil {
		return f(ctx, name, req, stdin, stdout, stderr)
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	a := req.Argv
	switch {
	case len(a) == 0:
		return -1, errors.New("no command")
	case a[0] == "echo":
		fmt.Fprintln(stdout, strings.Join(a[1:], " "))
		return 0, nil
	case a[0] == "true":
		return 0, nil
	case a[0] == "false":
		return 1, nil
	case a[0] == "sh" && len(a) == 3 && a[1] == "-c" && strings.HasPrefix(a[2], "exit "):
		n, err := strconv.Atoi(strings.TrimPrefix(a[2], "exit "))
		if err != nil {
			return 2, nil
		}
		return n, nil
	case a[0] == "cat" && len(a) == 2:
		e.mu.Lock()
		body, ok := e.files[name][e.rel(req.Dir, a[1])]
		e.mu.Unlock()
		if !ok {
			fmt.Fprintf(stderr, "cat: %s: No such file or directory\n", a[1])
			return 1, nil
		}
		io.WriteString(stdout, body)
		return 0, nil
	case a[0] == "ls":
		e.mu.Lock()
		var names []string
		for k := range e.files[name] {
			names = append(names, k)
		}
		e.mu.Unlock()
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintln(stdout, n)
		}
		return 0, nil
	case len(a) == 5 && a[0] == "sh" && a[1] == "-c" && a[2] == CopyInScript:
		return e.untar(name, e.rel(req.Dir, a[4]), stdin, stderr)
	case len(a) == 7 && a[0] == "tar" && a[1] == "-c" && a[2] == "-f" && a[3] == "-" && a[4] == "-C":
		return e.tar(name, e.rel(req.Dir, a[5]), a[6], stdout, stderr)
	}
	fmt.Fprintf(stderr, "%s: command not found\n", a[0])
	return 127, nil
}

// rel turns a path in the sandbox into a key of its file tree.
func (e *Executor) rel(dir, p string) string {
	if !path.IsAbs(p) {
		p = path.Join(dir, p)
	}
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if p == "" {
		return "."
	}
	return p
}

func (e *Executor) untar(name, dst string, in io.Reader, stderr io.Writer) (int, error) {
	tr := tar.NewReader(in)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintln(stderr, "tar: "+err.Error())
			return 2, nil
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			fmt.Fprintln(stderr, "tar: "+err.Error())
			return 2, nil
		}
		got[e.rel(dst, h.Name)] = string(b)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.files == nil {
		e.files = map[string]map[string]string{}
	}
	if e.files[name] == nil {
		e.files[name] = map[string]string{}
	}
	for k, v := range got {
		e.files[name][k] = v
	}
	return 0, nil
}

func (e *Executor) tar(name, parent, base string, out, stderr io.Writer) (int, error) {
	root := e.rel(parent, base)
	e.mu.Lock()
	var keys []string
	for k := range e.files[name] {
		if k == root || root == "." || strings.HasPrefix(k, root+"/") {
			keys = append(keys, k)
		}
	}
	files := e.files[name]
	e.mu.Unlock()
	if len(keys) == 0 {
		fmt.Fprintf(stderr, "tar: %s: Cannot stat: No such file or directory\n", base)
		return 2, nil
	}
	sort.Strings(keys)
	tw := tar.NewWriter(out)
	prefix := parent
	if prefix == "." {
		prefix = ""
	}
	for _, k := range keys {
		n := strings.TrimPrefix(strings.TrimPrefix(k, prefix), "/")
		if root == "." {
			n = path.Join(base, k)
		}
		e.mu.Lock()
		body := files[k]
		e.mu.Unlock()
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return -1, err
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return -1, err
		}
	}
	return 0, tw.Close()
}
