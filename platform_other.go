//go:build !linux

package ublk

import "os"

const mmapPopulateFlag = 0 // MAP_POPULATE is Linux-specific; other platforms ignore it

func currentThreadID() int {
	return os.Getpid()
}
