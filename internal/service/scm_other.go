//go:build !windows

package service

import "context"

// NewSCM returns an SCM that fails: there is none off Windows.
func NewSCM() SCM { return noSCM{} }

type noSCM struct{}

func (noSCM) Install(string, string, string, string, []string) error { return ErrNoSCM }
func (noSCM) Remove(string) error                                    { return ErrNoSCM }
func (noSCM) Start(string) error                                     { return ErrNoSCM }
func (noSCM) Stop(string) error                                      { return ErrNoSCM }
func (noSCM) Query(string) (State, error)                            { return State{}, ErrNoSCM }

// IsWindowsService reports whether the Windows service manager started this
// process. Always false off Windows.
func IsWindowsService() bool { return false }

// RunWindowsService runs run under the Windows service manager. Off Windows
// it is never called; it runs run directly.
func RunWindowsService(run func(ctx context.Context) error) error {
	return run(context.Background())
}
