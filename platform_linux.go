//go:build linux

package ublk

import "syscall"

const mmapPopulateFlag = syscall.MAP_POPULATE // pre-fault mmap pages on Linux

func currentThreadID() int {
	return syscall.Gettid()
}
