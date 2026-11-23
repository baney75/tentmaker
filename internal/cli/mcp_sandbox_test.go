package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
)

// The controller refuses a timeout the tool should have refused, so a
// fraction can never truncate to 0 (no limit) on the way to the daemon.
func TestMCPSandboxExecRefusesFractionalTimeout(t *testing.T) {
	for _, bad := range []float64{0.5, -1, 1e300} {
		_, err := controller{}.SandboxExec(context.Background(), "sbx-0123456789ab", []string{"true"}, "", bad, "")
		if err == nil || !strings.Contains(err.Error(), "whole number") {
			t.Errorf("timeout %g: %v", bad, err)
		}
	}
}

// sandbox_exec returned up to 1 MiB of each stream, more than fits a
// model's context. It keeps the last 64 KiB and says how much it dropped.
func TestMCPSandboxExecKeepsTheTail(t *testing.T) {
	f := newFixture(t)
	serveSandboxes(t, f, control.Caller{Admin: true, ID: "0"})
	call := mcpSession(t, f)
	res, ok := call("sandbox_create", map[string]any{})
	if !ok {
		t.Fatalf("create: %s", mustJSON(res))
	}
	id := res["structuredContent"].(map[string]any)["id"].(string)
	const total = 300 << 10
	f.ex.SetExec(func(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		// Many small writes, as a chatty build makes, then the error last.
		line := strings.Repeat("x", 1023) + "\n"
		for i := 0; i < total/len(line)-1; i++ {
			io.WriteString(stdout, line)
		}
		io.WriteString(stdout, strings.Repeat("y", 1023)+"E")
		io.WriteString(stderr, "short")
		return 1, nil
	})
	res, ok = call("sandbox_exec", map[string]any{"id": id, "command": []any{"make"}})
	if !ok {
		t.Fatalf("exec: %s", mustJSON(res))
	}
	v := res["structuredContent"].(map[string]any)
	out, _ := v["stdout"].(string)
	if len(out) != mcpMaxOutput || !strings.HasSuffix(out, "E") {
		t.Fatalf("stdout is %d bytes ending %q, want the last %d", len(out), out[max(0, len(out)-8):], mcpMaxOutput)
	}
	if v["stdout_cut_bytes"] != float64(total-mcpMaxOutput) || v["stderr"] != "short" || v["stderr_cut_bytes"] != nil {
		t.Fatalf("cut %v, stderr %v %v", v["stdout_cut_bytes"], v["stderr"], v["stderr_cut_bytes"])
	}
	if v["exit_code"] != 1.0 || !strings.Contains(v["note"].(string), "64 KiB") {
		t.Fatalf("exit %v note %v", v["exit_code"], v["note"])
	}
	if b, _ := json.Marshal(res); len(b) > 2*mcpMaxOutput+16<<10 {
		t.Fatalf("the whole answer is %d bytes", len(b))
	}
}

func TestTailBufferKeepsWholeRunes(t *testing.T) {
	tb := &tailBuffer{max: 4}
	io.WriteString(tb, "aé") // é is 2 bytes; the last 4 of "aébcd" start in it
	io.WriteString(tb, "bcd")
	if tb.String() != "bcd" || tb.cut() != 3 {
		t.Fatalf("tail %q cut %d", tb.String(), tb.cut())
	}
}

// The client stops a copy out at MaxCopy too, whatever the daemon sends.
func TestCopyLimitFailsPastMaxCopy(t *testing.T) {
	var got strings.Builder
	cl := &copyLimit{w: &got, left: 5}
	if n, err := cl.Write([]byte("abcd")); n != 4 || err != nil {
		t.Fatalf("under the limit: %d %v", n, err)
	}
	if _, err := cl.Write([]byte("ef")); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("past the limit: %v", err)
	}
	if got.String() != "abcd" {
		t.Fatalf("passed %q", got.String())
	}
}
