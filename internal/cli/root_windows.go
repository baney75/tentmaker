//go:build windows

package cli

import "golang.org/x/sys/windows"

func isRoot() bool { return windows.GetCurrentProcessToken().IsElevated() }
