// Package mcp is a minimal Model Context Protocol server over stdio, so an
// agent can run Tentmaker the way a person runs the CLI.
//
// Transport: JSON-RPC 2.0, one message per line, on stdin and stdout.
// Protocol version 2025-06-18. Methods: initialize, ping, tools/list,
// tools/call, and the notifications/initialized and notifications/cancelled
// notifications. Every tool maps to one Controller method.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// maxLine caps one incoming message.
const maxLine = 8 << 20

// Controller is what the tools drive. The CLI implements it.
//
// SetBudget: an empty memory string or a zero CPU count leaves that value
// unchanged.
type Controller interface {
	Status(ctx context.Context) (any, error)
	Pool(ctx context.Context) (any, error)
	Doctor(ctx context.Context) (any, error)
	SetBudget(ctx context.Context, everydayMemory, boostMemory string, everydayCPUs, boostCPUs float64) error
	AddRepo(ctx context.Context, repo, forge, mode string, allowPublic bool) error
	RemoveRepo(ctx context.Context, repo string) error
	Pause(ctx context.Context, reason string) error
	Resume(ctx context.Context) error
	MigrateWorkflows(ctx context.Context, repoPath, platform string, write bool) (any, error)
	InstallOnDevice(ctx context.Context, host, channel, memory, boostMemory string) (any, error)
	Update(ctx context.Context, channel string) (any, error)
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type server struct {
	c       Controller
	version string

	wmu sync.Mutex
	enc *json.Encoder

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	wg       sync.WaitGroup
}

// Serve reads requests from in and writes responses to out until in ends
// (it then waits for running tool calls) or ctx is canceled. Requests run
// concurrently, so a ping is answered while a long tool call works.
func Serve(ctx context.Context, in io.Reader, out io.Writer, c Controller, version string) error {
	s := &server{c: c, version: version, enc: json.NewEncoder(out), inflight: map[string]context.CancelFunc{}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return ctx.Err()
		case err := <-readErr:
			s.wg.Wait()
			if err != nil {
				return fmt.Errorf("mcp: reading input: %w", err)
			}
			return nil
		case line := <-lines:
			s.handleLine(ctx, line)
		}
	}
}

func (s *server) write(r response) {
	r.JSONRPC = "2.0"
	if r.ID == nil {
		r.ID = json.RawMessage("null")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.enc.Encode(r) // Encode adds the newline; a write error means the client is gone
}

func (s *server) fail(id json.RawMessage, code int, msg string) {
	s.write(response{ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *server) handleLine(ctx context.Context, line []byte) {
	trimmed := trimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	if trimmed[0] == '[' {
		s.fail(nil, codeInvalidRequest, "batches are not supported in MCP 2025-06-18; send one message per line")
		return
	}
	var req request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		s.fail(nil, codeParse, "parse error: "+err.Error())
		return
	}
	isNotification := len(req.ID) == 0
	if req.JSONRPC != "2.0" || req.Method == "" || (!isNotification && !validID(req.ID)) {
		if isNotification {
			return
		}
		id := req.ID
		if !validID(id) {
			// JSON-RPC 2.0: an id that can't be read (an object, null,
			// 1e999) is answered as null, never echoed.
			id = nil
		}
		s.fail(id, codeInvalidRequest, `invalid request: need "jsonrpc":"2.0", a string or number "id" and a "method"`)
		return
	}
	if isNotification {
		s.notification(req)
		return
	}
	switch req.Method {
	case "initialize":
		s.write(response{ID: req.ID, Result: s.initialize()})
	case "ping":
		s.write(response{ID: req.ID, Result: struct{}{}})
	case "tools/list":
		s.write(response{ID: req.ID, Result: map[string]any{"tools": toolList()}})
	case "tools/call":
		key := normalizeID(req.ID)
		cctx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.inflight[key] = cancel
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.inflight, key)
				s.mu.Unlock()
				cancel()
			}()
			res, rerr := s.call(cctx, req.Params)
			if cctx.Err() != nil && ctx.Err() == nil {
				return // canceled by the client: MCP says send nothing
			}
			if rerr != nil {
				s.write(response{ID: req.ID, Error: rerr})
				return
			}
			s.write(response{ID: req.ID, Result: res})
		}()
	default:
		s.fail(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *server) notification(req request) {
	if req.Method != "notifications/cancelled" {
		return // notifications/initialized and anything else need no reply
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(req.Params, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	s.mu.Lock()
	cancel := s.inflight[normalizeID(p.RequestID)]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *server) initialize() map[string]any {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "tentmaker", "title": "Tentmaker", "version": s.version},
		"instructions": "Tentmaker runs CI jobs on this device and its pool in the spare memory and CPU the owner gave it. " +
			"Start with status (this device) or pool (every device). Run doctor when jobs don't start. " +
			"Changes take effect without a restart. Tools that change state say so in their annotations.",
	}
}

// call runs one tools/call. Unknown tools and bad arguments are protocol
// errors; a Controller failure is a tool result with isError so the agent
// can read it and try something else.
func (s *server) call(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil || p.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: `tools/call needs params {"name": ..., "arguments": {...}}`}
	}
	t, ok := toolByName(p.Name)
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name + "; call tools/list for the tool names"}
	}
	args := map[string]any{}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "arguments must be a JSON object"}
		}
	}
	a, err := t.check(args)
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: p.Name + ": " + err.Error()}
	}
	out, err := safeRun(ctx, t, s.c, a)
	if err != nil {
		return toolResult(map[string]any{"error": err.Error()}, err.Error(), true), nil
	}
	if out == nil {
		out = map[string]any{"ok": true}
	}
	text, jerr := json.Marshal(out)
	if jerr != nil {
		return nil, &rpcError{Code: codeInternal, Message: "can't encode the result: " + jerr.Error()}
	}
	return toolResult(out, string(text), false), nil
}

// safeRun turns a Controller panic into a tool error instead of killing the
// server.
func safeRun(ctx context.Context, t tool, c Controller, a args) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("internal error in %s: %v", t.Name, r)
		}
	}()
	return t.run(ctx, c, a)
}

func toolResult(structured any, text string, isError bool) map[string]any {
	r := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
	// structuredContent must be an object.
	if m, ok := asObject(structured); ok {
		r["structuredContent"] = m
	}
	return r
}

func asObject(v any) (map[string]any, bool) {
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 || b[0] != '{' {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	return m, true
}

func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	return b
}
