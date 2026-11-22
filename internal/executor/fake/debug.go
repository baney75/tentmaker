package fake

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

var _ core.Debugger = (*Executor)(nil)

// DebugCall is one DebugExec or DebugShell the fake ran.
type DebugCall struct {
	Name, Command string
	Spec          core.DebugSpec
}

// SetTests sets what TestResults returns for name.
func (e *Executor) SetTests(name string, t runs.TestResults) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tests == nil {
		e.tests = map[string]runs.TestResults{}
	}
	e.tests[name] = t
}

// TestResults implements dispatch.TestCollector: what SetTests gave, or
// no tests for a runner the fake knows.
func (e *Executor) TestResults(ctx context.Context, name string) (runs.TestResults, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.tests[name]; ok {
		return t, nil
	}
	if _, ok := e.runs[name]; !ok {
		return runs.TestResults{}, core.ErrNotFound
	}
	return runs.TestResults{}, nil
}

// SetKeepErr makes Keep fail, as an executor that can't keep a sandbox.
func (e *Executor) SetKeepErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keepErr = err
}

// SetDebugExec sets how DebugExec answers; nil answers "ran: <command>".
func (e *Executor) SetDebugExec(f func(name, command string) (core.DebugResult, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.debugExec = f
}

// DebugCalls lists the debug commands and shells run, oldest first.
func (e *Executor) DebugCalls() []DebugCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]DebugCall(nil), e.debugCalls...)
}

// Kept reports whether Keep ran for name and Clean has not since.
func (e *Executor) Kept(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kept[name]
}

// Keep implements core.Debugger for an exited runner.
func (e *Executor) Keep(ctx context.Context, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.record("Keep", name)
	if e.keepErr != nil {
		return e.keepErr
	}
	r, ok := e.runs[name]
	if !ok || r.State == core.ExecRunning {
		return fmt.Errorf("runner %s is not an exited job", name)
	}
	if e.kept == nil {
		e.kept = map[string]bool{}
	}
	e.kept[name] = true
	return nil
}

func (e *Executor) debuggable(name string) error {
	if !e.kept[name] {
		return errors.New("no kept sandbox for " + name)
	}
	return nil
}

// DebugExec implements core.Debugger.
func (e *Executor) DebugExec(ctx context.Context, name, command string, spec core.DebugSpec) (core.DebugResult, error) {
	e.mu.Lock()
	e.record("DebugExec", name)
	if err := e.debuggable(name); err != nil {
		e.mu.Unlock()
		return core.DebugResult{}, err
	}
	e.debugCalls = append(e.debugCalls, DebugCall{Name: name, Command: command, Spec: spec})
	f := e.debugExec
	e.mu.Unlock()
	if f != nil {
		return f(name, command)
	}
	return core.DebugResult{Output: []byte("ran: " + command + "\n")}, nil
}

// DebugShell implements core.Debugger: it writes a prompt line, then
// echoes stdin to stdout until stdin ends.
func (e *Executor) DebugShell(ctx context.Context, name string, spec core.DebugSpec, stdin io.Reader, stdout io.Writer) error {
	e.mu.Lock()
	e.record("DebugShell", name)
	if err := e.debuggable(name); err != nil {
		e.mu.Unlock()
		return err
	}
	e.debugCalls = append(e.debugCalls, DebugCall{Name: name, Spec: spec})
	e.mu.Unlock()
	if _, err := fmt.Fprintf(stdout, "fake shell in %s\n", name); err != nil {
		return err
	}
	_, err := io.Copy(stdout, stdin)
	return err
}
