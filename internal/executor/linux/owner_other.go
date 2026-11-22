//go:build !unix

package linux

import "io/fs"

// fileOwner has no uid to report off Unix, so nothing is hard-linked.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
