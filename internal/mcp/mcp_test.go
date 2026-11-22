package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeController struct {
	mu      sync.Mutex
	calls   []string
	failAll error
	block   chan struct{} // Update blocks on this until ctx ends
}

func (f *fakeController) rec(format string, a ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
	return f.failAll
}

func (f *fakeController) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeController) Status(ctx context.Context) (any, error) {
	if err := f.rec("status"); err != nil {
		return nil, err
	}
	return map[string]any{"profile": "boost", "slots": 2}, nil
}
func (f *fakeController) Pool(ctx context.Context) (any, error) {
	return []string{"a", "b"}, f.rec("pool") // a list: no structuredContent
}
func (f *fakeController) Doctor(ctx context.Context) (any, error) {
	panic("doctor exploded")
}
func (f *fakeController) SetBudget(ctx context.Context, em, bm string, ec, bc float64) error {
	return f.rec("set_budget %s %s %g %g", em, bm, ec, bc)
}
func (f *fakeController) AddRepo(ctx context.Context, repo, forge, mode string, allowPublic bool) error {
	return f.rec("add_repo %s %s %s %v", repo, forge, mode, allowPublic)
}
func (f *fakeController) RemoveRepo(ctx context.Context, repo string) error {
	return f.rec("remove_repo %s", repo)
}
func (f *fakeController) Pause(ctx context.Context, reason string) error {
	return f.rec("pause %q", reason)
}
func (f *fakeController) Resume(ctx context.Context) error { return f.rec("resume") }
func (f *fakeController) MigrateWorkflows(ctx context.Context, repoPath, platform string, write bool) (any, error) {
	return struct {
		Changed []string `json:"changed"`
	}{[]string{"ci.yml"}}, f.rec("migrate %s %s %v", repoPath, platform, write)
}
func (f *fakeController) InstallOnDevice(ctx context.Context, host, channel, memory, boost string) (any, error) {
	return nil, f.rec("install %s %s %s %s", host, channel, memory, boost)
}
func (f *fakeController) Runs(ctx context.Context, repo, sha string, failed bool, limit int) (any, error) {
	return map[string]any{"records": []any{}}, f.rec("runs %s %s %v %d", repo, sha, failed, limit)
}
func (f *fakeController) Logs(ctx context.Context, id string, lines int) (any, error) {
	return map[string]any{"id": id, "lines": []string{"x"}}, f.rec("logs %s %d", id, lines)
}
func (f *fakeController) Rerun(ctx context.Context, target string) (any, error) {
	return map[string]any{"broker": "box"}, f.rec("rerun %s", target)
}
func (f *fakeController) Stats(ctx context.Context, since, repo string) (any, error) {
	return map[string]any{"estimate": "estimated"}, f.rec("stats %s %s", since, repo)
}
func (f *fakeController) WaitForRun(ctx context.Context, repo, sha, runURL string, timeout time.Duration) (any, error) {
	return map[string]any{"done": true}, f.rec("wait %s %s %s %s", repo, sha, runURL, timeout)
}
func (f *fakeController) Tests(ctx context.Context, id string, failed bool) (any, error) {
	return map[string]any{"record": map[string]any{"id": id}}, f.rec("tests %s %v", id, failed)
}
func (f *fakeController) Flaky(ctx context.Context, repo, since string) (any, error) {
	return map[string]any{"tests": []any{}}, f.rec("flaky %s %s", repo, since)
}
func (f *fakeController) WhyFailed(ctx context.Context, id, repo, sha string, runID int64) (any, error) {
	return map[string]any{"conclusion": "failure"}, f.rec("why %s %s %s %d", id, repo, sha, runID)
}
func (f *fakeController) DebugExec(ctx context.Context, id, command string, timeoutS, maxBytes int64) (any, error) {
	return map[string]any{"exit_code": 1}, f.rec("debug_exec %s %q %d %d", id, command, timeoutS, maxBytes)
}
func (f *fakeController) Update(ctx context.Context, channel string) (any, error) {
	f.rec("update %s", channel)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]any{"version": "1.2.3"}, nil
}

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type session struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	done chan error
	msgs chan msg
}

func start(t *testing.T, c Controller) (*session, context.CancelFunc) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{t: t, in: inW, done: make(chan error, 1), msgs: make(chan msg, 16)}
	go func() {
		s.done <- Serve(ctx, inR, outW, c, "1.2.3-test")
		outW.Close()
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m msg
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("server wrote invalid JSON %q: %v", sc.Text(), err)
				continue
			}
			if m.JSONRPC != "2.0" {
				t.Errorf("response without jsonrpc 2.0: %s", sc.Text())
			}
			s.msgs <- m
		}
		close(s.msgs)
	}()
	t.Cleanup(func() { cancel(); inW.Close() })
	return s, cancel
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) recv() msg {
	s.t.Helper()
	select {
	case m, ok := <-s.msgs:
		if !ok {
			s.t.Fatal("server closed output")
		}
		return m
	case <-time.After(5 * time.Second):
		s.t.Fatal("no response within 5 s")
	}
	return msg{}
}

// call sends a request and returns its response.
func (s *session) call(id int, method string, params any) msg {
	s.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	s.send(string(b))
	m := s.recv()
	if string(m.ID) != fmt.Sprint(id) {
		s.t.Fatalf("response id %s, want %d", m.ID, id)
	}
	return m
}

type callResult struct {
	Content []struct {
		Type, Text string
	} `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError"`
}

func (s *session) tool(id int, name string, arguments any) (callResult, *rpcError) {
	s.t.Helper()
	m := s.call(id, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if m.Error != nil {
		return callResult{}, m.Error
	}
	var r callResult
	if err := json.Unmarshal(m.Result, &r); err != nil {
		s.t.Fatal(err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		s.t.Fatalf("content = %+v", r.Content)
	}
	return r, nil
}

func TestFullSession(t *testing.T) {
	fc := &fakeController{}
	s, _ := start(t, fc)

	m := s.call(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	if err := json.Unmarshal(m.Result, &init); err != nil || m.Error != nil {
		t.Fatalf("initialize: %v %+v", err, m.Error)
	}
	if init.ProtocolVersion != "2025-06-18" || init.ServerInfo.Name != "tentmaker" || init.ServerInfo.Version != "1.2.3-test" || init.Capabilities.Tools == nil {
		t.Fatalf("initialize result %s", m.Result)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	if m := s.call(2, "ping", nil); m.Error != nil || string(m.Result) != "{}" {
		t.Fatalf("ping: %s %+v", m.Result, m.Error)
	}

	m = s.call(3, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name, Description string
			InputSchema       struct {
				Type       string                    `json:"type"`
				Properties map[string]map[string]any `json:"properties"`
				Required   []string                  `json:"required"`
			} `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(m.Result, &list); err != nil {
		t.Fatal(err)
	}
	want := []string{"status", "pool", "doctor", "set_budget", "add_repo", "remove_repo", "pause", "resume", "migrate_workflows",
		"install_on_device", "update", "runs", "logs", "rerun", "stats", "wait_for_run", "tests", "flaky", "why_failed", "debug_exec"}
	if len(list.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(list.Tools), len(want))
	}
	byName := map[string]int{}
	for i, tl := range list.Tools {
		if tl.Name != want[i] {
			t.Errorf("tool %d = %s, want %s", i, tl.Name, want[i])
		}
		byName[tl.Name] = i
		if tl.InputSchema.Type != "object" || len(tl.Description) < 20 {
			t.Errorf("%s: schema type %q, description %q", tl.Name, tl.InputSchema.Type, tl.Description)
		}
		for _, k := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := tl.Annotations[k].(bool); !ok {
				t.Errorf("%s: annotation %s missing", tl.Name, k)
			}
		}
		if strings.ContainsAny(tl.Description, "\u2013\u2014") {
			t.Errorf("%s: description has a dash character", tl.Name)
		}
	}
	if list.Tools[byName["status"]].Annotations["readOnlyHint"] != true {
		t.Error("status should be read-only")
	}
	if a := list.Tools[byName["remove_repo"]].Annotations; a["destructiveHint"] != true || a["readOnlyHint"] != false {
		t.Errorf("remove_repo annotations %v", a)
	}
	if req := list.Tools[byName["add_repo"]].InputSchema.Required; len(req) != 1 || req[0] != "repo" {
		t.Errorf("add_repo required %v", req)
	}

	r, rerr := s.tool(4, "status", nil)
	if rerr != nil || r.IsError || r.StructuredContent["profile"] != "boost" || !strings.Contains(r.Content[0].Text, `"slots":2`) {
		t.Fatalf("status: %+v %+v", r, rerr)
	}
	r, _ = s.tool(5, "pool", map[string]any{})
	if r.IsError || r.StructuredContent != nil || r.Content[0].Text != `["a","b"]` {
		t.Fatalf("pool: %+v", r)
	}
	r, _ = s.tool(6, "set_budget", map[string]any{"everyday_memory": "4GiB", "boost_cpus": 6.5})
	if r.IsError || fc.last() != "set_budget 4GiB  0 6.5" || r.StructuredContent["ok"] != true {
		t.Fatalf("set_budget: %+v, call %q", r, fc.last())
	}
	s.tool(7, "add_repo", map[string]any{"repo": "o/r"})
	if fc.last() != "add_repo o/r github  false" {
		t.Errorf("add_repo defaults: %q", fc.last())
	}
	s.tool(8, "add_repo", map[string]any{"repo": "group/sub/r", "forge": "gitlab", "allow_public": true})
	if fc.last() != "add_repo group/sub/r gitlab  true" {
		t.Errorf("add_repo gitlab: %q", fc.last())
	}
	s.tool(9, "pause", map[string]any{"reason": "video call"})
	if fc.last() != `pause "video call"` {
		t.Errorf("pause: %q", fc.last())
	}
	r, _ = s.tool(10, "migrate_workflows", map[string]any{"repo_path": "/src/r"})
	if fc.last() != "migrate /src/r linux false" || r.StructuredContent["changed"] == nil {
		t.Errorf("migrate: %q %+v", fc.last(), r)
	}
	s.tool(11, "install_on_device", map[string]any{"host": "laptop", "memory": "4GiB"})
	if fc.last() != "install laptop stable 4GiB " {
		t.Errorf("install: %q", fc.last())
	}
	s.tool(12, "update", map[string]any{"channel": "dev"})
	s.tool(13, "remove_repo", map[string]any{"repo": "o/r"})
	s.tool(14, "resume", nil)
	if fc.last() != "resume" {
		t.Errorf("resume: %q", fc.last())
	}
	for _, rt := range []string{"runs", "logs", "stats", "wait_for_run"} {
		if list.Tools[byName[rt]].Annotations["readOnlyHint"] != true {
			t.Errorf("%s should be read-only", rt)
		}
	}
	if a := list.Tools[byName["rerun"]].Annotations; a["readOnlyHint"] != false || a["openWorldHint"] != true {
		t.Errorf("rerun annotations %v", a)
	}
	if req := list.Tools[byName["logs"]].InputSchema.Required; len(req) != 1 || req[0] != "job_id" {
		t.Errorf("logs required %v", req)
	}

	// The run history tools, with their defaults.
	run := func(id int, name string, args map[string]any, want string) {
		t.Helper()
		r, rerr := s.tool(id, name, args)
		if rerr != nil || r.IsError || fc.last() != want {
			t.Errorf("%s: %+v %+v, call %q, want %q", name, r, rerr, fc.last(), want)
		}
	}
	run(15, "runs", nil, "runs   false 20")
	run(16, "runs", map[string]any{"repo": "o/r", "sha": "abcdef1", "failed": true, "limit": 5}, "runs o/r abcdef1 true 5")
	run(17, "logs", map[string]any{"job_id": "tm-box-1"}, "logs tm-box-1 200")
	run(18, "logs", map[string]any{"job_id": "tm-box-1", "lines": 80}, "logs tm-box-1 80")
	run(19, "rerun", map[string]any{"target": "tm-box-1"}, "rerun tm-box-1")
	run(20, "rerun", map[string]any{"target": "https://github.com/o/r/actions/runs/9"}, "rerun https://github.com/o/r/actions/runs/9")
	run(21, "stats", nil, "stats 7d ")
	run(22, "stats", map[string]any{"since": "30d", "repo": "o/r"}, "stats 30d o/r")
	run(23, "wait_for_run", map[string]any{"repo": "o/r", "sha": "abcdef1234"}, "wait o/r abcdef1234  10m0s")
	run(24, "wait_for_run", map[string]any{"run_url": "https://github.com/o/r/actions/runs/9", "timeout_s": 60},
		"wait   https://github.com/o/r/actions/runs/9 1m0s")
	for i, bad := range []map[string]any{{}, {"repo": "o/r"}, {"sha": "abcdef1"},
		{"repo": "o/r", "sha": "abcdef1", "run_url": "https://github.com/o/r/actions/runs/9"}} {
		r, _ := s.tool(30+i, "wait_for_run", bad)
		if !r.IsError || !strings.Contains(r.Content[0].Text, "repo and sha, or run_url") {
			t.Errorf("wait_for_run %v: %+v", bad, r)
		}
	}

	// Test results, flaky tests, why a job failed and debug commands.
	for _, rt := range []string{"tests", "flaky", "why_failed"} {
		if list.Tools[byName[rt]].Annotations["readOnlyHint"] != true {
			t.Errorf("%s should be read-only", rt)
		}
	}
	if a := list.Tools[byName["debug_exec"]].Annotations; a["readOnlyHint"] != false || a["destructiveHint"] != true || a["idempotentHint"] != false {
		t.Errorf("debug_exec annotations %v", a)
	}
	if req := list.Tools[byName["debug_exec"]].InputSchema.Required; len(req) != 2 || req[0] != "id" || req[1] != "command" {
		t.Errorf("debug_exec required %v", req)
	}
	run(40, "tests", map[string]any{"id": "tm-box-1"}, "tests tm-box-1 true")
	run(41, "tests", map[string]any{"id": "tm-box-1", "failed": false}, "tests tm-box-1 false")
	run(42, "flaky", nil, "flaky  14d")
	run(43, "flaky", map[string]any{"repo": "o/r", "since": "30d"}, "flaky o/r 30d")
	run(44, "why_failed", map[string]any{"id": "tm-box-1"}, "why tm-box-1   0")
	run(45, "why_failed", map[string]any{"repo": "o/r", "sha": "abcdef1"}, "why  o/r abcdef1 0")
	run(46, "why_failed", map[string]any{"repo": "o/r", "run_id": 9}, "why  o/r  9")
	run(47, "debug_exec", map[string]any{"id": "tm-box-1", "command": "go test ./..."}, `debug_exec tm-box-1 "go test ./..." 60 65536`)
	run(48, "debug_exec", map[string]any{"id": "tm-box-1", "command": "ls", "timeout_s": 600, "max_bytes": 1 << 20}, `debug_exec tm-box-1 "ls" 600 1048576`)
	for i, bad := range []map[string]any{{}, {"repo": "o/r"}, {"sha": "abcdef1"}, {"repo": "o/r", "sha": "abcdef1", "run_id": 9},
		{"id": "tm-box-1", "repo": "o/r"}, {"id": "tm-box-1", "run_id": 9}} {
		r, _ := s.tool(50+i, "why_failed", bad)
		if !r.IsError || !strings.Contains(r.Content[0].Text, "id alone, or repo with sha or run_id") {
			t.Errorf("why_failed %v: %+v", bad, r)
		}
	}

	s.in.Close()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("Serve after EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}

func TestBadRequests(t *testing.T) {
	s, _ := start(t, &fakeController{})
	cases := []struct {
		line string
		code int
	}{
		{`{not json`, codeParse},
		{`{"id":1,"method":"ping"}`, codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":{"x":1},"method":"ping"}`, codeInvalidRequest},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, codeInvalidRequest},
		{`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`, codeMethodNotFound},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rm_rf","arguments":{}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":4,"method":"tools/call"}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"add_repo","arguments":{}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"add_repo","arguments":{"repo":5}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"add_repo","arguments":{"repo":"noslash"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"add_repo","arguments":{"repo":"o/r","forge":"svn"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"status","arguments":{"verbose":true}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"set_budget","arguments":{"everyday_memory":"lots"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"install_on_device","arguments":{"host":"-oProxyCommand=x"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"update","arguments":{"channel":"beta"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"status","arguments":[1]}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"logs","arguments":{}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"logs","arguments":{"job_id":"../../etc/passwd"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"logs","arguments":{"job_id":"tm-a-1","lines":0}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"runs","arguments":{"limit":2.5}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"runs","arguments":{"sha":"zzz"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"rerun","arguments":{"target":"https://example.com/x"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"stats","arguments":{"since":"forever"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"wait_for_run","arguments":{"run_url":"https://github.com/o/r/pull/1"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"tests","arguments":{}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"why_failed","arguments":{"id":"../x"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"debug_exec","arguments":{"id":"tm-a-1"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"debug_exec","arguments":{"id":"tm-a-1","command":"  "}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"debug_exec","arguments":{"id":"tm-a-1","command":"ls","timeout_s":601}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":28,"method":"tools/call","params":{"name":"debug_exec","arguments":{"id":"tm-a-1","command":"ls","max_bytes":2097152}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":29,"method":"tools/call","params":{"name":"flaky","arguments":{"since":"forever"}}}`, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"wait_for_run","arguments":{"repo":"o/r","sha":"abcdef1","timeout_s":86400}}}`, codeInvalidParams},
	}
	for _, tc := range cases {
		s.send(tc.line)
		m := s.recv()
		if m.Error == nil || m.Error.Code != tc.code {
			t.Errorf("%s: got %+v, want code %d", tc.line, m.Error, tc.code)
		}
		if m.Error != nil && m.Error.Message == "" {
			t.Errorf("%s: empty error message", tc.line)
		}
	}
	// Unknown notifications get no reply; the next request still works.
	s.send(`{"jsonrpc":"2.0","method":"notifications/whatever"}`)
	s.send(`   `)
	if m := s.call(99, "ping", nil); m.Error != nil {
		t.Fatalf("ping after bad input: %+v", m.Error)
	}
}

func TestControllerErrorIsToolError(t *testing.T) {
	fc := &fakeController{failAll: errors.New("budget file is read-only: run tentmaker as root")}
	s, _ := start(t, fc)
	r, rerr := s.tool(1, "set_budget", map[string]any{"boost_memory": "8GiB"})
	if rerr != nil {
		t.Fatalf("controller error became a protocol error: %+v", rerr)
	}
	if !r.IsError || !strings.Contains(r.Content[0].Text, "read-only") || r.StructuredContent["error"] == nil {
		t.Fatalf("got %+v", r)
	}
	// A tool-level validation failure (nothing to change) is also isError.
	r, _ = s.tool(2, "set_budget", map[string]any{})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "nothing to change") {
		t.Fatalf("empty set_budget: %+v", r)
	}
	// A panic in the controller is reported, not fatal.
	r, rerr = s.tool(3, "doctor", nil)
	if rerr != nil || !r.IsError || !strings.Contains(r.Content[0].Text, "doctor exploded") {
		t.Fatalf("panic: %+v %+v", r, rerr)
	}
	if m := s.call(4, "ping", nil); m.Error != nil {
		t.Fatal("server died after a controller panic")
	}
}

func TestConcurrentAndCancel(t *testing.T) {
	fc := &fakeController{block: make(chan struct{})}
	s, _ := start(t, fc)
	s.send(`{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{"name":"update","arguments":{}}}`)
	// A ping is answered while the update runs.
	if m := s.call(2, "ping", nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	// Cancel only once the update is running: a cancel that wins the race
	// stops the call before it starts, which is right but not this test.
	for deadline := time.Now().Add(5 * time.Second); fc.last() == "" && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"slow","reason":"user"}}`)
	// The canceled call sends nothing; the next response is the ping.
	if m := s.call(3, "ping", nil); string(m.ID) != "3" {
		t.Fatalf("got a response for %s", m.ID)
	}
	if fc.last() != "update " {
		t.Errorf("update called with %q", fc.last())
	}
}

func TestServeContextCancel(t *testing.T) {
	s, cancel := start(t, &fakeController{})
	cancel()
	select {
	case err := <-s.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve ignored ctx cancel")
	}
}
