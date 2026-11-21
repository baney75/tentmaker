package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LaunchdPlist is the launchd daemon for s. AbandonProcessGroup keeps
// running job VMs alive when the daemon restarts; the next daemon adopts
// them.
func LaunchdPlist(s Spec) string {
	logPath := s.LogPath
	if logPath == "" {
		logPath = MacLogPath
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range append([]string{s.Binary}, s.args()...) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("\t</array>\n")
	if len(s.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range sortedKeys(s.Env) {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEscape(k), xmlEscape(s.Env[k]))
		}
		b.WriteString("\t</dict>\n")
	}
	fmt.Fprintf(&b, `	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>ExitTimeOut</key>
	<integer>30</integer>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, xmlEscape(logPath), xmlEscape(logPath))
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

type launchd struct{ o Options }

const target = "system/" + Label

func (m *launchd) loaded(ctx context.Context) (bool, string) {
	out, err := m.o.run(ctx, "launchctl", "print", target)
	if err != nil {
		return false, ""
	}
	return true, string(out)
}

func (m *launchd) Install(ctx context.Context, s Spec) error {
	if err := s.validate(); err != nil {
		return err
	}
	logPath := s.LogPath
	if logPath == "" {
		logPath = MacLogPath
	}
	if err := os.MkdirAll(m.o.path(filepath.Dir(logPath)), 0o755); err != nil {
		return err
	}
	if err := writeFile(m.o.path(PlistPath), []byte(LaunchdPlist(s))); err != nil {
		return fmt.Errorf("write %s: %w", PlistPath, err)
	}
	// Unload an older copy first so launchd reads the new plist.
	if ok, _ := m.loaded(ctx); ok {
		if _, err := m.o.run(ctx, "launchctl", "bootout", target); err != nil {
			return err
		}
	}
	if _, err := m.o.run(ctx, "launchctl", "enable", target); err != nil {
		return err
	}
	if _, err := m.o.run(ctx, "launchctl", "bootstrap", "system", PlistPath); err != nil {
		return err
	}
	// RunAtLoad starts it; kickstart makes sure, and is a no-op when running.
	_, err := m.o.run(ctx, "launchctl", "kickstart", target)
	return err
}

func (m *launchd) Uninstall(ctx context.Context) error {
	if ok, _ := m.loaded(ctx); ok {
		if _, err := m.o.run(ctx, "launchctl", "bootout", target); err != nil {
			return err
		}
	}
	if err := os.Remove(m.o.path(PlistPath)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *launchd) Start(ctx context.Context) error {
	if ok, _ := m.loaded(ctx); !ok {
		if !exists(m.o.path(PlistPath)) {
			return fmt.Errorf("%s is missing; run `sudo tentmaker setup`", PlistPath)
		}
		if _, err := m.o.run(ctx, "launchctl", "bootstrap", "system", PlistPath); err != nil {
			return err
		}
	}
	_, err := m.o.run(ctx, "launchctl", "kickstart", target)
	return err
}

// Stop unloads the daemon: with KeepAlive, a plain kill would restart it.
func (m *launchd) Stop(ctx context.Context) error {
	if ok, _ := m.loaded(ctx); !ok {
		return nil
	}
	_, err := m.o.run(ctx, "launchctl", "bootout", target)
	return err
}

func (m *launchd) Restart(ctx context.Context) error {
	if ok, _ := m.loaded(ctx); !ok {
		return m.Start(ctx)
	}
	_, err := m.o.run(ctx, "launchctl", "kickstart", "-k", target)
	return err
}

func (m *launchd) Status(ctx context.Context) (State, error) {
	st := State{Installed: exists(m.o.path(PlistPath))}
	ok, out := m.loaded(ctx)
	if !ok {
		st.Detail = "not loaded"
		return st, nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, found := strings.CutPrefix(line, "state = "); found {
			st.Detail = v
			st.Running = v == "running"
			break
		}
	}
	return st, nil
}
