package ublk

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Minimal io_uring implementation for ublk. Supports regular SQEs and SQE128
// (for ublk control commands which embed struct data in the SQE cmd field).

// Linux syscall numbers for io_uring (arch-independent since 5.1).
// See include/uapi/asm-generic/unistd.h.
const (
	sysIoURingSetup    = 425 // io_uring_setup(entries, params) → ring fd
	sysIoURingEnter    = 426 // io_uring_enter(fd, to_submit, min_complete, flags, ...) → submitted count
	sysIoURingRegister = 427 // io_uring_register(fd, opcode, arg, nr_args) → 0 or error
)

var ioURingEnter = func(fd int, toSubmit, minComplete, flags uint32) (uint32, syscall.Errno) {
	r1, _, errno := syscall.Syscall6(sysIoURingEnter,
		uintptr(fd),
		uintptr(toSubmit),
		uintptr(minComplete),
		uintptr(flags),
		0, 0)
	return uint32(r1), errno
}

// io_uring setup flags (IORING_SETUP_*). Passed in io_uring_params.flags.
const (
	ioringSetupCQSize      = 1 << 3  // IORING_SETUP_CQSIZE: userspace provides completion queue size
	ioringSetupCoopTaskrun = 1 << 8  // IORING_SETUP_COOP_TASKRUN: run task_work cooperatively in enter syscalls
	ioringSetupSQE128      = 1 << 10 // IORING_SETUP_SQE128: use 128-byte SQEs (needed for ublk control cmds)
)

// io_uring_enter flags (IORING_ENTER_*).
const (
	ioringEnterGetEvents = 1 << 0 // IORING_ENTER_GETEVENTS: wait for min_complete CQEs
)

// io_uring opcodes (IORING_OP_*). Set in sqe.opcode.
const (
	ioringOpReadFixed  = 4  // IORING_OP_READ_FIXED: read from fd into a registered fixed buffer
	ioringOpWriteFixed = 5  // IORING_OP_WRITE_FIXED: write from a registered fixed buffer to fd
	ioringOpURingCmd   = 46 // IORING_OP_URING_CMD: passthrough command to a file's uring_cmd handler
)

// io_uring register opcodes and flags for buffer registration.
const (
	ioringRegisterBuffers2   = 15     // IORING_REGISTER_BUFFERS2: register buffers with rsrc_register struct
	ioringRsrcRegisterSparse = 1 << 0 // IORING_RSRC_REGISTER_SPARSE: allocate sparse (empty) buffer slots
)

// mmap offsets for io_uring ring memory regions.
// Passed as the offset argument to mmap(2) on the io_uring fd.
const (
	ioringOffSQRing = 0          // SQ ring: head, tail, mask, flags, array
	ioringOffCQRing = 0x8000000  // CQ ring: head, tail, mask, CQE array
	ioringOffSQEs   = 0x10000000 // SQE array: submission queue entries
)

// ioURingRsrcRegister mirrors `struct io_uring_rsrc_register` from
// `include/uapi/linux/io_uring.h`, used with IORING_REGISTER_BUFFERS2 to
// register a (possibly sparse) buffer table.
type ioURingRsrcRegister struct {
	Nr    uint32 // number of buffer slots to register
	Flags uint32 // IORING_RSRC_REGISTER_SPARSE for empty slots
	Resv2 uint64 // must be zero
	Data  uint64 // pointer to iovec array (0 for sparse)
	Tags  uint64 // pointer to tags array (0 for sparse)
}

// ioURingSQE mirrors the byte layout of `struct io_uring_sqe` from
// `include/uapi/linux/io_uring.h`.
//
// Base SQE is 64 bytes. With SQE128, the total is 128 bytes.
// For IORING_OP_URING_CMD, the cmd payload starts at offset 48 (overlapping
// addr3/pad2 in the base layout) and extends through byte 127 (80 bytes total
// with SQE128). We use a flat [128]byte and access fields via unsafe offsets
// to match the kernel layout exactly.
type ioURingSQE [128]byte

// SQE field offsets matching the kernel's io_uring_sqe layout.
const (
	sqeOffOpcode      = 0
	sqeOffFlags       = 1
	sqeOffIoPrio      = 2
	sqeOffFd          = 4
	sqeOffOff         = 8  // union: off, addr2, cmd_op
	sqeOffAddr        = 16 // union: addr, splice_off_in
	sqeOffLen         = 24
	sqeOffOpFlags     = 28 // union: rw_flags, fsync_flags, etc.
	sqeOffUserData    = 32
	sqeOffBufIndex    = 40 // union: buf_index, buf_group
	sqeOffPersonality = 42
	sqeOffSpliceFdIn  = 44
	sqeOffCmd         = 48 // uring_cmd inline data (80 bytes with SQE128)
)

func sqeSetU8(sqe *ioURingSQE, off int, v uint8)   { sqe[off] = v }
func sqeSetU16(sqe *ioURingSQE, off int, v uint16) { *(*uint16)(unsafe.Pointer(&sqe[off])) = v }
func sqeSetU32(sqe *ioURingSQE, off int, v uint32) { *(*uint32)(unsafe.Pointer(&sqe[off])) = v }
func sqeSetI32(sqe *ioURingSQE, off int, v int32)  { *(*int32)(unsafe.Pointer(&sqe[off])) = v }
func sqeSetU64(sqe *ioURingSQE, off int, v uint64) { *(*uint64)(unsafe.Pointer(&sqe[off])) = v }

// ioURingCQE mirrors `struct io_uring_cqe` from
// `include/uapi/linux/io_uring.h`.
type ioURingCQE struct {
	UserData uint64 // sqe->user_data value passed back
	Res      int32  // result code for this event
	Flags    uint32 // IORING_CQE_F_* flags
}

// ioURingParams mirrors `struct io_uring_params` from
// `include/uapi/linux/io_uring.h`. Passed to io_uring_setup(2) and copied
// back with updated info on success.
type ioURingParams struct {
	SqEntries    uint32          // number of submission queue entries
	CqEntries    uint32          // number of completion queue entries
	Flags        uint32          // IORING_SETUP_* flags
	SqThreadCPU  uint32          // CPU for the SQ poll thread when IORING_SETUP_SQ_AFF is set
	SqThreadIdle uint32          // milliseconds before the SQ poll thread goes to sleep
	Features     uint32          // IORING_FEAT_* flags returned by the kernel
	WqFd         uint32          // existing io_uring fd to attach to when IORING_SETUP_ATTACH_WQ is set
	Resv         [3]uint32       // must be zero
	SqOff        ioSqRingOffsets // offsets for the submission ring mmap
	CqOff        ioCqRingOffsets // offsets for the completion ring mmap
}

// ioSqRingOffsets mirrors `struct io_sqring_offsets` from
// `include/uapi/linux/io_uring.h`. Filled with the offsets used to mmap and
// address the SQ ring.
type ioSqRingOffsets struct {
	Head        uint32 // offset of the SQ head index
	Tail        uint32 // offset of the SQ tail index
	RingMask    uint32 // offset of the ring mask used for index wrapping
	RingEntries uint32 // offset of the ring entry count
	Flags       uint32 // offset of sq_ring->flags (IORING_SQ_* bits)
	Dropped     uint32 // offset of the dropped-submission counter
	Array       uint32 // offset of the SQ index array
	Resv1       uint32 // reserved
	UserAddr    uint64 // user-provided SQ ring address with IORING_SETUP_NO_MMAP
}

// ioCqRingOffsets mirrors `struct io_cqring_offsets` from
// `include/uapi/linux/io_uring.h`. Filled with the offsets used to mmap and
// address the CQ ring.
type ioCqRingOffsets struct {
	Head        uint32 // offset of the CQ head index
	Tail        uint32 // offset of the CQ tail index
	RingMask    uint32 // offset of the ring mask used for index wrapping
	RingEntries uint32 // offset of the ring entry count
	Overflow    uint32 // offset of the CQ overflow counter
	Cqes        uint32 // offset of the CQE array
	Flags       uint32 // offset of cq_ring->flags
	Resv1       uint32 // reserved
	UserAddr    uint64 // user-provided CQ ring address with IORING_SETUP_NO_MMAP
}

// ioURing is the local runtime wrapper around the three mmap regions that
// back a ring instance. These fields are internal bookkeeping, not a direct
// kernel UAPI struct.
type ioURing struct {
	fd     int  // io_uring file descriptor returned by io_uring_setup
	sqe128 bool // whether the ring was created with IORING_SETUP_SQE128

	// SQ ring (backed by sqRingMem mmap)
	sqRingMem  []byte  // mapped SQ ring region at IORING_OFF_SQ_RING
	sqHeadOff  uintptr // byte offset of sq_ring->head within sqRingMem
	sqTailOff  uintptr // byte offset of sq_ring->tail within sqRingMem
	sqMask     uint32  // cached SQ ring mask
	sqArrayOff uintptr // byte offset of the SQ index array within sqRingMem
	sqEntries  uint32  // number of SQ entries returned by the kernel
	sqSubmit   uint32  // local cursor of SQEs already submitted to the kernel

	// SQE array (backed by sqesMem mmap)
	sqesMem []byte  // mapped SQE array at IORING_OFF_SQES
	sqeSize uintptr // 64 for normal SQEs, 128 for SQE128

	// CQ ring (backed by cqRingMem mmap)
	cqRingMem []byte  // mapped CQ ring region at IORING_OFF_CQ_RING
	cqHeadOff uintptr // byte offset of cq_ring->head within cqRingMem
	cqTailOff uintptr // byte offset of cq_ring->tail within cqRingMem
	cqMask    uint32  // cached CQ ring mask
	cqesOff   uintptr // byte offset of the CQE array within cqRingMem
	cqEntries uint32  // number of CQ entries returned by the kernel
}

func newIOURing(entries uint32, sqe128 bool) (*ioURing, error) {
	var params ioURingParams
	params.Flags = ioringSetupCQSize | ioringSetupCoopTaskrun
	params.CqEntries = entries * 2
	if sqe128 {
		params.Flags |= ioringSetupSQE128
	}

	fd, _, errno := syscall.RawSyscall(sysIoURingSetup,
		uintptr(entries),
		uintptr(unsafe.Pointer(&params)),
		0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	ring := &ioURing{
		fd:        int(fd),
		sqe128:    sqe128,
		sqEntries: params.SqEntries,
		cqEntries: params.CqEntries,
	}

	if sqe128 {
		ring.sqeSize = 128
	} else {
		ring.sqeSize = 64
	}

	if err := ring.mmapRings(&params); err != nil {
		_ = syscall.Close(ring.fd)
		return nil, err
	}

	return ring, nil
}

func mmapShared(fd int, offset int64, length int) ([]byte, error) {
	return syscall.Mmap(fd, offset, length, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
}

func (r *ioURing) mmapRings(p *ioURingParams) error {
	var err error

	// Map SQ ring
	sqRingLen := int(p.SqOff.Array) + int(p.SqEntries)*4
	r.sqRingMem, err = mmapShared(r.fd, ioringOffSQRing, sqRingLen)
	if err != nil {
		return fmt.Errorf("mmap sq ring: %w", err)
	}
	r.sqHeadOff = uintptr(p.SqOff.Head)
	r.sqTailOff = uintptr(p.SqOff.Tail)
	r.sqMask = *(*uint32)(unsafe.Pointer(&r.sqRingMem[p.SqOff.RingMask]))
	r.sqArrayOff = uintptr(p.SqOff.Array)

	// Map CQ ring
	cqRingLen := int(p.CqOff.Cqes) + int(p.CqEntries)*int(unsafe.Sizeof(ioURingCQE{}))
	r.cqRingMem, err = mmapShared(r.fd, ioringOffCQRing, cqRingLen)
	if err != nil {
		_ = syscall.Munmap(r.sqRingMem)
		return fmt.Errorf("mmap cq ring: %w", err)
	}
	r.cqHeadOff = uintptr(p.CqOff.Head)
	r.cqTailOff = uintptr(p.CqOff.Tail)
	r.cqMask = *(*uint32)(unsafe.Pointer(&r.cqRingMem[p.CqOff.RingMask]))
	r.cqesOff = uintptr(p.CqOff.Cqes)

	// Map SQEs
	sqesLen := int(p.SqEntries) * int(r.sqeSize)
	r.sqesMem, err = mmapShared(r.fd, ioringOffSQEs, sqesLen)
	if err != nil {
		_ = syscall.Munmap(r.cqRingMem)
		_ = syscall.Munmap(r.sqRingMem)
		return fmt.Errorf("mmap sqes: %w", err)
	}

	return nil
}

func (r *ioURing) Close() error {
	var err error
	if r.fd >= 0 {
		err = errors.Join(err, syscall.Close(r.fd))
	}
	if r.sqesMem != nil {
		err = errors.Join(err, syscall.Munmap(r.sqesMem))
	}
	if r.cqRingMem != nil {
		err = errors.Join(err, syscall.Munmap(r.cqRingMem))
	}
	if r.sqRingMem != nil {
		err = errors.Join(err, syscall.Munmap(r.sqRingMem))
	}
	return err
}

// Interrupt closes the fd to unblock any goroutine stuck in WaitCQE.
// The caller must NOT access the ring after this. The serve goroutine
// should call Close() to unmap memory on its way out.
func (r *ioURing) Interrupt() {
	_ = syscall.Close(r.fd)
	r.fd = -1
}

func (r *ioURing) sqHead() *uint32 {
	return (*uint32)(unsafe.Pointer(&r.sqRingMem[r.sqHeadOff]))
}

func (r *ioURing) sqTail() *uint32 {
	return (*uint32)(unsafe.Pointer(&r.sqRingMem[r.sqTailOff]))
}

func (r *ioURing) cqHead() *uint32 {
	return (*uint32)(unsafe.Pointer(&r.cqRingMem[r.cqHeadOff]))
}

func (r *ioURing) cqTail() *uint32 {
	return (*uint32)(unsafe.Pointer(&r.cqRingMem[r.cqTailOff]))
}

func (r *ioURing) sqArrayEntry(idx uint32) *uint32 {
	off := r.sqArrayOff + uintptr(idx)*4
	return (*uint32)(unsafe.Pointer(&r.sqRingMem[off]))
}

func (r *ioURing) sqeEntry(idx uint32) *ioURingSQE {
	off := uintptr(idx) * r.sqeSize
	return (*ioURingSQE)(unsafe.Pointer(&r.sqesMem[off]))
}

func (r *ioURing) sqeBytes(idx uint32) []byte {
	off := uintptr(idx) * r.sqeSize
	return r.sqesMem[off : off+r.sqeSize]
}

func (r *ioURing) cqeEntry(idx uint32) *ioURingCQE {
	off := r.cqesOff + uintptr(idx)*unsafe.Sizeof(ioURingCQE{})
	return (*ioURingCQE)(unsafe.Pointer(&r.cqRingMem[off]))
}

func (r *ioURing) GetSQE() *ioURingSQE {
	tail := atomic.LoadUint32(r.sqTail())
	head := atomic.LoadUint32(r.sqHead())

	if tail-head >= r.sqEntries {
		return nil
	}

	idx := tail & r.sqMask
	sqe := r.sqeEntry(idx)

	// Only clear the bytes that belong to this SQE slot. Queue rings use
	// 64-byte SQEs, while control rings can opt into 128-byte SQEs.
	clear(r.sqeBytes(idx))

	// Set the SQ array entry
	*r.sqArrayEntry(idx) = idx

	atomic.StoreUint32(r.sqTail(), tail+1)
	return sqe
}

func (r *ioURing) submitAndMaybeWait(minComplete uint32) error {
	for {
		tail := atomic.LoadUint32(r.sqTail())
		toSubmit := tail - r.sqSubmit

		if toSubmit == 0 && minComplete == 0 {
			return nil
		}

		flags := uint32(0)
		if minComplete > 0 {
			flags |= ioringEnterGetEvents
		}
		submitted, errno := ioURingEnter(r.fd, toSubmit, minComplete, flags)
		if errno != 0 {
			return fmt.Errorf("io_uring_enter submit: %w", errno)
		}
		if toSubmit > 0 && submitted == 0 {
			return fmt.Errorf("io_uring_enter submit: submitted 0 of %d SQEs", toSubmit)
		}
		r.sqSubmit += submitted
		if minComplete > 0 && r.TryCQE() == nil {
			continue
		}
		if tail-r.sqSubmit == 0 {
			return nil
		}
	}
}

func (r *ioURing) Submit() error {
	return r.submitAndMaybeWait(0)
}

func (r *ioURing) SubmitAndWaitCQE() (*ioURingCQE, error) {
	if err := r.submitAndMaybeWait(1); err != nil {
		return nil, err
	}
	for {
		if cqe := r.TryCQE(); cqe != nil {
			return cqe, nil
		}
		if err := r.submitAndMaybeWait(1); err != nil {
			return nil, err
		}
	}
}

func (r *ioURing) WaitCQE() (*ioURingCQE, error) {
	for {
		if cqe := r.TryCQE(); cqe != nil {
			return cqe, nil
		}

		// No CQE available, wait
		_, errno := ioURingEnter(r.fd, 0, 1, ioringEnterGetEvents)
		if errno != 0 {
			if errno == syscall.EINTR {
				runtime.Gosched()
				continue
			}
			return nil, fmt.Errorf("io_uring_enter wait: %w", errno)
		}
	}
}

func (r *ioURing) TryCQE() *ioURingCQE {
	head := atomic.LoadUint32(r.cqHead())
	tail := atomic.LoadUint32(r.cqTail())
	if head == tail {
		return nil
	}
	idx := head & r.cqMask
	return r.cqeEntry(idx)
}

func (r *ioURing) SeenCQE(cqe *ioURingCQE) {
	atomic.AddUint32(r.cqHead(), 1)
}

func (r *ioURing) AvailableSQEs() uint32 {
	tail := atomic.LoadUint32(r.sqTail())
	head := atomic.LoadUint32(r.sqHead())
	if tail-head >= r.sqEntries {
		return 0
	}
	return r.sqEntries - (tail - head)
}

// RegisterSparseBuffers registers a sparse fixed-buffer table with nr entries.
// Required for zero-copy mode.
func (r *ioURing) RegisterSparseBuffers(nr uint32) error {
	reg := ioURingRsrcRegister{
		Nr:    nr,
		Flags: ioringRsrcRegisterSparse,
	}
	_, _, errno := syscall.Syscall6(sysIoURingRegister,
		uintptr(r.fd),
		uintptr(ioringRegisterBuffers2),
		uintptr(unsafe.Pointer(&reg)),
		uintptr(unsafe.Sizeof(reg)),
		0, 0)
	if errno != 0 {
		return fmt.Errorf("register sparse buffers: %w", errno)
	}
	return nil
}

// prepUringCmd sets up an SQE for a URING_CMD operation (used for IO commands).
func prepUringCmd(sqe *ioURingSQE, cmdOp uint32, fd int32, qid, tag uint16, result int32, addr uint64) {
	sqeSetU8(sqe, sqeOffOpcode, ioringOpURingCmd)
	sqeSetI32(sqe, sqeOffFd, fd)
	sqeSetU64(sqe, sqeOffOff, uint64(cmdOp))

	cmd := (*ioCmd)(unsafe.Pointer(&sqe[sqeOffCmd]))
	cmd.QID = qid
	cmd.Tag = tag
	cmd.Result = result
	cmd.Addr = addr
}

// prepUringCmdAutoBuf sets up a URING_CMD SQE with auto-buf-reg data in sqe.Addr.
func prepUringCmdAutoBuf(sqe *ioURingSQE, cmdOp uint32, fd int32, qid, tag uint16, result int32, bufIndex uint16) {
	sqeSetU8(sqe, sqeOffOpcode, ioringOpURingCmd)
	sqeSetI32(sqe, sqeOffFd, fd)
	sqeSetU64(sqe, sqeOffOff, uint64(cmdOp))
	sqeSetU64(sqe, sqeOffAddr, autoBufRegToSQEAddr(bufIndex, 0))

	cmd := (*ioCmd)(unsafe.Pointer(&sqe[sqeOffCmd]))
	cmd.QID = qid
	cmd.Tag = tag
	cmd.Result = result
}

// prepReadFixed sets up a READ_FIXED SQE (read from fd into fixed buffer).
func prepReadFixed(sqe *ioURingSQE, fd int32, offset int64, size uint32, bufIndex uint16) {
	sqeSetU8(sqe, sqeOffOpcode, ioringOpReadFixed)
	sqeSetI32(sqe, sqeOffFd, fd)
	sqeSetU64(sqe, sqeOffOff, uint64(offset))
	sqeSetU64(sqe, sqeOffAddr, 0)
	sqeSetU32(sqe, sqeOffLen, size)
	sqeSetU16(sqe, sqeOffBufIndex, bufIndex)
}

// prepWriteFixed sets up a WRITE_FIXED SQE (write from fixed buffer to fd).
func prepWriteFixed(sqe *ioURingSQE, fd int32, offset int64, size uint32, bufIndex uint16) {
	sqeSetU8(sqe, sqeOffOpcode, ioringOpWriteFixed)
	sqeSetI32(sqe, sqeOffFd, fd)
	sqeSetU64(sqe, sqeOffOff, uint64(offset))
	sqeSetU64(sqe, sqeOffAddr, 0)
	sqeSetU32(sqe, sqeOffLen, size)
	sqeSetU16(sqe, sqeOffBufIndex, bufIndex)
}

// prepCtrlCmd sets up an SQE128 for a control command to /dev/ublk-control.
func prepCtrlCmd(sqe *ioURingSQE, cmdOp uint32, fd int32, ctrl *ctrlCmd) {
	sqeSetU8(sqe, sqeOffOpcode, ioringOpURingCmd)
	sqeSetI32(sqe, sqeOffFd, fd)
	sqeSetU64(sqe, sqeOffOff, uint64(cmdOp))

	dst := (*ctrlCmd)(unsafe.Pointer(&sqe[sqeOffCmd]))
	*dst = *ctrl
}

// autoBufRegToSQEAddr encodes ublk_auto_buf_reg into the sqe->addr format.
func autoBufRegToSQEAddr(index uint16, flags uint8) uint64 {
	return uint64(index) | (uint64(flags) << 16)
}
