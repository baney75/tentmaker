//go:build unix

package linux

import (
	"io/fs"
	"syscall"
)

// fileOwner is the uid that owns fi.
func fileOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
