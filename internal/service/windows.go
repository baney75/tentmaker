package service

import (
	"context"
	"errors"
)

// SCM is the part of the Windows service control manager Tentmaker uses.
// NewSCM returns the real one on Windows; tests use a fake.
type SCM interface {
	// Install creates the service, or updates its binary and arguments when
	// it exists: automatic start, restart after 5 s on the first three
	// failures.
	Install(name, display, description, exe string, args []string) error
	// Remove stops and deletes the service. A missing service is fine.
	Remove(name string) error
	Start(name string) error
	// Stop asks the service to stop and waits for it. A stopped or missing
	// service is fine.
	Stop(name string) error
	Query(name string) (State, error)
}

// ErrNoSCM is returned by the SCM on systems that aren't Windows.
var ErrNoSCM = errors.New("the Windows service manager exists only on Windows")

type winService struct{ o Options }

func (m *winService) Install(ctx context.Context, s Spec) error {
	if err := s.validate(); err != nil {
		return err
	}
	if len(s.Env) > 0 {
		return errors.New("a Windows service can't carry environment variables; use the default config path")
	}
	if err := m.o.SCM.Install(WindowsName, "Tentmaker", "Runs CI jobs in this machine's spare capacity. https://github.com/baney75/tentmaker", s.Binary, s.args()); err != nil {
		return err
	}
	if err := m.o.SCM.Stop(WindowsName); err != nil {
		return err
	}
	return m.o.SCM.Start(WindowsName)
}

func (m *winService) Uninstall(ctx context.Context) error { return m.o.SCM.Remove(WindowsName) }
func (m *winService) Start(ctx context.Context) error     { return m.o.SCM.Start(WindowsName) }
func (m *winService) Stop(ctx context.Context) error      { return m.o.SCM.Stop(WindowsName) }
func (m *winService) Restart(ctx context.Context) error {
	if err := m.o.SCM.Stop(WindowsName); err != nil {
		return err
	}
	return m.o.SCM.Start(WindowsName)
}
func (m *winService) Status(ctx context.Context) (State, error) { return m.o.SCM.Query(WindowsName) }
