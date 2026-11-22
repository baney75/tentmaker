package service

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// SystemdUnit is the unit file for s. KillMode=process stops only the
// daemon: busy runner units live in tentmaker.slice and keep running, and
// the next daemon adopts them.
func SystemdUnit(s Spec) string {
	var b strings.Builder
	b.WriteString(`[Unit]
Description=Tentmaker CI device
Documentation=https://github.com/baney75/tentmaker
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
`)
	for _, k := range sortedKeys(s.Env) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(k+"="+s.Env[k]))
	}
	words := []string{systemdQuote(s.Binary)}
	for _, a := range s.args() {
		words = append(words, systemdQuote(a))
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(words, " "))
	b.WriteString(`ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
KillMode=process
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`)
	return b.String()
}

// systemdQuote quotes a word for ExecStart or Environment when it needs it.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

type systemd struct{ o Options }

func (m *systemd) Install(ctx context.Context, s Spec) error {
	if err := s.validate(); err != nil {
		return err
	}
	if err := writeFile(m.o.path(UnitPath), []byte(SystemdUnit(s))); err != nil {
		return fmt.Errorf("write %s: %w", UnitPath, err)
	}
	if _, err := m.o.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if _, err := m.o.run(ctx, "systemctl", "enable", UnitName); err != nil {
		return err
	}
	// restart, not start: a rerun of setup picks up a new binary or unit.
	_, err := m.o.run(ctx, "systemctl", "restart", UnitName)
	return err
}

func (m *systemd) Uninstall(ctx context.Context) error {
	if exists(m.o.path(UnitPath)) {
		if _, err := m.o.run(ctx, "systemctl", "disable", "--now", UnitName); err != nil {
			return err
		}
	}
	if err := os.Remove(m.o.path(UnitPath)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err := m.o.run(ctx, "systemctl", "daemon-reload")
	return err
}

func (m *systemd) Start(ctx context.Context) error {
	_, err := m.o.run(ctx, "systemctl", "start", UnitName)
	return err
}

func (m *systemd) Stop(ctx context.Context) error {
	_, err := m.o.run(ctx, "systemctl", "stop", UnitName)
	return err
}

func (m *systemd) Restart(ctx context.Context) error {
	_, err := m.o.run(ctx, "systemctl", "restart", UnitName)
	return err
}

func (m *systemd) Status(ctx context.Context) (State, error) {
	st := State{Installed: exists(m.o.path(UnitPath))}
	// is-active exits non-zero when inactive; the word on stdout is the answer.
	out, err := m.o.run(ctx, "systemctl", "is-active", UnitName)
	word := strings.TrimSpace(firstLine(string(out)))
	switch word {
	case "active", "reloading", "activating", "deactivating":
		st.Running = word == "active" || word == "reloading"
		st.Detail = word
	case "inactive", "failed", "unknown", "":
		st.Detail = word
		if word == "" && err != nil {
			return st, err
		}
	default:
		st.Detail = word
	}
	return st, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
