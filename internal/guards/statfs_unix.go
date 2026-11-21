//go:build linux || darwin

package guards

import "golang.org/x/sys/unix"

// statfsFree is the bytes free to unprivileged users on path's file system.
func statfsFree(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
