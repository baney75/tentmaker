//go:build !windows

package binsafe

// OwnNewFiles does nothing outside Windows: a file root creates is owned by
// root.
func OwnNewFiles() error { return nil }
