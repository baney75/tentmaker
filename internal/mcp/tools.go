package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// prop is one tool argument.
type prop struct {
	Name        string
	Type        string // "string", "number", "boolean"
	Description string
	Enum        []string
	Default     any
	Required    bool
	Check       func(v any) error // extra validation after the type check
}

type tool struct {
	Name        string
	Title       string
	Description string
	Props       []prop
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	OpenWorld   bool
	run         func(ctx context.Context, c Controller, a args) (any, error)
}

// args holds checked arguments with defaults applied.
type args map[string]any

func (a args) str(k string) string {
	s, _ := a[k].(string)
	return s
}

func (a args) num(k string) float64 {
	f, _ := a[k].(float64)
	return f
}

func (a args) flag(k string) bool {
	b, _ := a[k].(bool)
	return b
}

func ok(err error) (any, error) { return nil, err }

// sizeRe matches the sizes config.Size accepts: "4GiB", "4G", "4GB",
// "512MiB", "0". The controller does the real parse.
var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9]+(\.[0-9]+)?)\s*([KMGT]i?B?)?\s*$`)

func memorySize(v any) error {
	s := v.(string)
	if s == "" {
		return nil
	}
	m := sizeRe.FindStringSubmatch(s)
	if m == nil || (m[3] == "" && strings.Trim(m[1], "0.") != "") {
		return fmt.Errorf("size %q: use a number with MiB or GiB, like 8GiB", s)
	}
	return nil
}

func cpuCount(v any) error {
	f := v.(float64)
	if f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("must be a positive number of CPUs (0 or omitted keeps the current value)")
	}
	return nil
}

func repoName(v any) error {
	s := v.(string)
	if strings.TrimSpace(s) != s || strings.ContainsAny(s, " \t\n") || strings.HasPrefix(s, "/") ||
		strings.HasSuffix(s, "/") || !strings.Contains(s, "/") || strings.Contains(s, "//") {
		return fmt.Errorf("%q is not a repository; use owner/name, like baney75/tentmaker", s)
	}
	return nil
}

func hostName(v any) error {
	s := v.(string)
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\n\"'`;|&$<>\\") {
		return fmt.Errorf("%q is not a host name; use the device's tailnet name, like laptop or user@laptop", s)
	}
	return nil
}

func nonEmpty(v any) error {
	if strings.TrimSpace(v.(string)) == "" {
		return fmt.Errorf("must not be empty")
	}
	return nil
}

var memoryHelp = `Memory as a number with a unit, like "4GiB" or "512MiB".`

var tools = []tool{
	{
		Name:  "status",
		Title: "Device status",
		Description: "Show this device's Tentmaker state: the active budget profile (everyday, boost or off) and why, " +
			"free slots, running jobs, paused or draining, power and guard readings, and the installed version. " +
			"Read-only. Call this first.",
		ReadOnly: true, Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) { return c.Status(ctx) },
	},
	{
		Name:  "pool",
		Title: "Pool status",
		Description: "List every device in this device's pool (same tailnet, same pool name) with its budget, free " +
			"slots, running jobs, version and whether it is a broker. Read-only. Use it to see total CI capacity " +
			"or find a device that isn't serving jobs.",
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) { return c.Pool(ctx) },
	},
	{
		Name:  "doctor",
		Title: "Diagnose problems",
		Description: "Run every health check on this device (service running, executor installed, forge credentials, " +
			"tailnet reachable, disk and memory guards, config valid) and return each problem with the exact fix. " +
			"Read-only. Run it when jobs stay queued or a device is missing from the pool.",
		ReadOnly: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) { return c.Doctor(ctx) },
	},
	{
		Name:  "set_budget",
		Title: "Set the CI budget",
		Description: "Change how much of this device CI may use. Everyday applies all the time; boost applies when " +
			"the device is plugged in (and idle, if configured) and must be at least everyday. Pass only the values " +
			"to change; omitted ones keep their current setting. Running jobs are not stopped: the device drains " +
			"down to a smaller budget.",
		Props: []prop{
			{Name: "everyday_memory", Type: "string", Description: "Everyday memory budget. " + memoryHelp, Check: memorySize},
			{Name: "boost_memory", Type: "string", Description: "Boost memory budget, at least the everyday one. " + memoryHelp, Check: memorySize},
			{Name: "everyday_cpus", Type: "number", Description: "Everyday CPU budget in cores; fractions allowed, like 1.5.", Check: cpuCount},
			{Name: "boost_cpus", Type: "number", Description: "Boost CPU budget in cores; fractions allowed.", Check: cpuCount},
		},
		Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			if a.str("everyday_memory") == "" && a.str("boost_memory") == "" && a.num("everyday_cpus") == 0 && a.num("boost_cpus") == 0 {
				return nil, fmt.Errorf("nothing to change: pass at least one of everyday_memory, boost_memory, everyday_cpus, boost_cpus")
			}
			return ok(c.SetBudget(ctx, a.str("everyday_memory"), a.str("boost_memory"), a.num("everyday_cpus"), a.num("boost_cpus")))
		},
	},
	{
		Name:  "add_repo",
		Title: "Serve a repository",
		Description: "Let this device run CI jobs for a repository. Mode runner registers the forge's own runner " +
			"(GitHub, Forgejo); mode checks runs workflow jobs itself and posts commit statuses (any forge, required " +
			"for GitLab). Public repos are refused unless allow_public is true, which first makes the forge require " +
			"approval before outside contributors' workflows run. Workflows must ask for runs-on: [self-hosted, tentmaker, <os>].",
		Props: []prop{
			{Name: "repo", Type: "string", Required: true, Description: "Repository as owner/name.", Check: repoName},
			{Name: "forge", Type: "string", Enum: []string{"github", "forgejo", "gitlab"}, Default: "github", Description: "Forge that hosts the repository."},
			{Name: "mode", Type: "string", Enum: []string{"runner", "checks"}, Description: "runner (default for github) or checks (the default, and the only mode, for gitlab and forgejo)."},
			{Name: "allow_public", Type: "boolean", Default: false, Description: "Allow a public repository. Leave false unless the owner asked for it."},
		},
		Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			// An empty mode lets the controller pick the forge's default, the
			// same rule the CLI uses; filling in "runner" here broke every
			// Forgejo add (review r1 partB first-F3).
			return ok(c.AddRepo(ctx, a.str("repo"), a.str("forge"), a.str("mode"), a.flag("allow_public")))
		},
	},
	{
		Name:  "remove_repo",
		Title: "Stop serving a repository",
		Description: "Stop running CI for a repository on this device and remove its runners from the forge. Jobs " +
			"already running finish first. Queued jobs for it wait for another runner.",
		Props: []prop{
			{Name: "repo", Type: "string", Required: true, Description: "Repository as owner/name.", Check: repoName},
		},
		Destructive: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return ok(c.RemoveRepo(ctx, a.str("repo")))
		},
	},
	{
		Name:  "pause",
		Title: "Pause CI on this device",
		Description: "Stop taking new jobs on this device until resume is called. Running jobs finish. The pause " +
			"survives restarts. Use it before heavy work on the machine.",
		Props: []prop{
			{Name: "reason", Type: "string", Description: "Why, shown in status, like \"video call\"."},
		},
		Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return ok(c.Pause(ctx, a.str("reason")))
		},
	},
	{
		Name:        "resume",
		Title:       "Resume CI on this device",
		Description: "Undo pause: take new jobs again, within the budget.",
		Idempotent:  true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return ok(c.Resume(ctx))
		},
	},
	{
		Name:  "migrate_workflows",
		Title: "Point workflows at Tentmaker",
		Description: "Rewrite a local checkout's GitHub workflow runs-on lines to use Tentmaker runners for the given " +
			"platform, and report every change. With write false (the default) it only reports; with write true it " +
			"edits files under .github/workflows in place. Review the report before writing, and commit the result yourself.",
		Props: []prop{
			{Name: "repo_path", Type: "string", Required: true, Description: "Absolute path of a local git checkout.", Check: nonEmpty},
			{Name: "platform", Type: "string", Default: "linux", Description: "Target runner OS: linux, macos or windows.", Check: nonEmpty},
			{Name: "write", Type: "boolean", Default: false, Description: "Edit the files (true) or only report (false)."},
		},
		Destructive: true, Idempotent: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.MigrateWorkflows(ctx, a.str("repo_path"), a.str("platform"), a.flag("write"))
		},
	},
	{
		Name:  "install_on_device",
		Title: "Install on another device",
		Description: "Install Tentmaker on another device on the same tailnet over tailscale ssh, run setup " +
			"non-interactively and join it to this pool. Safe to rerun. The host must allow tailscale ssh for this user. " +
			"Memory values default to the installer's (a quarter and half of RAM).",
		Props: []prop{
			{Name: "host", Type: "string", Required: true, Description: "Tailnet host name, like laptop or user@laptop.", Check: hostName},
			{Name: "channel", Type: "string", Enum: []string{"stable", "dev"}, Default: "stable", Description: "Release channel."},
			{Name: "memory", Type: "string", Description: "Everyday memory budget. " + memoryHelp, Check: memorySize},
			{Name: "boost_memory", Type: "string", Description: "Boost memory budget. " + memoryHelp, Check: memorySize},
		},
		Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.InstallOnDevice(ctx, a.str("host"), a.str("channel"), a.str("memory"), a.str("boost_memory"))
		},
	},
	{
		Name:  "update",
		Title: "Update Tentmaker",
		Description: "Update this device to the newest release on a channel: download, verify the sha256, health " +
			"check, swap the binary, restart the service, and roll back if it isn't healthy within 60 s. It first " +
			"pauses new jobs and waits up to 30 min for running ones to finish; if they don't, nothing changes and " +
			"new jobs resume. Omit channel to use the configured one; passing one also switches the device to it.",
		Props: []prop{
			{Name: "channel", Type: "string", Enum: []string{"stable", "dev"}, Description: "stable or dev; omit for the configured channel."},
		},
		Destructive: true, Idempotent: true, OpenWorld: true,
		run: func(ctx context.Context, c Controller, a args) (any, error) {
			return c.Update(ctx, a.str("channel"))
		},
	},
}

func toolByName(name string) (tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

// check validates raw arguments against t's schema and applies defaults.
func (t tool) check(raw map[string]any) (args, error) {
	known := map[string]prop{}
	for _, p := range t.Props {
		known[p.Name] = p
	}
	var unknown []string
	for k := range raw {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown argument %s; this tool takes %s", strings.Join(unknown, ", "), t.argNames())
	}
	a := args{}
	for _, p := range t.Props {
		v, present := raw[p.Name]
		if !present || v == nil {
			if p.Required {
				return nil, fmt.Errorf("%s is required: %s", p.Name, p.Description)
			}
			switch {
			case p.Default != nil:
				v = p.Default
			case p.Type == "string":
				v = ""
			case p.Type == "number":
				v = float64(0)
			case p.Type == "boolean":
				v = false
			}
		}
		if !typeOK(p.Type, v) {
			return nil, fmt.Errorf("%s must be a %s", p.Name, p.Type)
		}
		if len(p.Enum) > 0 && present {
			if !contains(p.Enum, v.(string)) {
				return nil, fmt.Errorf("%s must be one of %s", p.Name, strings.Join(p.Enum, ", "))
			}
		}
		if p.Check != nil {
			if err := p.Check(v); err != nil {
				return nil, fmt.Errorf("%s: %v", p.Name, err)
			}
		}
		a[p.Name] = v
	}
	return a, nil
}

func (t tool) argNames() string {
	if len(t.Props) == 0 {
		return "no arguments"
	}
	var n []string
	for _, p := range t.Props {
		n = append(n, p.Name)
	}
	return strings.Join(n, ", ")
}

func typeOK(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// toolList is the tools/list payload.
func toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		props := map[string]any{}
		var required []string
		for _, p := range t.Props {
			s := map[string]any{"type": p.Type, "description": p.Description}
			if len(p.Enum) > 0 {
				s["enum"] = p.Enum
			}
			if p.Default != nil {
				s["default"] = p.Default
			}
			props[p.Name] = s
			if p.Required {
				required = append(required, p.Name)
			}
		}
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": schema,
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
				"idempotentHint":  t.Idempotent,
				"openWorldHint":   t.OpenWorld,
			},
		})
	}
	return out
}

// normalizeID makes 1, 1.0 and "1" compare by their JSON value, so a
// cancellation finds its request regardless of spacing.
func normalizeID(id json.RawMessage) string {
	var v any
	if json.Unmarshal(id, &v) != nil {
		return string(id)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
