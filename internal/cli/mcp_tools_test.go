package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// mcpSession runs `tentmaker mcp` on the fixture over pipes and returns a
// tool caller. The session ends when the test does.
func mcpSession(t *testing.T, f *fixture, flags ...string) func(name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f.env.Stdin = inR
	f.env.Stdout = outW
	done := make(chan struct{})
	go func() {
		Run(f.env, append([]string{"mcp"}, flags...))
		outW.Close()
		close(done)
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("mcp did not end at EOF")
		}
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	id := 0
	send := func(method string, params any) map[string]any {
		t.Helper()
		id++
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		if !sc.Scan() {
			t.Fatalf("no answer to %s: %v", method, sc.Err())
		}
		var resp map[string]any
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			t.Fatalf("%s: not JSON: %s", method, sc.Bytes())
		}
		return resp
	}
	send("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	return func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		r := send("tools/call", map[string]any{"name": name, "arguments": args})
		res, _ := r["result"].(map[string]any)
		return res, res != nil && res["isError"] != true
	}
}

// add_repo for Forgejo with no mode gets checks mode, as the CLI does,
// instead of failing on a "runner" the tool filled in (review r1 partB
// first-F3).
func TestMCPAddRepoForgejoDefaultsToChecks(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	f.isRoot = true
	call := mcpSession(t, f)
	res, ok := call("add_repo", map[string]any{"repo": "o/n", "forge": "forgejo"})
	if !ok {
		t.Fatalf("add_repo forgejo: %s", mustJSON(res))
	}
	if r := f.config().Repos; len(r) != 1 || r[0].Mode != "checks" || r[0].Forge != "forgejo" {
		t.Fatalf("repos %+v", r)
	}
}

// add_repo with allow_public sets the fork approval policy the tool
// description promises (review r1 partB first-F4).
func TestMCPAddRepoAllowPublicSetsForkApproval(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	f.isRoot = true
	f.store.Set("github-token", []byte(canary))
	f.forge.SetRepo(core.RepoInfo{Repo: "o/pub", Public: true, DefaultBranch: "main", ForkPolicy: "approve_first_time"})
	ff := &forkForge{Forge: f.forge}
	f.env.NewForge = func(name string, fc config.Forge, tok func(context.Context) (string, error), log *slog.Logger) (core.Forge, core.StatusForge, error) {
		return ff, f.forge, nil
	}
	call := mcpSession(t, f)
	res, ok := call("add_repo", map[string]any{"repo": "o/pub", "allow_public": true})
	if !ok {
		t.Fatalf("add_repo: %s", mustJSON(res))
	}
	if !reflect.DeepEqual(ff.set, []string{"o/pub"}) {
		t.Fatalf("fork approval set on %v, want [o/pub]", ff.set)
	}
	if strings.Contains(f.stderr.String(), canary) {
		t.Error("credential on stderr")
	}
}

// update with a channel switches to it even when this binary is already
// the newest release there (review r1 partB sol-F7).
func TestMCPUpdateSwitchesChannelWhenCurrent(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	f.isRoot = true
	cfg := f.config()
	cfg.Update.Channel = "dev"
	a := &app{env: f.env, ctx: context.Background(), json: true}
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	f.up.newer = false
	call := mcpSession(t, f)
	res, ok := call("update", map[string]any{"channel": "stable"})
	if !ok {
		t.Fatalf("update: %s", mustJSON(res))
	}
	if got := f.config().Update.Channel; got != "stable" {
		t.Fatalf("channel after update --channel stable with nothing newer: %q", got)
	}
}
