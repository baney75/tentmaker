package mesh

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
)

const (
	installURL    = "https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh"
	installPS1URL = "https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1"
	// installTimeout bounds the whole remote install (download, setup).
	installTimeout = 15 * time.Minute
)

// InstallArgs is what RemoteInstall sets up on the other device.
type InstallArgs struct {
	Channel     string   // "stable" (default) or "dev"
	Pool        string   // mesh pool name; empty = setup's default
	Memory      string   // everyday memory, like "4GiB"; empty = setup's default
	BoostMemory string   // boost memory, like "12GiB"; empty = setup's default
	Repos       []string // "owner/name"
}

var (
	poolRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	hostRe = regexp.MustCompile(`^(?:[a-z_][a-z0-9_-]{0,31}@)?[A-Za-z0-9](?:[A-Za-z0-9-]{0,62})(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,62}))*\.?$`)
)

// RemoteInstall installs Tentmaker on host over `tailscale ssh` and runs
// `tentmaker setup` there. Windows hosts can't take Tailscale SSH, so for
// them it returns an error with the PowerShell to run on that machine.
func RemoteInstall(ctx context.Context, host string, args InstallArgs) error {
	find := func() (string, error) {
		return findTailscale(runtime.GOOS, exec.LookPath, fileExists, os.Getenv)
	}
	return remoteInstall(ctx, newCLI(ExecRunner, find, time.Now), find, ExecRunner, host, args)
}

func remoteInstall(ctx context.Context, ts Tailscale, find func() (string, error), run Runner, host string, args InstallArgs) error {
	flags, channel, err := installFlags(args)
	if err != nil {
		return err
	}
	if err := validHost(host); err != nil {
		return err
	}
	if hostOS(ctx, ts, host) == "windows" {
		return fmt.Errorf("%s runs Windows, and Tailscale SSH has no Windows server. In an Administrator PowerShell on %s, run:\n%s", host, host, windowsCommand(channel, flags))
	}
	bin, err := find()
	if err != nil {
		return err
	}
	script := installScript(channel, flags)
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	out, err := run(ctx, bin, "ssh", host, "--", "sh", "-c", shellQuote(script))
	if err != nil {
		return fmt.Errorf("install on %s failed: %w\n%s", host, err, tail(string(out), 4096))
	}
	return nil
}

// installFlags validates args and returns the setup flags, already
// shell-quoted, and the channel.
func installFlags(a InstallArgs) ([]string, string, error) {
	channel := a.Channel
	switch channel {
	case "":
		channel = "stable"
	case "stable", "dev":
	default:
		return nil, "", fmt.Errorf(`channel %q: use "stable" or "dev"`, a.Channel)
	}
	var flags []string
	if a.Pool != "" {
		if !poolRe.MatchString(a.Pool) {
			return nil, "", fmt.Errorf("pool %q: use letters, digits, dot, dash or underscore", a.Pool)
		}
		flags = append(flags, "--pool", shellQuote(a.Pool))
	}
	for _, s := range []struct{ flag, v string }{{"--memory", a.Memory}, {"--boost-memory", a.BoostMemory}} {
		if s.v == "" {
			continue
		}
		size, err := config.ParseSize(s.v)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", s.flag, err)
		}
		if size <= 0 {
			return nil, "", fmt.Errorf("%s must be above zero", s.flag)
		}
		flags = append(flags, s.flag, shellQuote(size.String()))
	}
	for _, r := range a.Repos {
		if !repoRe.MatchString(r) || strings.Contains(r, "..") {
			return nil, "", fmt.Errorf("repo %q must be owner/name", r)
		}
		flags = append(flags, "--repo", shellQuote(r))
	}
	return flags, channel, nil
}

func validHost(host string) error {
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if len(host) > 253 || !hostRe.MatchString(host) {
		return fmt.Errorf("host %q: use a tailnet device name, MagicDNS name or IP", host)
	}
	return nil
}

// hostOS looks host up in tailscale status; "" when it can't tell.
func hostOS(ctx context.Context, ts Tailscale, host string) string {
	if ts == nil {
		return ""
	}
	if _, h, ok := strings.Cut(host, "@"); ok {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	peers, err := ts.Peers(ctx)
	if err != nil {
		return ""
	}
	for _, p := range peers {
		dns := strings.ToLower(p.DNSName)
		short, _, _ := strings.Cut(dns, ".")
		if host == strings.ToLower(p.HostName) || host == dns || (short != "" && host == short) {
			return strings.ToLower(p.OS)
		}
		for _, ip := range p.IPs {
			if host == ip.String() {
				return strings.ToLower(p.OS)
			}
		}
	}
	return ""
}

// installScript is the POSIX sh run on the remote device. The installer is
// downloaded to a file first so a failed download fails the install
// instead of piping nothing into sh.
func installScript(channel string, flags []string) string {
	setup := "tentmaker setup --yes --json"
	if len(flags) > 0 {
		setup += " " + strings.Join(flags, " ")
	}
	return strings.Join([]string{
		"set -eu",
		"exec 2>&1",
		`f=$(mktemp)`,
		`trap 'rm -f "$f"' EXIT`,
		"curl -fsSL --max-time 300 " + installURL + ` -o "$f"`,
		`sh "$f" --channel ` + shellQuote(channel),
		`if [ "$(id -u)" -eq 0 ]; then ` + setup + "; else sudo -n " + setup + "; fi",
	}, "\n")
}

// windowsCommand is the PowerShell to run by hand on a Windows device.
func windowsCommand(channel string, flags []string) string {
	install := "irm " + installPS1URL + " | iex"
	if channel != "stable" {
		install = "& ([scriptblock]::Create((irm " + installPS1URL + "))) -Channel " + channel
	}
	setup := "tentmaker setup --yes --json"
	if len(flags) > 0 {
		setup += " " + strings.Join(flags, " ")
	}
	return install + "; " + setup
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
