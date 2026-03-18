package ublk

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// ctrlAddDev sends UBLK_U_CMD_ADD_DEV. On success, info is updated with the
// kernel-assigned device ID and other fields.
//
// If ioctl-encoded commands fail with EINVAL (kernel too old), it retries
// with legacy command encoding.
func (d *Device) ctrlAddDev(info *DevInfo) error {
	err := d.tryCtrlAddDev(info)
	if err == nil {
		return nil
	}

	// If the ioctl-encoded command failed with EINVAL, the kernel may not
	// support UBLK_F_CMD_IOCTL_ENCODE. Retry with legacy encoding.
	if errors.Is(err, syscall.EINVAL) {
		info.Flags &^= FlagCmdIoctlEncode
		d.legacyCmds = true
		return d.tryCtrlAddDev(info)
	}
	return err
}

func (d *Device) tryCtrlAddDev(info *DevInfo) error {
	buf := make([]byte, unsafe.Sizeof(DevInfo{}))
	copyDevInfoToBytes(buf, info)

	// Pin the buffer so the GC cannot relocate it while the kernel
	// holds a pointer to it (between Submit and WaitCQE).
	var pinner runtime.Pinner
	pinner.Pin(&buf[0])
	defer pinner.Unpin()

	cmd := ctrlCmd{
		DevID:   ^uint32(0), // -1: let kernel assign
		QueueID: ^uint16(0), // -1: not queue-specific
		Addr:    uint64(uintptr(unsafe.Pointer(&buf[0]))),
		Len:     uint16(len(buf)),
	}

	cmdOp := d.ctrlOp(ublkUCmdAddDev, CmdAddDev)
	sqe := d.ctrlRing.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	prepCtrlCmd(sqe, cmdOp, int32(d.ctrlFile.Fd()), &cmd)
	sqeSetU64(sqe, sqeOffUserData, 1)

	if err := d.ctrlRing.Submit(); err != nil {
		return err
	}

	cqe, err := d.ctrlRing.WaitCQE()
	if err != nil {
		return err
	}
	res := cqe.Res
	d.ctrlRing.SeenCQE(cqe)

	if res < 0 {
		return fmt.Errorf("ADD_DEV failed: %w", errnoFromResult(res))
	}

	copyBytesToDevInfo(info, buf)
	return nil
}

func (d *Device) ctrlDelDev() error {
	return d.simpleCtrlCmd(ublkUCmdDelDev, CmdDelDev)
}

func (d *Device) ctrlStartDev() error {
	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
	}
	cmd.Data[0] = uint64(syscall.Getpid())

	return d.ctrlCmdWithPayload(d.ctrlOp(ublkUCmdStartDev, CmdStartDev), &cmd)
}

func (d *Device) ctrlStopDev() error {
	// Submit STOP_DEV without waiting for the CQE. The kernel will abort
	// all pending IO commands (returning ENODEV), which causes the serve
	// goroutines to exit. Only after they exit can the kernel complete
	// the STOP_DEV. The CQE is drained in ctrlStopDevWait.
	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
	}
	cmdOp := d.ctrlOp(ublkUCmdStopDev, CmdStopDev)
	sqe := d.ctrlRing.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	prepCtrlCmd(sqe, cmdOp, int32(d.ctrlFile.Fd()), &cmd)
	sqeSetU64(sqe, sqeOffUserData, 1)
	return d.ctrlRing.Submit()
}

// ctrlStopDevWait waits for the STOP_DEV CQE. Call after serve goroutines exit.
func (d *Device) ctrlStopDevWait() error {
	cqe, err := d.ctrlRing.WaitCQE()
	if err != nil {
		return err
	}
	d.ctrlRing.SeenCQE(cqe)
	if cqe.Res < 0 {
		return fmt.Errorf("STOP_DEV failed: %w", errnoFromResult(cqe.Res))
	}
	return nil
}

func (d *Device) ctrlSetParams(params *Params) error {
	// Pin the params struct so the GC cannot relocate it while the
	// kernel reads from the pointer embedded in the io_uring SQE.
	var pinner runtime.Pinner
	pinner.Pin(params)
	defer pinner.Unpin()

	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
		Addr:    uint64(uintptr(unsafe.Pointer(params))),
		Len:     uint16(unsafe.Sizeof(Params{})),
	}
	return d.ctrlCmdWithPayload(d.ctrlOp(ublkUCmdSetParams, CmdSetParams), &cmd)
}

func (d *Device) ctrlGetParams(params *Params) error {
	// Pin the params struct so the GC cannot relocate it while the
	// kernel writes into the pointer embedded in the io_uring SQE.
	var pinner runtime.Pinner
	pinner.Pin(params)
	defer pinner.Unpin()

	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
		Addr:    uint64(uintptr(unsafe.Pointer(params))),
		Len:     uint16(unsafe.Sizeof(Params{})),
	}
	return d.ctrlCmdWithPayload(d.ctrlOp(ublkUCmdGetParams, CmdGetParams), &cmd)
}

func (d *Device) simpleCtrlCmd(encoded, legacy uint32) error {
	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
	}
	return d.ctrlCmdWithPayload(d.ctrlOp(encoded, legacy), &cmd)
}

func (d *Device) ctrlCmdWithPayload(cmdOp uint32, cmd *ctrlCmd) error {
	sqe := d.ctrlRing.GetSQE()
	if sqe == nil {
		return fmt.Errorf("no SQE available")
	}
	prepCtrlCmd(sqe, cmdOp, int32(d.ctrlFile.Fd()), cmd)
	sqeSetU64(sqe, sqeOffUserData, 1)

	if err := d.ctrlRing.Submit(); err != nil {
		return err
	}

	cqe, err := d.ctrlRing.WaitCQE()
	if err != nil {
		return err
	}
	res := cqe.Res
	d.ctrlRing.SeenCQE(cqe)

	if res < 0 {
		return fmt.Errorf("ublk ctrl cmd 0x%x failed: %w", cmdOp, errnoFromResult(res))
	}
	return nil
}

// ctrlOp returns the ioctl-encoded command opcode if the kernel supports it,
// or the legacy opcode otherwise.
func (d *Device) ctrlOp(encoded, legacy uint32) uint32 {
	if d.legacyCmds {
		return legacy
	}
	return encoded
}

// ioOp returns the ioctl-encoded IO command opcode if the kernel supports it,
// or the legacy opcode otherwise.
func (d *Device) ioOp(encoded, legacy uint32) uint32 {
	if d.legacyCmds {
		return legacy
	}
	return encoded
}

// errnoFromResult converts a negative ublk result code to a syscall.Errno.
// This allows callers to use errors.Is(err, syscall.EINVAL) etc.
func errnoFromResult(res int32) syscall.Errno {
	return syscall.Errno(-res)
}

func copyDevInfoToBytes(buf []byte, info *DevInfo) {
	src := (*[unsafe.Sizeof(DevInfo{})]byte)(unsafe.Pointer(info))
	copy(buf, src[:])
}

func copyBytesToDevInfo(info *DevInfo, buf []byte) {
	dst := (*[unsafe.Sizeof(DevInfo{})]byte)(unsafe.Pointer(info))
	copy(dst[:], buf)
}
