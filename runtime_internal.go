package ublk

import (
	"errors"
	"fmt"
	"syscall"
)

type controlRing interface {
	GetSQE() *ioURingSQE
	Submit() error
	WaitCQE() (*ioURingCQE, error)
	SeenCQE(*ioURingCQE)
}

type queueRing interface {
	GetSQE() *ioURingSQE
	Submit() error
	SubmitAndWaitCQE() (*ioURingCQE, error)
	WaitCQE() (*ioURingCQE, error)
	TryCQE() *ioURingCQE
	SeenCQE(*ioURingCQE)
	AvailableSQEs() uint32
}

type ioDescLoader func(tag uint16) ioDesc
type ioDescSnapshotter func(tag uint16) [ioDescSize]byte

type userCopyReadWriter interface {
	readerAt
	writerAt
}

func submitCtrlCmdAndWait(r controlRing, fd int32, cmdOp uint32, cmd *ctrlCmd) (int32, error) {
	sqe := r.GetSQE()
	if sqe == nil {
		return 0, fmt.Errorf("no SQE available")
	}
	prepCtrlCmd(sqe, cmdOp, fd, cmd)
	sqeSetU64(sqe, sqeOffUserData, 1)

	if err := r.Submit(); err != nil {
		return 0, err
	}

	cqe, err := r.WaitCQE()
	if err != nil {
		return 0, err
	}
	res := cqe.Res
	r.SeenCQE(cqe)
	return res, nil
}

func (d *Device) activeUserCopyTarget() userCopyReadWriter {
	if d.userCopyData != nil {
		return d.userCopyData
	}
	return d.charFile
}

func (d *Device) submitInitialFetches(r queueRing, qid uint16) error {
	for tag := uint16(0); tag < d.info.QueueDepth; tag++ {
		if err := d.submitFetch(r, qid, tag); err != nil {
			return fmt.Errorf("initial fetch tag %d: %w", tag, err)
		}
	}
	return nil
}

func (d *Device) submitInitialFetchesAutoBuf(r queueRing, qid uint16) error {
	for tag := uint16(0); tag < d.info.QueueDepth; tag++ {
		if err := d.submitFetchAutoBuf(r, qid, tag); err != nil {
			return fmt.Errorf("initial fetch tag %d: %w", tag, err)
		}
	}
	return nil
}

func (d *Device) runUserQueueLoop(r queueRing, qid uint16, loadDesc ioDescLoader, snapshot ioDescSnapshotter, h Handler) error {
	debugEnabled := ioDebugEnabled()
	verifyDescEnabled := ioDebugVerifyDescEnabled()
	delaySeenEnabled := ioDebugDelaySeenEnabled()
	batchStatsEnabled := ioDebugBatchStatsEnabled()
	pendingCommit := make([]bool, d.info.QueueDepth)
	reqs := make([]Request, d.info.QueueDepth)
	var nextCQE *ioURingCQE
	var batchSubmitCalls uint64
	var batchCommitted uint64
	maxBatchCommitted := 0
	defer func() {
		if batchStatsEnabled {
			ioDebugf("batch stats q=%d submit_waits=%d committed=%d avg_batch=%.2f max_batch=%d",
				qid,
				batchSubmitCalls,
				batchCommitted,
				func() float64 {
					if batchSubmitCalls == 0 {
						return 0
					}
					return float64(batchCommitted) / float64(batchSubmitCalls)
				}(),
				maxBatchCommitted,
			)
		}
	}()

	flushQueued := func(batchQueued int) error {
		if batchQueued == 0 {
			return nil
		}
		if err := r.Submit(); err != nil {
			if errors.Is(err, syscall.EBADF) && d.isStopped() {
				return nil
			}
			return fmt.Errorf("submit commit batch: %w", err)
		}
		return nil
	}

	for {
		select {
		case <-d.stopped:
			return nil
		default:
		}

		cqe := nextCQE
		nextCQE = nil
		if cqe == nil {
			var err error
			cqe, err = r.WaitCQE()
			if err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if errors.Is(err, syscall.EBADF) && d.isStopped() {
					return nil
				}
				return fmt.Errorf("wait cqe: %w", err)
			}
		}

		batchQueued := 0
		budget := int(r.AvailableSQEs())
		if budget < 1 {
			budget = 1
		}

		for {
			tag := uint16(cqe.UserData)
			res := cqe.Res
			var iod ioDesc
			seen := false
			if res >= 0 {
				if verifyDescEnabled && snapshot != nil {
					first, second := snapshot(tag), snapshot(tag)
					if first != second {
						ioDebugf("iod unstable q=%d tag=%d first=%x second=%x", qid, tag, first, second)
					}
				}
				iod = loadDesc(tag)
			}
			if debugEnabled {
				ioDebugf("cqe q=%d tag=%d res=%d pendingCommit=%t", qid, tag, res, pendingCommit[tag])
			}
			if !delaySeenEnabled {
				r.SeenCQE(cqe)
				seen = true
			}

			if res == int32(-int32(syscall.ENODEV)) {
				if !seen {
					r.SeenCQE(cqe)
				}
				if err := flushQueued(batchQueued); err != nil {
					return err
				}
				return nil
			}
			if res == int32(-int32(syscall.EBADF)) {
				if !seen {
					r.SeenCQE(cqe)
				}
				if err := flushQueued(batchQueued); err != nil {
					return err
				}
				return nil
			}
			if res == int32(-int32(syscall.EBUSY)) && pendingCommit[tag] {
				if !seen {
					r.SeenCQE(cqe)
				}
			} else {
				if res < 0 {
					if !seen {
						r.SeenCQE(cqe)
					}
					if err := flushQueued(batchQueued); err != nil {
						return err
					}
					return fmt.Errorf("cqe error for tag %d: %d", tag, res)
				}
				pendingCommit[tag] = false

				req := &reqs[tag]
				req.Op = IOOp(iod.OpFlags & 0xff)
				req.Flags = iod.OpFlags >> 8
				req.StartSector = iod.StartSector
				req.NrSectors = iod.NrSectors
				req.Tag = tag
				req.QueueID = qid
				req.dev = d

				result := int32(req.NrSectors) * 512
				if debugEnabled {
					ioDebugf("handle q=%d tag=%d op=%d flags=%d start=%d sectors=%d", qid, tag, req.Op, req.Flags, req.StartSector, req.NrSectors)
				}
				if err := h.HandleIO(req); err != nil {
					if debugEnabled {
						ioDebugf("handle error q=%d tag=%d err=%v", qid, tag, err)
					}
					result = -int32(syscall.EIO)
				}

				if debugEnabled {
					ioDebugf("commit q=%d tag=%d result=%d", qid, tag, result)
				}
				if err := d.submitCommitAndFetch(r, qid, tag, result); err != nil {
					if !seen {
						r.SeenCQE(cqe)
					}
					if err2 := flushQueued(batchQueued); err2 != nil {
						return err2
					}
					return fmt.Errorf("commit tag %d: %w", tag, err)
				}
				pendingCommit[tag] = true
				batchQueued++
			}

			if !seen {
				r.SeenCQE(cqe)
			}
			if batchQueued >= budget {
				break
			}
			next := r.TryCQE()
			if next == nil {
				break
			}
			cqe = next
		}

		if batchQueued == 0 {
			continue
		}
		if batchStatsEnabled {
			batchSubmitCalls++
			batchCommitted += uint64(batchQueued)
			if batchQueued > maxBatchCommitted {
				maxBatchCommitted = batchQueued
			}
		}
		var err error
		nextCQE, err = r.SubmitAndWaitCQE()
		if err != nil {
			if errors.Is(err, syscall.EBADF) && d.isStopped() {
				return nil
			}
			return fmt.Errorf("submit+wait commit batch: %w", err)
		}
	}
}

func (d *Device) runZeroCopyQueueLoop(r queueRing, qid uint16, charFd int32, loadDesc ioDescLoader, h ZeroCopyHandler) error {
	reqs := make([]ZeroCopyRequest, d.info.QueueDepth)
	pendingCommit := make([]bool, d.info.QueueDepth)
	var nextCQE *ioURingCQE
	flushQueued := func(batchQueued int) error {
		if batchQueued == 0 {
			return nil
		}
		if err := r.Submit(); err != nil {
			if errors.Is(err, syscall.EBADF) && d.isStopped() {
				return nil
			}
			return fmt.Errorf("submit commit batch: %w", err)
		}
		return nil
	}

	for {
		select {
		case <-d.stopped:
			return nil
		default:
		}

		cqe := nextCQE
		nextCQE = nil
		if cqe == nil {
			var err error
			cqe, err = r.WaitCQE()
			if err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if errors.Is(err, syscall.EBADF) && d.isStopped() {
					return nil
				}
				return fmt.Errorf("wait cqe: %w", err)
			}
		}

		batchQueued := 0
		budget := int(r.AvailableSQEs())
		if budget < 1 {
			budget = 1
		}

		for {
			tag := uint16(cqe.UserData)
			res := cqe.Res
			var iod ioDesc
			if res >= 0 {
				iod = loadDesc(tag)
			}
			r.SeenCQE(cqe)

			if res == int32(-int32(syscall.ENODEV)) {
				if err := flushQueued(batchQueued); err != nil {
					return err
				}
				return nil
			}
			if res == int32(-int32(syscall.EBADF)) && d.isStopped() {
				if err := flushQueued(batchQueued); err != nil {
					return err
				}
				return nil
			}
			if res == int32(-int32(syscall.EBUSY)) && pendingCommit[tag] {
				// Nothing to queue for this CQE.
			} else {
				if res < 0 {
					if err := flushQueued(batchQueued); err != nil {
						return err
					}
					return fmt.Errorf("cqe error for tag %d: %d", tag, res)
				}
				pendingCommit[tag] = false

				req := &reqs[tag]
				req.Op = IOOp(iod.OpFlags & 0xff)
				req.Flags = iod.OpFlags >> 8
				req.StartSector = iod.StartSector
				req.NrSectors = iod.NrSectors
				req.Tag = tag
				req.QueueID = qid
				req.BufIndex = tag
				req.ring = r
				req.charFd = charFd
				req.dev = d

				result := int32(req.NrSectors) * 512
				if err := h.HandleIO(req); err != nil {
					result = -int32(syscall.EIO)
				}

				if err := d.submitCommitAndFetchAutoBuf(r, qid, tag, result); err != nil {
					if err2 := flushQueued(batchQueued); err2 != nil {
						return err2
					}
					return fmt.Errorf("commit tag %d: %w", tag, err)
				}
				pendingCommit[tag] = true
				batchQueued++
			}

			if batchQueued >= budget {
				break
			}
			next := r.TryCQE()
			if next == nil {
				break
			}
			cqe = next
		}

		if batchQueued == 0 {
			continue
		}
		var err error
		nextCQE, err = r.SubmitAndWaitCQE()
		if err != nil {
			if errors.Is(err, syscall.EBADF) && d.isStopped() {
				return nil
			}
			return fmt.Errorf("submit+wait commit batch: %w", err)
		}
	}
}
