//go:build !windows

package windows

import (
	"context"
	"errors"
)

var errNotWindows = errors.New("the windows-job executor runs only on Windows")

// OSAPI is the real API. Off Windows every call fails, so the package
// builds and tests with fakes run anywhere.
type OSAPI struct{}

// NewOSAPI returns the real API.
func NewOSAPI() *OSAPI { return &OSAPI{} }

func (*OSAPI) IsAdmin() (bool, error)                             { return false, errNotWindows }
func (*OSAPI) FreeDisk(string) (uint64, error)                    { return 0, errNotWindows }
func (*OSAPI) UserSID(string) (string, error)                     { return "", errNotWindows }
func (*OSAPI) CreateUser(string, []byte) error                    { return errNotWindows }
func (*OSAPI) SetPassword(string, []byte) error                   { return errNotWindows }
func (*OSAPI) DeleteUser(string) error                            { return errNotWindows }
func (*OSAPI) LocalGroups(string) ([]Group, error)                { return nil, errNotWindows }
func (*OSAPI) RemoveFromGroup(string, string) error               { return errNotWindows }
func (*OSAPI) AccountRights(string) ([]string, error)             { return nil, errNotWindows }
func (*OSAPI) AddRights(string, []string) error                   { return errNotWindows }
func (*OSAPI) RemoveAllRights(string) error                       { return errNotWindows }
func (*OSAPI) HideUser(string, bool) error                        { return errNotWindows }
func (*OSAPI) Protect([]byte) ([]byte, error)                     { return nil, errNotWindows }
func (*OSAPI) Unprotect([]byte) ([]byte, error)                   { return nil, errNotWindows }
func (*OSAPI) SetDACL(string, []ACE) error                        { return errNotWindows }
func (*OSAPI) Owner(string) (string, error)                       { return "", errNotWindows }
func (*OSAPI) StartProcess(ProcSpec) (Process, error)             { return nil, errNotWindows }
func (*OSAPI) KillUserProcesses(string) (int, error)              { return 0, errNotWindows }
func (*OSAPI) DeleteUserTasks(string, string) (int, error)        { return 0, errNotWindows }
func (*OSAPI) Fetch(ctx context.Context, url, dst string) error   { return httpFetch(ctx, url, dst) }
func (*OSAPI) PowerShell(context.Context, string) (string, error) { return "", errNotWindows }
