package ublk

import (
	"fmt"
	"runtime"
	"syscall"
)

// targetIOFlag distinguishes target IO CQEs from ublk IO command CQEs.
const targetIOFlag = uint64(1) << 63

// ZeroCopyHandler processes IO requests using zero-copy buffer access.
// The handler receives a ZeroCopyRequest which provides methods to perform
// fixed-buffer IO on the request's kernel buffer without data copying.
type ZeroCopyHandler interface {
	HandleIO(req *ZeroCopyRequest) error
}

// ZeroCopyHandlerFunc adapts a function as a ZeroCopyHandler.
type ZeroCopyHandlerFunc func(req *ZeroCopyRequest) error

func (f ZeroCopyHandlerFunc) HandleIO(req *ZeroCopyRequest) error {
	return f(req)
}

// ZeroCopyRequest represents an IO request with zero-copy buffer access.
// The request's kernel buffer is registered in the io_uring fixed buffer
// table at BufIndex, and can be used with ReadFixed/WriteFixed.
type ZeroCopyRequest struct {
	Op          IOOp
	Flags       uint32
	StartSector uint64
	NrSectors   uint32
	Tag         uint16
	QueueID     uint16

	// BufIndex is the io_uring fixed buffer index for this request's
	// kernel buffer. Use with ReadFixed/WriteFixed.
	BufIndex uint16

	ring   queueRing
	charFd int32
	dev    *Device
}

// ReadFixed reads from fd at the given file offset into this request's
// zero-copy buffer. Used for host READ requests: read from backing storage
// into the buffer that will be returned to the host.
func (r *ZeroCopyRequest) ReadFixed(fd int32, fileOffset int64, size uint32) error {
	sqe := r.ring.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	prepReadFixed(sqe, fd, fileOffset, size, r.BufIndex)
	sqeSetU64(sqe, sqeOffUserData, targetIOFlag|uint64(r.Tag))

	if err := r.ring.Submit(); err != nil {
		return err
	}

	res, err := r.waitTargetCQE()
	if err != nil {
		return err
	}
	if res < 0 {
		return fmt.Errorf("READ_FIXED failed: %w", errnoFromResult(res))
	}
	return nil
}

// WriteFixed writes from this request's zero-copy buffer to fd at the given
// file offset. Used for host WRITE requests: write the host's data from the
// buffer to backing storage.
func (r *ZeroCopyRequest) WriteFixed(fd int32, fileOffset int64, size uint32) error {
	sqe := r.ring.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	prepWriteFixed(sqe, fd, fileOffset, size, r.BufIndex)
	sqeSetU64(sqe, sqeOffUserData, targetIOFlag|uint64(r.Tag))

	if err := r.ring.Submit(); err != nil {
		return err
	}

	res, err := r.waitTargetCQE()
	if err != nil {
		return err
	}
	if res < 0 {
		return fmt.Errorf("WRITE_FIXED failed: %w", errnoFromResult(res))
	}
	return nil
}

// waitTargetCQE waits for the target IO CQE, buffering any IO command
// CQEs that arrive in the meantime.
func (r *ZeroCopyRequest) waitTargetCQE() (int32, error) {
	for {
		cqe, err := r.ring.WaitCQE()
		if err != nil {
			return 0, err
		}
		ud := cqe.UserData
		res := cqe.Res
		r.ring.SeenCQE(cqe)

		if ud&targetIOFlag != 0 {
			return res, nil
		}
		// An IO command CQE arrived for another tag while we were
		// waiting. This shouldn't normally happen since we process
		// one request at a time, but handle it gracefully.
	}
}

// ServeZeroCopy starts the device in zero-copy mode and serves IO requests.
// The device must have been created with FlagSupportZeroCopy.
// Uses UBLK_F_AUTO_BUF_REG for automatic buffer registration.
func (d *Device) ServeZeroCopy(h ZeroCopyHandler) error {
	if d.info.Flags&FlagSupportZeroCopy == 0 {
		return fmt.Errorf("device not created with FlagSupportZeroCopy")
	}

	return d.serve(int(d.info.NrHwQueues), func(qid uint16, ready chan<- error) error {
		return d.serveQueueZeroCopy(qid, h, ready)
	})
}

func (d *Device) serveQueueZeroCopy(qid uint16, h ZeroCopyHandler, ready chan<- error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	d.setQueueAffinity(qid)

	depth := int(d.info.QueueDepth)

	ring, err := newIOURing(uint32(depth)*4, false)
	if err != nil {
		ready <- err
		return err
	}
	d.registerIORing(ring)
	defer func() { _ = ring.Close() }()

	if err := ring.RegisterSparseBuffers(uint32(depth)); err != nil {
		ready <- fmt.Errorf("register sparse buffers: %w", err)
		return err
	}

	cmdBufSize := ublkMaxCmdBufSize(d.info.QueueDepth)
	cmdBufOffset := int64(ublkSrvCmdBufOffset) + int64(qid)*int64(ublkMaxCmdBufSize(ublkMaxQueueDepth))

	cmdBuf, err := syscall.Mmap(
		int(d.charFile.Fd()),
		cmdBufOffset,
		int(cmdBufSize),
		syscall.PROT_READ,
		syscall.MAP_SHARED|mmapPopulateFlag,
	)
	if err != nil {
		ready <- fmt.Errorf("mmap cmd buf: %w", err)
		return err
	}
	defer func() { _ = syscall.Munmap(cmdBuf) }()

	if err := d.submitInitialFetchesAutoBuf(ring, qid); err != nil {
		ready <- err
		return err
	}

	if err := ring.Submit(); err != nil {
		ready <- fmt.Errorf("submit initial fetches: %w", err)
		return err
	}

	ready <- nil

	return d.runZeroCopyQueueLoop(ring, qid, int32(d.charFile.Fd()), func(tag uint16) ioDesc {
		return loadIODesc(cmdBuf, tag)
	}, h)
}

func (d *Device) submitFetchAutoBuf(ring queueRing, qid, tag uint16) error {
	sqe := ring.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	cmdOp := d.ioOp(ublkUIoFetchReq, IoFetchReq)
	prepUringCmdAutoBuf(sqe, cmdOp, int32(d.charFile.Fd()), qid, tag, 0, tag)
	sqeSetU64(sqe, sqeOffUserData, uint64(tag))
	return nil
}

func (d *Device) submitCommitAndFetchAutoBuf(ring queueRing, qid, tag uint16, result int32) error {
	sqe := ring.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	cmdOp := d.ioOp(ublkUIoCommitAndFetchReq, IoCommitAndFetchReq)
	prepUringCmdAutoBuf(sqe, cmdOp, int32(d.charFile.Fd()), qid, tag, result, tag)
	sqeSetU64(sqe, sqeOffUserData, uint64(tag))
	return nil
}
