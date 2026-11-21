package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FuzzServe feeds arbitrary lines to the server. Every request (a valid
// id, "jsonrpc":"2.0" and a method) gets exactly one response carrying the
// same id; every notification gets none; any other line gets one error
// whose id is null. A request the input also cancels may get none.
func FuzzServe(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"a","method":"tools/list"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"status"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"doctor"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"set_budget","arguments":{"everyday_memory":"4GiB","everyday_cpus":2}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"pause","arguments":[]}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"update"}}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":{"x":1},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1e999,"method":"ping"}`,
		`{"jsonrpc":"1.0","id":8,"method":"ping"}`,
		`{"id":9}`, `5`, `"x"`, `{`, "\r\n\r\n", `{"jsonrpc":"2.0","id":1.0,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","id":1,"method":"nope"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		want, cancelled := expectedResponses(data)
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := Serve(ctx, bytes.NewReader(data), &out, &fakeController{}, "1.2.3-fuzz"); err != nil && !strings.Contains(err.Error(), "reading input") {
			t.Fatalf("Serve: %v", err)
		}
		got := map[string]int{}
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			if line == "" {
				continue
			}
			var m msg
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("server wrote invalid JSON %q: %v", line, err)
			}
			if m.JSONRPC != "2.0" {
				t.Fatalf("response without jsonrpc 2.0: %s", line)
			}
			if (m.Error == nil) == (len(m.Result) == 0 || string(m.Result) == "null") {
				t.Fatalf("response needs exactly one of result and error: %s", line)
			}
			got[normalizeID(m.ID)]++
		}
		for id, n := range want {
			if g := got[id]; g > n || (g < n && !cancelled[id]) {
				t.Fatalf("id %s: %d responses, want %d\ninput:\n%s\noutput:\n%s", id, g, n, data, out.String())
			}
		}
		for id, g := range got {
			if want[id] == 0 {
				t.Fatalf("id %s: %d responses, want none\ninput:\n%s\noutput:\n%s", id, g, data, out.String())
			}
		}
	})
}

// expectedResponses works out, from the input alone, how many responses
// each normalized id should get, and which ids the input cancels.
func expectedResponses(data []byte) (map[string]int, map[string]bool) {
	want := map[string]int{}
	cancelled := map[string]bool{}
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := trimSpace(raw)
		if len(line) == 0 {
			continue
		}
		if line[0] == '[' {
			want["null"]++
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			want["null"]++
			continue
		}
		if len(req.ID) == 0 { // a notification
			if req.JSONRPC == "2.0" && req.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(req.Params, &p) == nil && len(p.RequestID) > 0 {
					cancelled[normalizeID(p.RequestID)] = true
				}
			}
			continue
		}
		var id any
		okID := json.Unmarshal(req.ID, &id) == nil
		switch id.(type) {
		case string, float64:
		default:
			okID = false
		}
		switch {
		case !okID:
			want["null"]++ // JSON-RPC: an id that can't be read is answered as null
		default:
			want[normalizeID(req.ID)]++
		}
	}
	return want, cancelled
}
