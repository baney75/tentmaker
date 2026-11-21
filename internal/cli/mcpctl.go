package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/baney75/tentmaker/internal/mcp"
	"github.com/baney75/tentmaker/internal/mesh"
)

// controller is the MCP Controller. Every tool calls the same functions
// as the matching command; output goes only through MCP.
type controller struct{ a *app }

var _ mcp.Controller = controller{}

// mcpErr flattens a command error into one line with its fix.
func mcpErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *cliError
	if errors.As(err, &ce) && ce.fix != "" {
		return fmt.Errorf("%s (fix: %s)", ce.msg, ce.fix)
	}
	return err
}

// mutating refuses a change when the MCP server is not root.
func (c controller) mutating() error {
	if c.a.env.IsRoot() {
		return nil
	}
	how := "sudo"
	if c.a.env.GOOS == "windows" {
		how = "an Administrator shell"
	}
	return fmt.Errorf("this tool changes the device, and this tentmaker MCP server is not running as root/Administrator; run the matching tentmaker command with %s, or start the server that way", how)
}

func (c controller) Status(ctx context.Context) (any, error) {
	v, err := c.a.statusData(ctx)
	return v, mcpErr(err)
}

func (c controller) Pool(ctx context.Context) (any, error) {
	v, err := c.a.poolData(ctx)
	return v, mcpErr(err)
}

func (c controller) Doctor(ctx context.Context) (any, error) {
	return c.a.doctorData(ctx), nil
}

func (c controller) SetBudget(ctx context.Context, everyday, boost string, cpus, boostCPUs float64) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.setBudget(budgetChange{Memory: everyday, BoostMemory: boost, CPUs: cpus, BoostCPUs: boostCPUs})
	return mcpErr(err)
}

func (c controller) AddRepo(ctx context.Context, repo, forge, mode string, allowPublic bool) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.repoAdd(ctx, repoAddOpts{Name: repo, Forge: forge, Mode: mode, AllowPublic: allowPublic})
	return mcpErr(err)
}

func (c controller) RemoveRepo(ctx context.Context, repo string) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.repoRemove(repo)
	return mcpErr(err)
}

func (c controller) Pause(ctx context.Context, reason string) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.pause(ctx, reason, 0)
	return mcpErr(err)
}

func (c controller) Resume(ctx context.Context) error {
	if err := c.mutating(); err != nil {
		return err
	}
	_, err := c.a.resume(ctx)
	return mcpErr(err)
}

func (c controller) MigrateWorkflows(ctx context.Context, path, platform string, write bool) (any, error) {
	v, err := c.a.migrate(path, platform, write)
	return v, mcpErr(err)
}

func (c controller) InstallOnDevice(ctx context.Context, host, channel, memory, boost string) (any, error) {
	v, err := c.a.installOnDevice(ctx, host, mesh.InstallArgs{Channel: channel, Memory: memory, BoostMemory: boost})
	return v, mcpErr(err)
}

func (c controller) Update(ctx context.Context, channel string) (any, error) {
	if err := c.mutating(); err != nil {
		return nil, err
	}
	v, err := c.a.update(ctx, channel, false)
	return v, mcpErr(err)
}

func (a *app) cmdMCP(args []string) (any, error) {
	f := a.flags("mcp", commands["mcp"].usage)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: tentmaker mcp", "unexpected argument %q", pos[0])
	}
	// stdout is the protocol: nothing else may print there.
	a.json = true
	if err := mcp.Serve(a.ctx, a.in, a.env.Stdout, controller{a}, a.env.Version); err != nil && a.ctx.Err() == nil {
		fmt.Fprintln(a.env.Stderr, "tentmaker mcp:", err)
		return silentExit(ExitFailed), nil
	}
	return nil, nil
}

// silentExit ends a command with a code and prints nothing more, for
// commands whose stdout is a protocol (mcp).
type silentExit int

func (s silentExit) exitCode() int { return int(s) }
