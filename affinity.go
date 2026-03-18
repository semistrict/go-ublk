package ublk

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	affinityBufSize = 128 // 1024 CPUs (128 bytes * 8 bits)
	sysSchedSetaffinity = 203 // SYS_SCHED_SETAFFINITY on amd64
)

// QueueAffinity represents the CPU affinity mask for a device queue.
type QueueAffinity struct {
	mask []byte
}

// CPUs returns the list of CPU indices in this affinity mask.
func (a *QueueAffinity) CPUs() []int {
	var cpus []int
	for i, b := range a.mask {
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(bit)) != 0 {
				cpus = append(cpus, i*8+bit)
			}
		}
	}
	return cpus
}

// GetQueueAffinity retrieves the CPU affinity for the given queue.
func (d *Device) GetQueueAffinity(qid uint16) (*QueueAffinity, error) {
	buf := make([]byte, affinityBufSize)

	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	defer pinner.Unpin()

	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: qid,
		Addr:    uint64(uintptr(unsafe.Pointer(&buf[0]))),
		Len:     uint16(len(buf)),
	}

	cmdOp := d.ctrlOp(ublkUCmdGetQueueAffinity, CmdGetQueueAffinity)
	if err := d.ctrlCmdWithPayload(cmdOp, &cmd); err != nil {
		return nil, fmt.Errorf("get queue affinity: %w", err)
	}

	return &QueueAffinity{mask: buf}, nil
}

// setQueueAffinity fetches and applies the CPU affinity for the given queue
// to the current OS thread. Must be called after runtime.LockOSThread().
// Failures are non-fatal (the goroutine runs without affinity pinning).
func (d *Device) setQueueAffinity(qid uint16) {
	aff, err := d.GetQueueAffinity(qid)
	if err != nil {
		return
	}
	cpus := aff.CPUs()
	if len(cpus) == 0 {
		return
	}
	setThreadAffinity(aff.mask)
}

// setThreadAffinity pins the current OS thread to the given CPU mask.
func setThreadAffinity(mask []byte) error {
	_, _, errno := syscall.RawSyscall(sysSchedSetaffinity,
		0, // 0 = current thread
		uintptr(len(mask)),
		uintptr(unsafe.Pointer(&mask[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
