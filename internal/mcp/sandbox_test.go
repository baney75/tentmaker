package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type fakeSandboxes struct{ fakeController }

func (f *fakeSandboxes) SandboxCreate(ctx context.Context, os, device string, cpus float64, memory, ttl, copyDir string) (any, error) {
	return map[string]any{"id": "sbx-0123456789ab"}, f.rec("create %s %q %g %q %s %q", os, device, cpus, memory, ttl, copyDir)
}
func (f *fakeSandboxes) SandboxExec(ctx context.Context, id string, argv []string, dir string, timeoutS float64, stdin string) (any, error) {
	b, _ := json.Marshal(argv)
	return map[string]any{"exit_code": 3}, f.rec("exec %s %s %q %g %q", id, b, dir, timeoutS, stdin)
}
func (f *fakeSandboxes) SandboxCopy(ctx context.Context, from, to string) (any, error) {
	return map[string]any{"bytes": 1}, f.rec("cp %s %s", from, to)
}
func (f *fakeSandboxes) SandboxList(ctx context.Context) (any, error) {
	return map[string]any{"sandboxes": []any{}}, f.rec("ls")
}
func (f *fakeSandboxes) SandboxRemove(ctx context.Context, id string) (any, error) {
	return map[string]any{"removed": []string{id}}, f.rec("rm %s", id)
}

func TestSandboxTools(t *testing.T) {
	fc := &fakeSandboxes{}
	s, _ := start(t, fc)
	id := 1
	call := func(name string, args map[string]any) (callResult, *rpcError) {
		id++
		return s.tool(id, name, args)
	}
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"sandbox_create", map[string]any{}, `create linux "" 0 "" 30m ""`},
		{"sandbox_create", map[string]any{"os": "linux", "cpus": 2.0, "memory": "4GiB", "ttl": "2h", "copy_dir": "/src"}, `create linux "" 2 "4GiB" 2h "/src"`},
		{"sandbox_exec", map[string]any{"id": "sbx-0123456789ab", "command": []any{"sh", "-c", "echo $HOME"}, "stdin": "x"},
			`exec sbx-0123456789ab ["sh","-c","echo $HOME"] "" 0 "x"`},
		{"sandbox_cp", map[string]any{"from": ".", "to": "sbx-0123456789ab:src"}, "cp . sbx-0123456789ab:src"},
		{"sandbox_ls", map[string]any{}, "ls"},
		{"sandbox_rm", map[string]any{"id": "sbx-0123456789ab"}, "rm sbx-0123456789ab"},
	} {
		r, rerr := call(c.name, c.args)
		if rerr != nil || r.IsError {
			t.Fatalf("%s: %+v %+v", c.name, rerr, r)
		}
		if got := fc.last(); got != c.want {
			t.Errorf("%s: called %s, want %s", c.name, got, c.want)
		}
	}
	// The exit code is a result, not a tool error.
	r, _ := call("sandbox_exec", map[string]any{"id": "sbx-0123456789ab", "command": []any{"false"}})
	if r.IsError || r.StructuredContent["exit_code"] != 3.0 {
		t.Errorf("exec result %+v", r)
	}

	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"sandbox_exec", map[string]any{"id": "sbx-0123456789ab", "command": "ls -la"}, "command must be a array"},
		{"sandbox_exec", map[string]any{"id": "sbx-0123456789ab", "command": []any{"ls", 3.0}}, "command must be a array"},
		{"sandbox_exec", map[string]any{"id": "sbx-0123456789ab", "command": []any{}}, "give the command"},
		{"sandbox_exec", map[string]any{"id": "../etc", "command": []any{"ls"}}, "not a sandbox ID"},
		{"sandbox_create", map[string]any{"ttl": "forever"}, "not a duration"},
		{"sandbox_create", map[string]any{"os": "plan9"}, "os must be one of"},
		{"sandbox_rm", map[string]any{}, "id is required"},
	} {
		before := fc.last()
		_, rerr := call(c.name, c.args)
		if rerr == nil || !strings.Contains(rerr.Message, c.want) {
			t.Errorf("%s %v: error %+v, want %q", c.name, c.args, rerr, c.want)
		}
		if fc.last() != before {
			t.Errorf("%s ran on bad arguments", c.name)
		}
	}
}

func TestSandboxToolsWithoutController(t *testing.T) {
	s, _ := start(t, &fakeController{})
	r, rerr := s.tool(2, "sandbox_ls", map[string]any{})
	if rerr != nil || !r.IsError || !strings.Contains(r.Content[0].Text, "can't run sandboxes") {
		t.Fatalf("got %+v %+v", r, rerr)
	}
}

func TestSandboxExecSchemaIsAnArrayOfStrings(t *testing.T) {
	for _, tl := range toolList() {
		if tl["name"] != "sandbox_exec" {
			continue
		}
		props := tl["inputSchema"].(map[string]any)["properties"].(map[string]any)
		cmd := props["command"].(map[string]any)
		if cmd["type"] != "array" || fmt.Sprint(cmd["items"]) != "map[type:string]" {
			t.Fatalf("command schema %v", cmd)
		}
		return
	}
	t.Fatal("no sandbox_exec tool")
}

// A sandbox id goes into a /v1/sandboxes/{id} URL, so only the exact shape
// the executor accepts passes: anything else could name another route.
func TestSandboxToolsRefuseInexactIDs(t *testing.T) {
	fc := &fakeSandboxes{}
	s, _ := start(t, fc)
	for i, id := range []string{
		"sbx-../../v1/pause",
		"sbx-0123456789ab/exec",
		"sbx-0123456789AB",
		"sbx-0123456789abc",
		"sbx-0123456789a",
		"sbx-",
		"sbx-0123456789ab\n",
		"sbx-0123456789ab?x=1",
		"tm-0123456789ab",
	} {
		for j, tl := range []string{"sandbox_exec", "sandbox_rm"} {
			a := map[string]any{"id": id}
			if tl == "sandbox_exec" {
				a["command"] = []any{"true"}
			}
			_, rerr := s.tool(10+2*i+j, tl, a)
			if rerr == nil || !strings.Contains(rerr.Message, "not a sandbox ID") {
				t.Errorf("%s %q: error %+v, want a refusal", tl, id, rerr)
			}
		}
	}
	if fc.last() != "" {
		t.Fatalf("a tool ran with a bad id: %s", fc.last())
	}
}
