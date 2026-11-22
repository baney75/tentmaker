//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"syscall"
)

func isRoot() bool { return os.Geteuid() == 0 }

// ownLikeParent gives path the owner and group of its folder. migrate
// --pr, run by root under sudo, writes workflow files into a checkout the
// user owns; without this they would stay root's.
func ownLikeParent(path string) error {
	st, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Lchown(path, int(sys.Uid), int(sys.Gid))
}
