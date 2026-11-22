//go:build windows

package cli

import "golang.org/x/sys/windows"

func isRoot() bool { return windows.GetCurrentProcessToken().IsElevated() }

// ownLikeParent does nothing on Windows: the elevated user who runs
// migrate --pr is the user who owns the checkout.
func ownLikeParent(string) error { return nil }
