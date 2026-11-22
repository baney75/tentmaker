//go:build !windows

package binsafe

import (
	"errors"
	"os"
	"syscall"
)

var hostJudge Judge = Unix

func hostLookup(path string) (Entry, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Entry{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Entry{}, errors.New("can't read the owner of " + path)
	}
	return Entry{UID: uint32(st.Uid), GID: uint32(st.Gid), Mode: fi.Mode()}, nil
}
