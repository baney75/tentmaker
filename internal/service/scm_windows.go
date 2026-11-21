//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// NewSCM returns the real Windows service control manager.
func NewSCM() SCM { return realSCM{} }

type realSCM struct{}

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("open the service manager (run as Administrator): %w", err)
	}
	return m, nil
}

func (realSCM) Install(name, display, description, exe string, args []string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err == nil {
		cfg, err := s.Config()
		if err != nil {
			s.Close()
			return err
		}
		cfg.BinaryPathName = windows.EscapeArg(exe)
		for _, a := range args {
			cfg.BinaryPathName += " " + windows.EscapeArg(a)
		}
		cfg.StartType = mgr.StartAutomatic
		cfg.DisplayName = display
		cfg.Description = description
		if err := s.UpdateConfig(cfg); err != nil {
			s.Close()
			return err
		}
	} else {
		s, err = m.CreateService(name, exe, mgr.Config{
			DisplayName: display,
			Description: description,
			StartType:   mgr.StartAutomatic,
		}, args...)
		if err != nil {
			return fmt.Errorf("create service %s: %w", name, err)
		}
	}
	defer s.Close()
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	}
	if err := s.SetRecoveryActions(actions, 24*60*60); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	// A clean exit with a non-zero code (after a self-update) restarts too.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	return nil
}

func (r realSCM) Remove(name string) error {
	if err := r.Stop(name); err != nil {
		return err
	}
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return nil
	}
	defer s.Close()
	if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return err
	}
	return nil
}

func (realSCM) Start(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service %s is not installed; run tentmaker setup as Administrator", name)
	}
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	return nil
}

func (realSCM) Stop(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	deadline := time.Now().Add(StopTimeout)
	for time.Now().Before(deadline) {
		st, err = s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("service %s did not stop within %s", name, StopTimeout)
}

func (realSCM) Query(name string) (State, error) {
	m, err := connect()
	if err != nil {
		return State{}, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return State{Detail: "not installed"}, nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return State{Installed: true}, err
	}
	names := map[svc.State]string{
		svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping",
		svc.Running: "running", svc.ContinuePending: "continuing", svc.PausePending: "pausing", svc.Paused: "paused",
	}
	return State{Installed: true, Running: st.State == svc.Running, Detail: names[st.State]}, nil
}

// IsWindowsService reports whether the service manager started this process.
func IsWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// RunWindowsService runs run under the service manager until it returns or
// the manager asks the service to stop or the machine shuts down. A run
// error becomes a non-zero exit code, so the recovery actions restart it.
func RunWindowsService(run func(ctx context.Context) error) error {
	h := &handler{run: run}
	if err := svc.Run(WindowsName, h); err != nil {
		return err
	}
	return h.err
}

type handler struct {
	run func(ctx context.Context) error
	err error
}

func (h *handler) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			h.err = err
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(StopTimeout / time.Millisecond)}
				cancel()
				h.err = <-done
				return false, 0
			}
		}
	}
}
