package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/executor/darwin"
	"github.com/baney75/tentmaker/internal/executor/linux"
	"github.com/baney75/tentmaker/internal/executor/windows"
	"github.com/baney75/tentmaker/internal/forge/github"
)

// readLine reads bytes up to a newline, one at a time, so nothing after the
// line is consumed. The line ending is dropped.
func readLine(r io.Reader) ([]byte, error) {
	var out []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				break
			}
			out = append(out, b[0])
			if len(out) > 64<<10 {
				return nil, errors.New("line too long")
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(out) > 0 {
				break
			}
			return nil, err
		}
	}
	return []byte(strings.TrimRight(string(out), "\r")), nil
}

// toolDirs is where host tools are found; the caller's PATH comes last.
var toolDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin", "/opt/homebrew/bin"}

func lookTool(name string) (string, error) {
	for _, d := range toolDirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found", name)
	}
	return p, nil
}

func execCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, path, args...)
}

// ghTimeout bounds `gh auth token`.
const ghTimeout = 15 * time.Second

// ghToken asks the GitHub CLI for its token, as user when set (under sudo,
// the token belongs to the person who ran sudo, not root). The token is
// returned only; it is never logged or put in an error.
func ghToken(ctx context.Context, user string) (string, error) {
	if user == "" || user == "root" {
		ctx, cancel := context.WithTimeout(ctx, ghTimeout)
		defer cancel()
		return github.TokenFromGH(ctx)
	}
	gh, err := lookTool("gh")
	if err != nil {
		return "", errors.New("the GitHub CLI (gh) is not installed")
	}
	sudo, err := lookTool("sudo")
	if err != nil {
		return "", errors.New("sudo not found")
	}
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sudo, "-n", "-H", "-u", user, "--", gh, "auth", "token")
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("`gh auth token` as %s failed; run `gh auth login` as %s first", user, user)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" || strings.ContainsAny(tok, " \n\t") {
		return "", fmt.Errorf("`gh auth token` as %s printed no token; run `gh auth login`", user)
	}
	return tok, nil
}

// InstallOptions are setup's choices for the executor install.
type InstallOptions struct {
	// MacOSGuests pulls the macOS VM image even when free disk doesn't
	// cover it with the usual margin (setup --macos-guests).
	MacOSGuests bool
}

// installExecutors runs this OS's executor Install.
func installExecutors(ctx context.Context, goos string, cfg config.Config, binary func(core.Platform) (string, error), o InstallOptions) error {
	switch goos {
	case "linux":
		return linux.Install(ctx, linux.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir, Binary: binary, AllowLAN: cfg.Guards.AllowLAN})
	case "darwin":
		return darwin.Install(ctx, darwin.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir, Binary: binary, MacOSGuests: o.MacOSGuests})
	case "windows":
		return windows.Install(ctx, windows.Options{BaseDir: windowsBaseDir(config.Path()), StateDir: cfg.StateDir, CacheDir: cfg.CacheDir, Binary: binary, AllowLAN: cfg.Guards.AllowLAN})
	}
	return fmt.Errorf("Tentmaker has no sandbox for %s", goos)
}

// windowsBaseDir is the folder that holds the config, when it is
// Tentmaker's own (%ProgramData%\Tentmaker), or "": setup never changes the
// ACL of a folder a custom config path points into.
func windowsBaseDir(configPath string) string {
	d := filepath.Dir(configPath)
	if !strings.EqualFold(filepath.Base(d), "Tentmaker") {
		return ""
	}
	return d
}

// uninstallExecutors runs this OS's executor Uninstall.
func uninstallExecutors(ctx context.Context, goos string, cfg config.Config, purge bool) error {
	switch goos {
	case "linux":
		return linux.Uninstall(ctx, linux.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir}, purge)
	case "darwin":
		return darwin.Uninstall(ctx, darwin.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir}, purge)
	case "windows":
		return windows.Uninstall(ctx, windows.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir}, purge)
	}
	return nil
}
