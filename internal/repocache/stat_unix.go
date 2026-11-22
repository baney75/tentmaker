//go:build unix

package repocache

import (
	"io/fs"
	"syscall"
)

// diskBytes is the space a file takes on disk (blocks, not length), so a
// cache of many small files is not undercounted.
func diskBytes(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return fi.Size()
}

func ownerOf(fi fs.FileInfo) (int, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), true
	}
	return 0, false
}
