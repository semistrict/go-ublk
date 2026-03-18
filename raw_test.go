//go:build linux

package ublk

import (
	"encoding/binary"
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

// TestRawAddDev bypasses all library code and does a raw io_uring ADD_DEV
// to isolate exactly where things fail.
func TestRawAddDev(t *testing.T) {
	if syscall.Getuid() != 0 {
		t.Skip("requires root")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 1. Open /dev/ublk-control
	ctrlFd, err := syscall.Open("/dev/ublk-control", syscall.O_RDWR, 0)
	if err != nil {
		t.Skipf("ublk not available: %v", err)
	}
	defer func() { _ = syscall.Close(ctrlFd) }()
	t.Logf("ublk-control fd=%d", ctrlFd)

	// 2. Check IORING_SETUP_SQE128 value from kernel headers
	// Ubuntu 24.04 kernel 6.8: IORING_SETUP_SQE128 = 1 << 10
	const sqe128Flag = 1 << 10

	// 3. Create io_uring with SQE128
	type ioURingParams struct {
		SqEntries    uint32
		CqEntries    uint32
		Flags        uint32
		SqThreadCPU  uint32
		SqThreadIdle uint32
		Features     uint32
		WqFd         uint32
		Resv         [3]uint32
		SqOff        struct {
			Head, Tail, RingMask, RingEntries, Flags, Dropped, Array, Resv1 uint32
			UserAddr                                                        uint64
		}
		CqOff struct {
			Head, Tail, RingMask, RingEntries, Overflow, Cqes, Flags, Resv1 uint32
			UserAddr                                                        uint64
		}
	}
	var p ioURingParams
	p.Flags = sqe128Flag

	ringFd, _, errno := syscall.RawSyscall(425, 4, uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		t.Fatalf("io_uring_setup: %v", errno)
	}
	defer func() { _ = syscall.Close(int(ringFd)) }()
	t.Logf("ring fd=%d sq_entries=%d cq_entries=%d features=0x%x", ringFd, p.SqEntries, p.CqEntries, p.Features)

	// 4. mmap rings
	sqLen := int(p.SqOff.Array) + int(p.SqEntries)*4
	sqRing, err := syscall.Mmap(int(ringFd), 0, sqLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		t.Fatalf("mmap sq ring: %v", err)
	}
	defer func() { _ = syscall.Munmap(sqRing) }()

	cqeSize := 16
	cqLen := int(p.CqOff.Cqes) + int(p.CqEntries)*cqeSize
	cqRing, err := syscall.Mmap(int(ringFd), 0x8000000, cqLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		t.Fatalf("mmap cq ring: %v", err)
	}
	defer func() { _ = syscall.Munmap(cqRing) }()

	sqesLen := int(p.SqEntries) * 128 // 128 bytes per SQE128
	sqes, err := syscall.Mmap(int(ringFd), 0x10000000, sqesLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		t.Fatalf("mmap sqes: %v", err)
	}
	defer func() { _ = syscall.Munmap(sqes) }()

	// 5. Build DevInfo buffer
	type devInfo struct {
		NrHwQueues    uint16
		QueueDepth    uint16
		State         uint16
		Pad0          uint16
		MaxIOBufBytes uint32
		DevID         uint32
		UblksrvPID    int32
		Pad1          uint32
		Flags         uint64
		UblksrvFlags  uint64
		OwnerUID      uint32
		OwnerGID      uint32
		Reserved1     uint64
		Reserved2     uint64
	}
	info := devInfo{
		NrHwQueues:    1,
		QueueDepth:    32,
		MaxIOBufBytes: 512 * 1024,
		DevID:         0xFFFFFFFF,
		UblksrvPID:    int32(syscall.Getpid()),
		Flags:         (1 << 7) | (1 << 6), // USER_COPY | CMD_IOCTL_ENCODE
	}
	infoBuf := make([]byte, unsafe.Sizeof(info))
	copy(infoBuf, unsafe.Slice((*byte)(unsafe.Pointer(&info)), unsafe.Sizeof(info)))

	var pinner runtime.Pinner
	pinner.Pin(&infoBuf[0])
	defer pinner.Unpin()

	t.Logf("sizeof(devInfo)=%d flags=0x%x", unsafe.Sizeof(info), info.Flags)

	// 6. Build UBLK_U_CMD_ADD_DEV = _IOWR('u', 0x04, ublksrv_ctrl_cmd)
	// sizeof(ublksrv_ctrl_cmd) = 32
	cmdOp := uint32((3 << 30) | (32 << 16) | (uint32('u') << 8) | 0x04)
	t.Logf("cmdOp=0x%08x", cmdOp)

	// 7. Fill SQE[0]
	sqe := sqes[0:128]
	clear(sqe)

	sqe[0] = 46                                             // opcode = IORING_OP_URING_CMD
	binary.LittleEndian.PutUint32(sqe[4:8], uint32(ctrlFd)) // fd
	binary.LittleEndian.PutUint32(sqe[8:12], cmdOp)         // cmd_op (low 32 of off)
	binary.LittleEndian.PutUint32(sqe[12:16], 0)            // __pad1 (high 32 of off)
	binary.LittleEndian.PutUint64(sqe[32:40], 0xDEAD)       // user_data
	// cmd payload at offset 48: ublksrv_ctrl_cmd
	binary.LittleEndian.PutUint32(sqe[48:52], 0xFFFFFFFF)                                   // dev_id = -1
	binary.LittleEndian.PutUint16(sqe[52:54], 0xFFFF)                                       // queue_id = -1
	binary.LittleEndian.PutUint16(sqe[54:56], uint16(unsafe.Sizeof(info)))                  // len
	binary.LittleEndian.PutUint64(sqe[56:64], uint64(uintptr(unsafe.Pointer(&infoBuf[0])))) // addr
	// data[0] at offset 64
	binary.LittleEndian.PutUint64(sqe[64:72], 0) // data[0]

	t.Logf("SQE[0:16]: % 02x", sqe[0:16])
	t.Logf("SQE[48:80]: % 02x", sqe[48:80])

	// 8. Set sq_array[0] = 0, advance sq_tail
	binary.LittleEndian.PutUint32(sqRing[p.SqOff.Array:], 0)
	binary.LittleEndian.PutUint32(sqRing[p.SqOff.Tail:], 1)

	// 9. Submit
	ret, _, errno := syscall.Syscall6(426, ringFd, 1, 0, 0, 0, 0)
	t.Logf("submit: ret=%d errno=%v", ret, errno)
	if errno != 0 {
		t.Fatalf("submit failed: %v", errno)
	}

	// 10. Wait for CQE
	_, _, errno = syscall.Syscall6(426, ringFd, 0, 1, 1, 0, 0)
	if errno != 0 && errno != syscall.EINTR {
		t.Fatalf("wait failed: %v", errno)
	}

	// 11. Read CQE
	cqHead := binary.LittleEndian.Uint32(cqRing[p.CqOff.Head:])
	cqTail := binary.LittleEndian.Uint32(cqRing[p.CqOff.Tail:])
	t.Logf("CQ head=%d tail=%d", cqHead, cqTail)

	if cqHead == cqTail {
		t.Fatal("no CQE received")
	}

	cqMask := binary.LittleEndian.Uint32(cqRing[p.CqOff.RingMask:])
	idx := cqHead & cqMask
	cqeOff := p.CqOff.Cqes + idx*uint32(cqeSize)
	userData := binary.LittleEndian.Uint64(cqRing[cqeOff:])
	res := int32(binary.LittleEndian.Uint32(cqRing[cqeOff+8:]))
	t.Logf("CQE: user_data=0x%x res=%d (errno=%d)", userData, res, -res)

	if res < 0 {
		t.Logf("ADD_DEV failed with errno %d (%v)", -res, syscall.Errno(-res))

		// Try stripping CMD_IOCTL_ENCODE flag
		t.Log("Retrying without CMD_IOCTL_ENCODE...")
		info.Flags = 1 << 7 // USER_COPY only
		copy(infoBuf, unsafe.Slice((*byte)(unsafe.Pointer(&info)), unsafe.Sizeof(info)))

		// Advance CQ head
		binary.LittleEndian.PutUint32(cqRing[p.CqOff.Head:], cqHead+1)

		// Rebuild SQE
		clear(sqe)
		sqe[0] = 46
		binary.LittleEndian.PutUint32(sqe[4:8], uint32(ctrlFd))
		binary.LittleEndian.PutUint32(sqe[8:12], cmdOp)
		binary.LittleEndian.PutUint64(sqe[32:40], 0xBEEF)
		binary.LittleEndian.PutUint32(sqe[48:52], 0xFFFFFFFF)
		binary.LittleEndian.PutUint16(sqe[52:54], 0xFFFF)
		binary.LittleEndian.PutUint16(sqe[54:56], uint16(unsafe.Sizeof(info)))
		binary.LittleEndian.PutUint64(sqe[56:64], uint64(uintptr(unsafe.Pointer(&infoBuf[0]))))

		tail := binary.LittleEndian.Uint32(sqRing[p.SqOff.Tail:])
		binary.LittleEndian.PutUint32(sqRing[p.SqOff.Array+4*(tail&binary.LittleEndian.Uint32(sqRing[p.SqOff.RingMask:])):], 0)
		binary.LittleEndian.PutUint32(sqRing[p.SqOff.Tail:], tail+1)

		ret, _, errno = syscall.Syscall6(426, ringFd, 1, 0, 0, 0, 0)
		t.Logf("retry submit: ret=%d errno=%v", ret, errno)
		_, _, errno = syscall.Syscall6(426, ringFd, 0, 1, 1, 0, 0)
		if errno != 0 && errno != syscall.EINTR {
			t.Fatalf("retry wait failed: %v", errno)
		}

		cqHead = binary.LittleEndian.Uint32(cqRing[p.CqOff.Head:])
		cqTail = binary.LittleEndian.Uint32(cqRing[p.CqOff.Tail:])
		if cqHead != cqTail {
			idx = cqHead & cqMask
			cqeOff = p.CqOff.Cqes + idx*uint32(cqeSize)
			userData = binary.LittleEndian.Uint64(cqRing[cqeOff:])
			res = int32(binary.LittleEndian.Uint32(cqRing[cqeOff+8:]))
			t.Logf("retry CQE: user_data=0x%x res=%d (errno=%d)", userData, res, -res)
		}

		if res < 0 {
			// Also try without USER_COPY
			t.Log("Retrying with flags=0...")
			info.Flags = 0
			copy(infoBuf, unsafe.Slice((*byte)(unsafe.Pointer(&info)), unsafe.Sizeof(info)))

			binary.LittleEndian.PutUint32(cqRing[p.CqOff.Head:], cqHead+1)
			clear(sqe)
			sqe[0] = 46
			binary.LittleEndian.PutUint32(sqe[4:8], uint32(ctrlFd))
			binary.LittleEndian.PutUint32(sqe[8:12], cmdOp)
			binary.LittleEndian.PutUint64(sqe[32:40], 0xCAFE)
			binary.LittleEndian.PutUint32(sqe[48:52], 0xFFFFFFFF)
			binary.LittleEndian.PutUint16(sqe[52:54], 0xFFFF)
			binary.LittleEndian.PutUint16(sqe[54:56], uint16(unsafe.Sizeof(info)))
			binary.LittleEndian.PutUint64(sqe[56:64], uint64(uintptr(unsafe.Pointer(&infoBuf[0]))))

			tail = binary.LittleEndian.Uint32(sqRing[p.SqOff.Tail:])
			binary.LittleEndian.PutUint32(sqRing[p.SqOff.Array+4*(tail&binary.LittleEndian.Uint32(sqRing[p.SqOff.RingMask:])):], 0)
			binary.LittleEndian.PutUint32(sqRing[p.SqOff.Tail:], tail+1)

			if _, _, errno = syscall.Syscall6(426, ringFd, 1, 0, 0, 0, 0); errno != 0 {
				t.Fatalf("flags=0 submit failed: %v", errno)
			}
			_, _, errno = syscall.Syscall6(426, ringFd, 0, 1, 1, 0, 0)
			if errno != 0 && errno != syscall.EINTR {
				t.Fatalf("flags=0 wait failed: %v", errno)
			}

			cqHead = binary.LittleEndian.Uint32(cqRing[p.CqOff.Head:])
			cqTail = binary.LittleEndian.Uint32(cqRing[p.CqOff.Tail:])
			if cqHead != cqTail {
				idx = cqHead & cqMask
				cqeOff = p.CqOff.Cqes + idx*uint32(cqeSize)
				userData = binary.LittleEndian.Uint64(cqRing[cqeOff:])
				res = int32(binary.LittleEndian.Uint32(cqRing[cqeOff+8:]))
				t.Logf("flags=0 CQE: user_data=0x%x res=%d (errno=%d)", userData, res, -res)
			}
		}

		if res < 0 {
			t.Fatalf("all ADD_DEV attempts failed, last errno=%d (%v)", -res, syscall.Errno(-res))
		}
	}

	// Read back device info
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&info)), unsafe.Sizeof(info)), infoBuf)
	t.Logf("device created: id=%d state=%d flags=0x%x", info.DevID, info.State, info.Flags)

	// Cleanup: DEL_DEV
	delOp := uint32((3 << 30) | (32 << 16) | (uint32('u') << 8) | 0x05)
	cqHead = binary.LittleEndian.Uint32(cqRing[p.CqOff.Head:])
	binary.LittleEndian.PutUint32(cqRing[p.CqOff.Head:], cqHead+1)

	clear(sqe)
	sqe[0] = 46
	binary.LittleEndian.PutUint32(sqe[4:8], uint32(ctrlFd))
	binary.LittleEndian.PutUint32(sqe[8:12], delOp)
	binary.LittleEndian.PutUint64(sqe[32:40], 0xDE1)
	binary.LittleEndian.PutUint32(sqe[48:52], info.DevID)
	binary.LittleEndian.PutUint16(sqe[52:54], 0xFFFF)

	tail := binary.LittleEndian.Uint32(sqRing[p.SqOff.Tail:])
	binary.LittleEndian.PutUint32(sqRing[p.SqOff.Array+4*(tail&binary.LittleEndian.Uint32(sqRing[p.SqOff.RingMask:])):], 0)
	binary.LittleEndian.PutUint32(sqRing[p.SqOff.Tail:], tail+1)

	if _, _, errno = syscall.Syscall6(426, ringFd, 1, 1, 1, 0, 0); errno != 0 {
		t.Fatalf("DEL_DEV submit failed: %v", errno)
	}
	t.Log("DEL_DEV submitted")
}
