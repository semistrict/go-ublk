package ublk

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

var (
	debugEnabledOnce sync.Once
	debugEnabled     bool

	debugVerifyDescOnce sync.Once
	debugVerifyDesc     bool

	debugDelaySeenOnce sync.Once
	debugDelaySeen     bool

	debugBatchStatsOnce sync.Once
	debugBatchStats     bool
)

func ioDebugEnabled() bool {
	debugEnabledOnce.Do(func() {
		debugEnabled = os.Getenv("GO_UBLK_DEBUG_IO") != ""
	})
	return debugEnabled
}

func ioDebugf(format string, args ...any) {
	if !ioDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "go-ublk debug tid=%d "+format+"\n", append([]any{syscall.Gettid()}, args...)...)
}

func ioDebugVerifyDescEnabled() bool {
	debugVerifyDescOnce.Do(func() {
		debugVerifyDesc = os.Getenv("GO_UBLK_DEBUG_IO_VERIFY_DESC") != ""
	})
	return debugVerifyDesc
}

func ioDebugDelaySeenEnabled() bool {
	debugDelaySeenOnce.Do(func() {
		debugDelaySeen = os.Getenv("GO_UBLK_DEBUG_IO_DELAY_SEEN") != ""
	})
	return debugDelaySeen
}

func ioDebugBatchStatsEnabled() bool {
	debugBatchStatsOnce.Do(func() {
		debugBatchStats = os.Getenv("GO_UBLK_DEBUG_BATCH_STATS") != ""
	})
	return debugBatchStats
}
