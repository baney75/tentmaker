//go:build unix

package joblog

import "syscall"

const nonblock = syscall.O_NONBLOCK
